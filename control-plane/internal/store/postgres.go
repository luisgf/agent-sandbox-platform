package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore implements Store against PostgreSQL via pgxpool.
type PostgresStore struct {
	pool            *pgxpool.Pool
	provisionNodeID string
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{
		pool:            pool,
		provisionNodeID: DefaultLocalNodeID,
	}
}

func (p *PostgresStore) SetProvisionNodeID(id string) {
	p.provisionNodeID = id
}

func (p *PostgresStore) Pool() *pgxpool.Pool { return p.pool }

// EnsureBootstrapNode upserts the stub provision node so FK assigns succeed.
func (p *PostgresStore) EnsureBootstrapNode(ctx context.Context) error {
	_, err := p.RegisterNode(RegisterNodeInput{
		ID:             p.provisionNodeID,
		Name:           p.provisionNodeID,
		Endpoint:       "local://stub",
		CapacityCPU:    0,
		CapacityMemMiB: 0,
	})
	return err
}

func (p *PostgresStore) ensureTenant(ctx context.Context, id string) error {
	name := id
	_, err := p.pool.Exec(ctx, `
		INSERT INTO tenants (id, name) VALUES ($1, $2)
		ON CONFLICT (id) DO NOTHING`, id, name)
	return err
}

func (p *PostgresStore) CreateSandbox(input CreateSandboxInput) (Sandbox, error) {
	if err := prepareCreateSandbox(&input); err != nil {
		return Sandbox{}, err
	}
	ctx := context.Background()
	if err := p.ensureTenant(ctx, input.TenantID); err != nil {
		return Sandbox{}, fmt.Errorf("ensure tenant: %w", err)
	}
	if err := p.EnsureBootstrapNode(ctx); err != nil {
		return Sandbox{}, fmt.Errorf("ensure bootstrap node: %w", err)
	}

	vmm := input.VMMProfile
	if vmm == "" {
		vmm = "cloud-hypervisor"
	}
	id := newID()
	now := time.Now().UTC()
	ownerSub := strings.TrimSpace(input.OwnerSub)
	ownerEmail := strings.TrimSpace(input.OwnerEmail)
	lnOn, lnState := localNetFromInput(input)
	actorSub := strings.TrimSpace(input.ActorSub)
	if actorSub == "" {
		actorSub = ownerSub
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Sandbox{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, `
		INSERT INTO sandboxes (
			id, tenant_id, node_id, state, vmm_profile, image_ref,
			cpu_millis, memory_mib, state_version, node_lease_until, created_at, updated_at,
			owner_sub, owner_email, last_activity_at, stop_reason, workspace_host_path,
			local_net, local_net_state, local_net_attached_at, local_net_grant_expires_at, local_net_grant_hash, local_net_client_public
		) VALUES ($1,$2,NULL,'requested',$3,$4,$5,$6,1,$7,$7,$7,$8,$9,$7,'',$10,$11,$12,NULL,NULL,$13,'')`,
		id, input.TenantID, vmm, input.ImageRef, input.CPUMillis, input.MemoryMiB, now,
		ownerSub, ownerEmail, input.WorkspaceHostPath, lnOn, lnState, "",
	)
	if err != nil {
		return Sandbox{}, fmt.Errorf("insert sandbox: %w", err)
	}

	if err := emitEventTx(ctx, tx, EmitEventInput{
		SandboxID: id,
		TenantID:  input.TenantID,
		EventType: "sandbox.created",
		ToState:   strPtr(string(SandboxRequested)),
		Actor:     "api",
		ActorSub:  actorSub,
		Payload:   json.RawMessage(`{}`),
	}); err != nil {
		return Sandbox{}, err
	}
	if lnOn {
		if err := emitEventTx(ctx, tx, EmitEventInput{
			SandboxID: id,
			TenantID:  input.TenantID,
			EventType: "sandbox.local_net_requested",
			Actor:     "api",
			ActorSub:  actorSub,
			Payload:   mustJSON(map[string]any{"local_net": true, "local_net_state": lnState}),
		}); err != nil {
			return Sandbox{}, err
		}
	}

	if AutoProvisionEnabled() {
		// provision stub: requested → starting → running
		now2 := time.Now().UTC()
		if _, err := tx.Exec(ctx, `
			UPDATE sandboxes SET state='starting', state_version=state_version+1, updated_at=$2
			WHERE id=$1`, id, now2); err != nil {
			return Sandbox{}, err
		}
		if err := emitEventTx(ctx, tx, EmitEventInput{
			SandboxID: id,
			TenantID:  input.TenantID,
			EventType: "sandbox.state_changed",
			FromState: strPtr(string(SandboxRequested)),
			ToState:   strPtr(string(SandboxStarting)),
			Actor:     "provisioner",
			Payload:   json.RawMessage(`{}`),
		}); err != nil {
			return Sandbox{}, err
		}

		now3 := time.Now().UTC()
		nodeID := p.provisionNodeID
		if input.NodeID != "" {
			nodeID = input.NodeID
		}
		if _, err := tx.Exec(ctx, `
			UPDATE sandboxes SET state='running', node_id=$2, state_version=state_version+1, updated_at=$3, last_activity_at=$3
			WHERE id=$1`, id, nodeID, now3); err != nil {
			return Sandbox{}, err
		}
		if err := emitEventTx(ctx, tx, EmitEventInput{
			SandboxID: id,
			TenantID:  input.TenantID,
			EventType: "sandbox.state_changed",
			FromState: strPtr(string(SandboxStarting)),
			ToState:   strPtr(string(SandboxRunning)),
			Actor:     "provisioner",
			Payload:   mustJSON(map[string]string{"node_id": nodeID}),
		}); err != nil {
			return Sandbox{}, err
		}
	} else {
		// Soft-assign: pin node_id when provided or when a ready node exists; leave requested.
		pin := strings.TrimSpace(input.NodeID)
		if pin == "" {
			nodes, err := p.ListNodes()
			if err != nil {
				return Sandbox{}, err
			}
			pin = PickReadyNodeID(nodes)
		}
		if pin != "" {
			nowPin := time.Now().UTC()
			if _, err := tx.Exec(ctx, `
				UPDATE sandboxes SET node_id=$2, updated_at=$3
				WHERE id=$1`, id, pin, nowPin); err != nil {
				return Sandbox{}, err
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Sandbox{}, err
	}
	return p.GetSandbox(id)
}

func (p *PostgresStore) GetSandbox(id string) (Sandbox, error) {
	ctx := context.Background()
	row := p.pool.QueryRow(ctx, `
		SELECT id, tenant_id, node_id, state, vmm_profile, image_ref,
		       cpu_millis, memory_mib, state_version, node_lease_until, created_at, updated_at,
		       owner_sub, owner_email, last_activity_at, stop_reason, workspace_host_path,
			local_net, local_net_state, local_net_attached_at, local_net_grant_expires_at, local_net_grant_hash, local_net_client_public, local_net_node_public
		FROM sandboxes WHERE id=$1`, id)
	sb, err := scanSandbox(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Sandbox{}, ErrNotFound
		}
		return Sandbox{}, err
	}
	return sb, nil
}

func (p *PostgresStore) ListSandboxes(tenantID string) ([]Sandbox, error) {
	ctx := context.Background()
	var rows pgx.Rows
	var err error
	if tenantID == "" {
		rows, err = p.pool.Query(ctx, `
			SELECT id, tenant_id, node_id, state, vmm_profile, image_ref,
			       cpu_millis, memory_mib, state_version, node_lease_until, created_at, updated_at,
			       owner_sub, owner_email, last_activity_at, stop_reason, workspace_host_path,
			local_net, local_net_state, local_net_attached_at, local_net_grant_expires_at, local_net_grant_hash, local_net_client_public, local_net_node_public
			FROM sandboxes ORDER BY created_at DESC`)
	} else {
		rows, err = p.pool.Query(ctx, `
			SELECT id, tenant_id, node_id, state, vmm_profile, image_ref,
			       cpu_millis, memory_mib, state_version, node_lease_until, created_at, updated_at,
			       owner_sub, owner_email, last_activity_at, stop_reason, workspace_host_path,
			local_net, local_net_state, local_net_attached_at, local_net_grant_expires_at, local_net_grant_hash, local_net_client_public, local_net_node_public
			FROM sandboxes WHERE tenant_id=$1 ORDER BY created_at DESC`, tenantID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Sandbox, 0)
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

func (p *PostgresStore) AssignSandbox(id, nodeID string, state SandboxState) (Sandbox, error) {
	if nodeID == "" {
		return Sandbox{}, fmt.Errorf("%w: node_id required", ErrInvalidInput)
	}
	ctx := context.Background()
	sb, err := p.GetSandbox(id)
	if err != nil {
		return Sandbox{}, err
	}
	from := string(sb.State)
	now := time.Now().UTC()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Sandbox{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE sandboxes SET node_id=$2, state=$3, state_version=state_version+1, updated_at=$4
		WHERE id=$1`, id, nodeID, string(state), now)
	if err != nil {
		return Sandbox{}, err
	}
	if tag.RowsAffected() == 0 {
		return Sandbox{}, ErrNotFound
	}
	if err := emitEventTx(ctx, tx, EmitEventInput{
		SandboxID: id,
		TenantID:  sb.TenantID,
		EventType: "sandbox.state_changed",
		FromState: &from,
		ToState:   strPtr(string(state)),
		Actor:     "api",
		Payload:   mustJSON(map[string]string{"node_id": nodeID}),
	}); err != nil {
		return Sandbox{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Sandbox{}, err
	}
	return p.GetSandbox(id)
}

func (p *PostgresStore) ClaimSandbox(id, nodeID string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(nodeID) == "" {
		return Sandbox{}, fmt.Errorf("%w: id and node_id required", ErrInvalidInput)
	}
	ctx := context.Background()
	sb, err := p.GetSandbox(id)
	if err != nil {
		return Sandbox{}, err
	}
	now := time.Now().UTC()
	if sb.State != SandboxRequested {
		// Allow reclaim of stuck starting/running with expired lease by resetting first.
		if (sb.State == SandboxStarting || sb.State == SandboxRunning) &&
			sb.NodeID != nil && *sb.NodeID != "" && *sb.NodeID != nodeID &&
			leaseExpired(sb.NodeLeaseUntil, now) {
			_, _ = p.ReclaimExpiredLeases(now, true)
			sb, err = p.GetSandbox(id)
			if err != nil {
				return Sandbox{}, err
			}
			if sb.State != SandboxRequested {
				return Sandbox{}, fmt.Errorf("%w: sandbox state %s not claimable", ErrConflict, sb.State)
			}
		} else {
			return Sandbox{}, fmt.Errorf("%w: sandbox state %s not claimable", ErrConflict, sb.State)
		}
	}
	if sb.NodeID != nil && *sb.NodeID != "" && *sb.NodeID != nodeID && !leaseExpired(sb.NodeLeaseUntil, now) {
		return Sandbox{}, fmt.Errorf("%w: already assigned to %s", ErrConflict, *sb.NodeID)
	}
	from := string(sb.State)
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Sandbox{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	until := leaseUntil(now)
	tag, err := tx.Exec(ctx, `
		UPDATE sandboxes
		SET node_id=$2, state='starting', state_version=state_version+1, updated_at=$3, node_lease_until=$4
		WHERE id=$1 AND state='requested'
		  AND (node_id IS NULL OR node_id = '' OR node_id = $2
		       OR node_lease_until IS NULL OR node_lease_until <= $3)`,
		id, nodeID, now, until)
	if err != nil {
		return Sandbox{}, err
	}
	if tag.RowsAffected() == 0 {
		return Sandbox{}, fmt.Errorf("%w: claim lost race", ErrConflict)
	}
	if err := emitEventTx(ctx, tx, EmitEventInput{
		SandboxID: id,
		TenantID:  sb.TenantID,
		EventType: "sandbox.claimed",
		FromState: &from,
		ToState:   strPtr(string(SandboxStarting)),
		Actor:     "node-agent",
		Payload:   mustJSON(map[string]string{"node_id": nodeID}),
	}); err != nil {
		return Sandbox{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Sandbox{}, err
	}
	return p.GetSandbox(id)
}

func (p *PostgresStore) ListNodeWork(nodeID string) ([]Sandbox, error) {
	if strings.TrimSpace(nodeID) == "" {
		return nil, fmt.Errorf("%w: node_id required", ErrInvalidInput)
	}
	ctx := context.Background()
	rows, err := p.pool.Query(ctx, `
		SELECT id, tenant_id, node_id, state, vmm_profile, image_ref,
		       cpu_millis, memory_mib, state_version, node_lease_until, created_at, updated_at,
		       owner_sub, owner_email, last_activity_at, stop_reason, workspace_host_path,
			local_net, local_net_state, local_net_attached_at, local_net_grant_expires_at, local_net_grant_hash, local_net_client_public, local_net_node_public
		FROM sandboxes
		WHERE (node_id = $1 AND state IN ('requested','starting','stopping'))
		   OR (node_id = $1 AND local_net = true AND state = 'running')
		   OR ((node_id IS NULL OR node_id = '') AND state = 'requested')
		ORDER BY created_at ASC`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Sandbox, 0)
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

func (p *PostgresStore) UpdateSandboxStatus(id string, state SandboxState, detail string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	if !ValidAgentStatus(state) {
		return Sandbox{}, fmt.Errorf("%w: invalid status %s", ErrInvalidInput, state)
	}
	ctx := context.Background()
	sb, err := p.GetSandbox(id)
	if err != nil {
		return Sandbox{}, err
	}
	from := string(sb.State)
	now := time.Now().UTC()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Sandbox{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	until := interface{}(nil)
	if state == SandboxRunning || state == SandboxStarting {
		u := leaseUntil(now)
		until = u
	}
	tag, err := tx.Exec(ctx, `
		UPDATE sandboxes SET state=$2, state_version=state_version+1, updated_at=$3, node_lease_until=$4,
		    last_activity_at=CASE WHEN $2='running' THEN $3 ELSE last_activity_at END,
		    stop_reason=CASE WHEN $2='running' THEN '' ELSE stop_reason END,
		    local_net_state=CASE
		      WHEN $2 IN ('failed','stopped','stopping') AND local_net THEN 'withdrawn'
		      WHEN $2 IN ('failed','stopped','stopping') AND NOT local_net THEN 'off'
		      ELSE local_net_state END,
		    local_net_client_public=CASE WHEN $2 IN ('failed','stopped','stopping') THEN '' ELSE local_net_client_public END,
		    local_net_grant_hash=CASE WHEN $2 IN ('failed','stopped','stopping') THEN '' ELSE local_net_grant_hash END,
		    local_net_grant_expires_at=CASE WHEN $2 IN ('failed','stopped','stopping') THEN NULL ELSE local_net_grant_expires_at END
		WHERE id=$1`, id, string(state), now, until)
	if err != nil {
		return Sandbox{}, err
	}
	if tag.RowsAffected() == 0 {
		return Sandbox{}, ErrNotFound
	}
	payload := map[string]string{}
	if detail != "" {
		payload["detail"] = detail
	}
	if err := emitEventTx(ctx, tx, EmitEventInput{
		SandboxID: id,
		TenantID:  sb.TenantID,
		EventType: "sandbox.state_changed",
		FromState: &from,
		ToState:   strPtr(string(state)),
		Actor:     "node-agent",
		Payload:   mustJSON(payload),
	}); err != nil {
		return Sandbox{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Sandbox{}, err
	}
	return p.GetSandbox(id)
}

func (p *PostgresStore) RenewSandboxLease(id, nodeID string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(nodeID) == "" {
		return Sandbox{}, fmt.Errorf("%w: id and node_id required", ErrInvalidInput)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	until := leaseUntil(now)
	tag, err := p.pool.Exec(ctx, `
		UPDATE sandboxes SET node_lease_until=$3, updated_at=$4
		WHERE id=$1 AND node_id=$2`, id, nodeID, until, now)
	if err != nil {
		return Sandbox{}, err
	}
	if tag.RowsAffected() == 0 {
		sb, gerr := p.GetSandbox(id)
		if gerr != nil {
			return Sandbox{}, gerr
		}
		if sb.NodeID == nil || *sb.NodeID != nodeID {
			return Sandbox{}, fmt.Errorf("%w: not owned by %s", ErrConflict, nodeID)
		}
		return Sandbox{}, ErrNotFound
	}
	return p.GetSandbox(id)
}

func (p *PostgresStore) ReclaimExpiredLeases(now time.Time, reRequest bool) ([]Sandbox, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	ctx := context.Background()
	target := string(SandboxFailed)
	if reRequest {
		target = string(SandboxRequested)
	}
	rows, err := p.pool.Query(ctx, `
		SELECT id, tenant_id, node_id, state, vmm_profile, image_ref,
		       cpu_millis, memory_mib, state_version, node_lease_until, created_at, updated_at,
		       owner_sub, owner_email, last_activity_at, stop_reason, workspace_host_path,
			local_net, local_net_state, local_net_attached_at, local_net_grant_expires_at, local_net_grant_hash, local_net_client_public, local_net_node_public
		FROM sandboxes
		WHERE state IN ('starting','running')
		  AND (node_lease_until IS NULL OR node_lease_until <= $1)`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := make([]Sandbox, 0)
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, sb)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Sandbox, 0, len(candidates))
	for _, sb := range candidates {
		from := string(sb.State)
		tx, err := p.pool.Begin(ctx)
		if err != nil {
			return out, err
		}
		var tag pgconn.CommandTag
		if reRequest {
			tag, err = tx.Exec(ctx, `
				UPDATE sandboxes
				SET state=$2, node_id=NULL, node_lease_until=NULL,
				    state_version=state_version+1, updated_at=$3
				WHERE id=$1 AND state IN ('starting','running')
				  AND (node_lease_until IS NULL OR node_lease_until <= $3)`,
				sb.ID, target, now)
		} else {
			tag, err = tx.Exec(ctx, `
				UPDATE sandboxes
				SET state=$2, node_lease_until=NULL,
				    state_version=state_version+1, updated_at=$3
				WHERE id=$1 AND state IN ('starting','running')
				  AND (node_lease_until IS NULL OR node_lease_until <= $3)`,
				sb.ID, target, now)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return out, err
		}
		if tag.RowsAffected() == 0 {
			_ = tx.Rollback(ctx)
			continue
		}
		if err := emitEventTx(ctx, tx, EmitEventInput{
			SandboxID: sb.ID,
			TenantID:  sb.TenantID,
			EventType: "sandbox.lease_expired",
			FromState: &from,
			ToState:   strPtr(target),
			Actor:     "lease",
			Payload:   mustJSON(map[string]any{"re_request": reRequest}),
		}); err != nil {
			_ = tx.Rollback(ctx)
			return out, err
		}
		if err := tx.Commit(ctx); err != nil {
			return out, err
		}
		got, err := p.GetSandbox(sb.ID)
		if err != nil {
			return out, err
		}
		out = append(out, got)
	}
	return out, nil
}

func (p *PostgresStore) MarkSandboxStopping(id, actorSub string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	actorSub = strings.TrimSpace(actorSub)
	ctx := context.Background()
	sb, err := p.GetSandbox(id)
	if err != nil {
		return Sandbox{}, err
	}
	if sb.State == SandboxStopped || sb.State == SandboxStopping {
		return sb, nil
	}
	from := string(sb.State)
	now := time.Now().UTC()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Sandbox{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	target := SandboxStopping
	payload := json.RawMessage(`{"reason":"destroy"}`)
	if sb.State == SandboxRequested && (sb.NodeID == nil || *sb.NodeID == "") {
		target = SandboxStopped
		payload = json.RawMessage(`{"reason":"destroy_unassigned"}`)
	} else if sb.State == SandboxFailed {
		target = SandboxStopped
		payload = json.RawMessage(`{"reason":"destroy_failed"}`)
	} else if !IsActiveLifecycle(sb.State) {
		return Sandbox{}, fmt.Errorf("%w: cannot destroy from state %s", ErrConflict, sb.State)
	}

	tag, err := tx.Exec(ctx, `
		UPDATE sandboxes SET state=$2, state_version=state_version+1, updated_at=$3,
		    local_net_state=CASE WHEN local_net THEN 'withdrawn' ELSE 'off' END,
		    local_net_client_public='',
		    local_net_grant_hash='',
		    local_net_grant_expires_at=NULL
		WHERE id=$1`, id, string(target), now)
	if err != nil {
		return Sandbox{}, err
	}
	if tag.RowsAffected() == 0 {
		return Sandbox{}, ErrNotFound
	}
	if err := emitEventTx(ctx, tx, EmitEventInput{
		SandboxID: id,
		TenantID:  sb.TenantID,
		EventType: "sandbox.state_changed",
		FromState: &from,
		ToState:   strPtr(string(target)),
		Actor:     "api",
		ActorSub:  actorSub,
		Payload:   payload,
	}); err != nil {
		return Sandbox{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Sandbox{}, err
	}
	return p.GetSandbox(id)
}

func (p *PostgresStore) TouchSandboxActivity(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	tag, err := p.pool.Exec(ctx, `
		UPDATE sandboxes SET last_activity_at=$2 WHERE id=$1`, id, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *PostgresStore) StopIdleSandboxes(now time.Time, idleFor time.Duration) ([]Sandbox, error) {
	if idleFor <= 0 {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cutoff := now.Add(-idleFor)
	ctx := context.Background()
	rows, err := p.pool.Query(ctx, `
		SELECT id, tenant_id, node_id, state, vmm_profile, image_ref,
		       cpu_millis, memory_mib, state_version, node_lease_until, created_at, updated_at,
		       owner_sub, owner_email, last_activity_at, stop_reason, workspace_host_path,
			local_net, local_net_state, local_net_attached_at, local_net_grant_expires_at, local_net_grant_hash, local_net_client_public, local_net_node_public
		FROM sandboxes
		WHERE state IN ('requested','scheduled','starting','running','paused')
		  AND last_activity_at <= $1`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := make([]Sandbox, 0)
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, sb)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Sandbox, 0, len(candidates))
	for _, sb := range candidates {
		if DecideIdle(sb.LastActivityAt, now, idleFor) != IdleExpired {
			continue
		}
		target := SandboxStopping
		if sb.State == SandboxRequested && (sb.NodeID == nil || *sb.NodeID == "") {
			target = SandboxStopped
		}
		from := string(sb.State)
		tx, err := p.pool.Begin(ctx)
		if err != nil {
			return out, err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE sandboxes
			SET state=$2, stop_reason=$3, node_lease_until=NULL,
			    state_version=state_version+1, updated_at=$4,
			    local_net_state=CASE WHEN local_net THEN 'withdrawn' ELSE 'off' END,
			    local_net_client_public='',
			    local_net_grant_hash='',
			    local_net_grant_expires_at=NULL
			WHERE id=$1 AND state=$5 AND last_activity_at <= $6`,
			sb.ID, string(target), StopReasonIdle, now, from, cutoff)
		if err != nil {
			_ = tx.Rollback(ctx)
			return out, err
		}
		if tag.RowsAffected() == 0 {
			_ = tx.Rollback(ctx)
			continue
		}
		if err := emitEventTx(ctx, tx, EmitEventInput{
			SandboxID: sb.ID,
			TenantID:  sb.TenantID,
			EventType: "sandbox.idle_reaped",
			FromState: &from,
			ToState:   strPtr(string(target)),
			Actor:     "idle-reaper",
			Payload:   mustJSON(map[string]any{"reason": StopReasonIdle, "idle_for": idleFor.String()}),
		}); err != nil {
			_ = tx.Rollback(ctx)
			return out, err
		}
		if err := tx.Commit(ctx); err != nil {
			return out, err
		}
		got, err := p.GetSandbox(sb.ID)
		if err != nil {
			return out, err
		}
		out = append(out, got)
	}
	return out, nil
}

func (p *PostgresStore) RegisterNode(input RegisterNodeInput) (Node, error) {
	if strings.TrimSpace(input.ID) == "" && strings.TrimSpace(input.Name) == "" {
		return Node{}, fmt.Errorf("%w: id or name required", ErrInvalidInput)
	}
	ctx := context.Background()
	id := input.ID
	if id == "" {
		id = newID()
	}
	name := input.Name
	if name == "" {
		name = id
	}
	profiles := input.VMMProfiles
	if len(profiles) == 0 {
		profiles = []string{"cloud-hypervisor"}
	}
	agentEndpoint := input.AgentEndpoint
	if agentEndpoint == "" {
		agentEndpoint = input.Endpoint
	}
	now := time.Now().UTC()

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Node{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var createdAt time.Time
	err = tx.QueryRow(ctx, `SELECT created_at FROM nodes WHERE id=$1`, id).Scan(&createdAt)
	isNew := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !isNew {
		return Node{}, err
	}

	if isNew {
		createdAt = now
		_, err = tx.Exec(ctx, `
			INSERT INTO nodes (
				id, name, endpoint, agent_endpoint, state, vmm_profiles,
				capacity_cpu, capacity_mem_mib, fence_endpoint, fence_token,
				last_seen_at, created_at, updated_at
			) VALUES ($1,$2,$3,$4,'ready',$5,$6,$7,$8,$9,$10,$10,$10)`,
			id, name, input.Endpoint, agentEndpoint, profiles, input.CapacityCPU, input.CapacityMemMiB,
			strings.TrimSpace(input.FenceEndpoint), strings.TrimSpace(input.FenceToken), now,
		)
	} else {
		_, err = tx.Exec(ctx, `
			UPDATE nodes SET
				name=$2, endpoint=$3,
				agent_endpoint=CASE WHEN $4 = '' THEN agent_endpoint ELSE $4 END,
				state='ready', vmm_profiles=$5,
				capacity_cpu=$6, capacity_mem_mib=$7,
				fence_endpoint=CASE WHEN $8 = '' THEN fence_endpoint ELSE $8 END,
				fence_token=CASE WHEN $9 = '' THEN fence_token ELSE $9 END,
				last_seen_at=$10, updated_at=$10
			WHERE id=$1`,
			id, name, input.Endpoint, agentEndpoint, profiles, input.CapacityCPU, input.CapacityMemMiB,
			strings.TrimSpace(input.FenceEndpoint), strings.TrimSpace(input.FenceToken), now,
		)
	}
	if err != nil {
		return Node{}, err
	}

	eventType := "node.registered"
	if !isNew {
		eventType = "node.updated"
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO node_events (node_id, event_type, actor, payload)
		VALUES ($1,$2,'node-agent',$3)`,
		id, eventType, mustJSON(map[string]any{
			"endpoint": input.Endpoint, "agent_endpoint": agentEndpoint, "name": name,
		}),
	)
	if err != nil {
		return Node{}, fmt.Errorf("node event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Node{}, err
	}
	return p.GetNode(id)
}

func (p *PostgresStore) EnrollNode(input EnrollNodeInput, cert CertMeta) (Node, error) {
	fp := strings.TrimSpace(cert.Fingerprint)
	serial := strings.TrimSpace(cert.Serial)
	if fp == "" {
		return Node{}, fmt.Errorf("%w: cert_fingerprint required", ErrInvalidInput)
	}
	if strings.TrimSpace(input.ID) == "" && strings.TrimSpace(input.Name) == "" {
		return Node{}, fmt.Errorf("%w: id or name required", ErrInvalidInput)
	}
	ctx := context.Background()
	id := input.ID
	if id == "" {
		id = newID()
	}
	name := input.Name
	if name == "" {
		name = id
	}
	profiles := input.VMMProfiles
	if len(profiles) == 0 {
		profiles = []string{"cloud-hypervisor"}
	}
	agentEndpoint := input.AgentEndpoint
	if agentEndpoint == "" {
		agentEndpoint = input.Endpoint
	}
	now := time.Now().UTC()

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Node{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM nodes WHERE id=$1)`, id).Scan(&exists); err != nil {
		return Node{}, err
	}
	if exists {
		var oldFP string
		_ = tx.QueryRow(ctx, `SELECT cert_fingerprint FROM nodes WHERE id=$1`, id).Scan(&oldFP)
		if oldFP != "" && oldFP != fp {
			_, _ = tx.Exec(ctx, `
				INSERT INTO node_cert_revocations (fingerprint, node_id, serial, reason, revoked_at)
				VALUES ($1,$2,'','rotated-on-enroll',$3)
				ON CONFLICT (fingerprint) DO NOTHING`, oldFP, id, now)
		}
		_, err = tx.Exec(ctx, `
			UPDATE nodes SET
				name=$2, endpoint=$3, agent_endpoint=$4, state='ready', vmm_profiles=$5,
				capacity_cpu=$6, capacity_mem_mib=$7, cert_fingerprint=$8, cert_serial=$9,
				enrolled_at=$10, last_seen_at=$10, updated_at=$10, revoked_at=NULL
			WHERE id=$1`,
			id, name, input.Endpoint, agentEndpoint, profiles,
			input.CapacityCPU, input.CapacityMemMiB, fp, serial, now,
		)
	} else {
		_, err = tx.Exec(ctx, `
			INSERT INTO nodes (
				id, name, endpoint, agent_endpoint, state, vmm_profiles,
				capacity_cpu, capacity_mem_mib, cert_fingerprint, cert_serial, enrolled_at,
				last_seen_at, created_at, updated_at
			) VALUES ($1,$2,$3,$4,'ready',$5,$6,$7,$8,$9,$10,$10,$10,$10)`,
			id, name, input.Endpoint, agentEndpoint, profiles,
			input.CapacityCPU, input.CapacityMemMiB, fp, serial, now,
		)
	}
	if err != nil {
		return Node{}, err
	}
	_, _ = tx.Exec(ctx, `DELETE FROM node_cert_revocations WHERE fingerprint=$1`, fp)
	_, err = tx.Exec(ctx, `
		INSERT INTO node_events (node_id, event_type, actor, payload)
		VALUES ($1,'node.enrolled','enrollment',$2)`,
		id, mustJSON(map[string]any{
			"cert_fingerprint": fp,
			"cert_serial":      serial,
			"agent_endpoint":   agentEndpoint,
		}),
	)
	if err != nil {
		return Node{}, fmt.Errorf("node event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Node{}, err
	}
	return p.GetNode(id)
}

func (p *PostgresStore) RotateNodeCert(nodeID string, cert CertMeta) (Node, error) {
	fp := strings.TrimSpace(cert.Fingerprint)
	serial := strings.TrimSpace(cert.Serial)
	if strings.TrimSpace(nodeID) == "" {
		return Node{}, fmt.Errorf("%w: node id required", ErrInvalidInput)
	}
	if fp == "" {
		return Node{}, fmt.Errorf("%w: cert_fingerprint required", ErrInvalidInput)
	}
	ctx := context.Background()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Node{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var oldFP, oldSerial string
	var revoked *time.Time
	err = tx.QueryRow(ctx, `
		SELECT cert_fingerprint, cert_serial, revoked_at FROM nodes WHERE id=$1`, nodeID).
		Scan(&oldFP, &oldSerial, &revoked)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Node{}, ErrNotFound
		}
		return Node{}, err
	}
	if revoked != nil {
		return Node{}, fmt.Errorf("%w: node is revoked; re-enroll required", ErrConflict)
	}
	now := time.Now().UTC()
	if oldFP != "" && oldFP != fp {
		_, err = tx.Exec(ctx, `
			INSERT INTO node_cert_revocations (fingerprint, node_id, serial, reason, revoked_at)
			VALUES ($1,$2,$3,'rotated',$4)
			ON CONFLICT (fingerprint) DO UPDATE SET revoked_at=EXCLUDED.revoked_at, reason=EXCLUDED.reason`,
			oldFP, nodeID, oldSerial, now)
		if err != nil {
			return Node{}, err
		}
	}
	_, err = tx.Exec(ctx, `
		UPDATE nodes SET cert_fingerprint=$2, cert_serial=$3, updated_at=$4
		WHERE id=$1`, nodeID, fp, serial, now)
	if err != nil {
		return Node{}, err
	}
	_, _ = tx.Exec(ctx, `DELETE FROM node_cert_revocations WHERE fingerprint=$1`, fp)
	_, err = tx.Exec(ctx, `
		INSERT INTO node_events (node_id, event_type, actor, payload)
		VALUES ($1,'node.cert_rotated','api',$2)`,
		nodeID, mustJSON(map[string]any{
			"cert_fingerprint": fp,
			"cert_serial":      serial,
			"old_fingerprint":  oldFP,
		}),
	)
	if err != nil {
		return Node{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Node{}, err
	}
	return p.GetNode(nodeID)
}

func (p *PostgresStore) RevokeNode(nodeID string) (Node, error) {
	if strings.TrimSpace(nodeID) == "" {
		return Node{}, fmt.Errorf("%w: node id required", ErrInvalidInput)
	}
	ctx := context.Background()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Node{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var fp, serial string
	err = tx.QueryRow(ctx, `
		SELECT cert_fingerprint, cert_serial FROM nodes WHERE id=$1`, nodeID).Scan(&fp, &serial)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Node{}, ErrNotFound
		}
		return Node{}, err
	}
	now := time.Now().UTC()
	_, err = tx.Exec(ctx, `
		UPDATE nodes SET revoked_at=$2, state='offline', updated_at=$2 WHERE id=$1`, nodeID, now)
	if err != nil {
		return Node{}, err
	}
	if fp != "" {
		_, err = tx.Exec(ctx, `
			INSERT INTO node_cert_revocations (fingerprint, node_id, serial, reason, revoked_at)
			VALUES ($1,$2,$3,'revoked',$4)
			ON CONFLICT (fingerprint) DO UPDATE SET revoked_at=EXCLUDED.revoked_at, reason=EXCLUDED.reason`,
			fp, nodeID, serial, now)
		if err != nil {
			return Node{}, err
		}
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO node_events (node_id, event_type, actor, payload)
		VALUES ($1,'node.revoked','api',$2)`,
		nodeID, mustJSON(map[string]any{"cert_fingerprint": fp, "cert_serial": serial}),
	)
	if err != nil {
		return Node{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Node{}, err
	}
	return p.GetNode(nodeID)
}

func (p *PostgresStore) IsCertRevoked(fingerprint string) (bool, error) {
	fp := strings.TrimSpace(fingerprint)
	if fp == "" {
		return false, nil
	}
	ctx := context.Background()
	var n int
	err := p.pool.QueryRow(ctx, `
		SELECT 1 FROM node_cert_revocations WHERE fingerprint=$1
		UNION ALL
		SELECT 1 FROM nodes WHERE cert_fingerprint=$1 AND revoked_at IS NOT NULL
		LIMIT 1`, fp).Scan(&n)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (p *PostgresStore) HeartbeatNode(id string) (Node, error) {
	if strings.TrimSpace(id) == "" {
		return Node{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	tag, err := p.pool.Exec(ctx, `
		UPDATE nodes SET last_seen_at=$2, updated_at=$2, state='ready'
		WHERE id=$1`, id, now)
	if err != nil {
		return Node{}, err
	}
	if tag.RowsAffected() == 0 {
		return Node{}, ErrNotFound
	}
	return p.GetNode(id)
}

func (p *PostgresStore) ListNodes() ([]Node, error) {
	ctx := context.Background()
	rows, err := p.pool.Query(ctx, `
		SELECT id, name, endpoint, agent_endpoint, state, vmm_profiles,
		       capacity_cpu, capacity_mem_mib, cert_fingerprint, cert_serial, fence_token, fence_endpoint, enrolled_at,
		       revoked_at, last_seen_at, created_at, updated_at
		FROM nodes ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Node, 0)
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (p *PostgresStore) GetNode(id string) (Node, error) {
	ctx := context.Background()
	row := p.pool.QueryRow(ctx, `
		SELECT id, name, endpoint, agent_endpoint, state, vmm_profiles,
		       capacity_cpu, capacity_mem_mib, cert_fingerprint, cert_serial, fence_token, fence_endpoint, enrolled_at,
		       revoked_at, last_seen_at, created_at, updated_at
		FROM nodes WHERE id=$1`, id)
	n, err := scanNode(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Node{}, ErrNotFound
		}
		return Node{}, err
	}
	return n, nil
}

func (p *PostgresStore) EmitEvent(input EmitEventInput) error {
	ctx := context.Background()
	return emitEventTx(ctx, p.pool, EmitEventInput(input))
}

func (p *PostgresStore) ListEvents(sandboxID string) ([]SandboxEvent, error) {
	ctx := context.Background()
	rows, err := p.pool.Query(ctx, `
		SELECT id, sandbox_id, tenant_id, event_type, from_state, to_state,
		       actor, actor_sub, request_id, payload, created_at
		FROM sandbox_events WHERE sandbox_id=$1 ORDER BY id ASC`, sandboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SandboxEvent, 0)
	for rows.Next() {
		var e SandboxEvent
		var payload []byte
		if err := rows.Scan(
			&e.ID, &e.SandboxID, &e.TenantID, &e.EventType,
			&e.FromState, &e.ToState, &e.Actor, &e.ActorSub, &e.RequestID,
			&payload, &e.CreatedAt,
		); err != nil {
			return nil, err
		}
		e.Payload = append(json.RawMessage(nil), payload...)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (p *PostgresStore) LookupAPIKeyByHash(secretHash string) (ApiKey, error) {
	ctx := context.Background()
	row := p.pool.QueryRow(ctx, `
		SELECT id, tenant_id, name, key_prefix, secret_hash,
		       last_used_at, expires_at, revoked_at, created_at
		FROM api_keys
		WHERE secret_hash=$1 AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > now())`, secretHash)
	var k ApiKey
	err := row.Scan(
		&k.ID, &k.TenantID, &k.Name, &k.KeyPrefix, &k.SecretHash,
		&k.LastUsedAt, &k.ExpiresAt, &k.RevokedAt, &k.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ApiKey{}, ErrNotFound
		}
		return ApiKey{}, err
	}
	return k, nil
}

func (p *PostgresStore) EnsureAPIKey(tenantID, name, keyPrefix, secretHash string) (ApiKey, error) {
	if tenantID == "" || name == "" || keyPrefix == "" || secretHash == "" {
		return ApiKey{}, fmt.Errorf("%w: tenant_id, name, key_prefix, secret_hash required", ErrInvalidInput)
	}
	ctx := context.Background()
	if err := p.ensureTenant(ctx, tenantID); err != nil {
		return ApiKey{}, err
	}
	// Return existing by tenant+name if present.
	var k ApiKey
	err := p.pool.QueryRow(ctx, `
		SELECT id, tenant_id, name, key_prefix, secret_hash,
		       last_used_at, expires_at, revoked_at, created_at
		FROM api_keys WHERE tenant_id=$1 AND name=$2`, tenantID, name,
	).Scan(
		&k.ID, &k.TenantID, &k.Name, &k.KeyPrefix, &k.SecretHash,
		&k.LastUsedAt, &k.ExpiresAt, &k.RevokedAt, &k.CreatedAt,
	)
	if err == nil {
		if k.SecretHash != secretHash || k.KeyPrefix != keyPrefix {
			_, uerr := p.pool.Exec(ctx, `
				UPDATE api_keys SET secret_hash=$2, key_prefix=$3, revoked_at=NULL
				WHERE id=$1`, k.ID, secretHash, keyPrefix)
			if uerr != nil {
				return ApiKey{}, uerr
			}
			k.SecretHash = secretHash
			k.KeyPrefix = keyPrefix
			k.RevokedAt = nil
		}
		return k, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ApiKey{}, err
	}
	id := newID()
	now := time.Now().UTC()
	_, err = p.pool.Exec(ctx, `
		INSERT INTO api_keys (id, tenant_id, name, key_prefix, secret_hash, created_at)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		id, tenantID, name, keyPrefix, secretHash, now,
	)
	if err != nil {
		return ApiKey{}, err
	}
	return ApiKey{
		ID: id, TenantID: tenantID, Name: name,
		KeyPrefix: keyPrefix, SecretHash: secretHash, CreatedAt: now,
	}, nil
}

func (p *PostgresStore) CountAPIKeys() (int64, error) {
	ctx := context.Background()
	var n int64
	err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM api_keys WHERE revoked_at IS NULL`).Scan(&n)
	return n, err
}

func (p *PostgresStore) TouchAPIKey(id string) error {
	ctx := context.Background()
	_, err := p.pool.Exec(ctx,
		`UPDATE api_keys SET last_used_at=now() WHERE id=$1`, id)
	return err
}

type scannable interface {
	Scan(dest ...any) error
}

func scanSandbox(row scannable) (Sandbox, error) {
	var sb Sandbox
	var state string
	err := row.Scan(
		&sb.ID, &sb.TenantID, &sb.NodeID, &state, &sb.VMMProfile, &sb.ImageRef,
		&sb.CPUMillis, &sb.MemoryMiB, &sb.StateVersion, &sb.NodeLeaseUntil,
		&sb.CreatedAt, &sb.UpdatedAt, &sb.OwnerSub, &sb.OwnerEmail, &sb.LastActivityAt, &sb.StopReason,
		&sb.WorkspaceHostPath,
		&sb.LocalNet, &sb.LocalNetState, &sb.LocalNetAttachedAt, &sb.LocalNetGrantExpiresAt,
		&sb.LocalNetGrantHash, &sb.LocalNetClientPublic, &sb.LocalNetNodePublic,
	)
	if err != nil {
		return Sandbox{}, err
	}
	sb.State = SandboxState(state)
	return sb, nil
}

func scanNode(row scannable) (Node, error) {
	var n Node
	err := row.Scan(
		&n.ID, &n.Name, &n.Endpoint, &n.AgentEndpoint, &n.State, &n.VMMProfiles,
		&n.CapacityCPU, &n.CapacityMemMiB, &n.CertFingerprint, &n.CertSerial, &n.FenceToken, &n.FenceEndpoint, &n.EnrolledAt,
		&n.RevokedAt, &n.LastSeenAt, &n.CreatedAt, &n.UpdatedAt,
	)
	if err != nil {
		return Node{}, err
	}
	if n.VMMProfiles == nil {
		n.VMMProfiles = []string{}
	}
	return n, nil
}

func emitEventTx(ctx context.Context, q interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}, input EmitEventInput) error {
	if strings.TrimSpace(input.SandboxID) == "" || strings.TrimSpace(input.EventType) == "" {
		return fmt.Errorf("%w: sandbox_id and event_type required", ErrInvalidInput)
	}
	actor := input.Actor
	if actor == "" {
		actor = "system"
	}
	payload := input.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	_, err := q.Exec(ctx, `
		INSERT INTO sandbox_events (
			sandbox_id, tenant_id, event_type, from_state, to_state,
			actor, actor_sub, request_id, payload
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		input.SandboxID, input.TenantID, input.EventType,
		input.FromState, input.ToState, actor, strings.TrimSpace(input.ActorSub),
		input.RequestID, []byte(payload),
	)
	return err
}

func (p *PostgresStore) ListEgressRules(tenantID string) ([]EgressRule, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, fmt.Errorf("%w: tenant_id required", ErrInvalidInput)
	}
	ctx := context.Background()
	rows, err := p.pool.Query(ctx, `
		SELECT id, tenant_id, host_pattern, port, enabled
		FROM tenant_egress_rules WHERE tenant_id=$1
		ORDER BY host_pattern, port NULLS FIRST`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]EgressRule, 0)
	for rows.Next() {
		var r EgressRule
		if err := rows.Scan(&r.ID, &r.TenantID, &r.HostPattern, &r.Port, &r.Enabled); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *PostgresStore) PutEgressRules(tenantID string, rules []EgressRule) ([]EgressRule, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, fmt.Errorf("%w: tenant_id required", ErrInvalidInput)
	}
	ctx := context.Background()
	if err := p.ensureTenant(ctx, tenantID); err != nil {
		return nil, err
	}
	cleaned := make([]EgressRule, 0, len(rules))
	for _, r := range rules {
		hp := strings.TrimSpace(strings.ToLower(r.HostPattern))
		if hp == "" {
			return nil, fmt.Errorf("%w: host_pattern required", ErrInvalidInput)
		}
		if r.Port != nil && (*r.Port <= 0 || *r.Port > 65535) {
			return nil, fmt.Errorf("%w: port out of range", ErrInvalidInput)
		}
		id := r.ID
		if id == "" {
			id = newID()
		}
		cleaned = append(cleaned, EgressRule{
			ID: id, TenantID: tenantID, HostPattern: hp, Port: cloneInt(r.Port), Enabled: r.Enabled,
		})
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM tenant_egress_rules WHERE tenant_id=$1`, tenantID); err != nil {
		return nil, err
	}
	for _, r := range cleaned {
		_, err := tx.Exec(ctx, `
			INSERT INTO tenant_egress_rules (id, tenant_id, host_pattern, port, enabled)
			VALUES ($1,$2,$3,$4,$5)`,
			r.ID, r.TenantID, r.HostPattern, r.Port, r.Enabled,
		)
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return p.ListEgressRules(tenantID)
}

var _ Store = (*PostgresStore)(nil)

func (p *PostgresStore) PutAttestation(input PutAttestationInput) (AttestationRecord, error) {
	if strings.TrimSpace(input.SandboxID) == "" {
		return AttestationRecord{}, fmt.Errorf("%w: sandbox_id required", ErrInvalidInput)
	}
	ctx := context.Background()
	if _, err := p.GetSandbox(input.SandboxID); err != nil {
		return AttestationRecord{}, err
	}
	now := time.Now().UTC()
	bundle := input.Bundle
	if len(bundle) == 0 {
		bundle = json.RawMessage(`{}`)
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO sandbox_attestations (
			sandbox_id, node_id, image_digest, vmm_profile, cid, statement_ts,
			alg, key_id, signature, bundle, received_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (sandbox_id) DO UPDATE SET
			node_id=EXCLUDED.node_id,
			image_digest=EXCLUDED.image_digest,
			vmm_profile=EXCLUDED.vmm_profile,
			cid=EXCLUDED.cid,
			statement_ts=EXCLUDED.statement_ts,
			alg=EXCLUDED.alg,
			key_id=EXCLUDED.key_id,
			signature=EXCLUDED.signature,
			bundle=EXCLUDED.bundle,
			received_at=EXCLUDED.received_at`,
		input.SandboxID, input.NodeID, input.ImageDigest, input.VMMProfile, int(input.CID),
		input.StatementTS, input.Alg, input.KeyID, input.Signature, bundle, now,
	)
	if err != nil {
		return AttestationRecord{}, err
	}
	return p.GetAttestation(input.SandboxID)
}

func (p *PostgresStore) GetAttestation(sandboxID string) (AttestationRecord, error) {
	if strings.TrimSpace(sandboxID) == "" {
		return AttestationRecord{}, fmt.Errorf("%w: sandbox_id required", ErrInvalidInput)
	}
	ctx := context.Background()
	var rec AttestationRecord
	var cid int
	err := p.pool.QueryRow(ctx, `
		SELECT sandbox_id, node_id, image_digest, vmm_profile, cid, statement_ts,
		       alg, key_id, signature, bundle, received_at
		FROM sandbox_attestations WHERE sandbox_id=$1`, sandboxID).Scan(
		&rec.SandboxID, &rec.NodeID, &rec.ImageDigest, &rec.VMMProfile, &cid, &rec.StatementTS,
		&rec.Alg, &rec.KeyID, &rec.Signature, &rec.Bundle, &rec.ReceivedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return AttestationRecord{}, ErrNotFound
	}
	if err != nil {
		return AttestationRecord{}, err
	}
	rec.CID = uint32(cid)
	if rec.Bundle == nil {
		rec.Bundle = json.RawMessage(`{}`)
	}
	return rec, nil
}
