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

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/sched"
)

// PostgresStore implements Store against PostgreSQL via pgxpool.
type PostgresStore struct {
	pool            *pgxpool.Pool
	provisionNodeID string
	schedCfg        sched.Config
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{
		pool:            pool,
		provisionNodeID: DefaultLocalNodeID,
		schedCfg:        sched.DefaultConfig(),
	}
}

// SetSchedConfig sets the placement policy (ASP_SCHED_POLICY and friends).
func (p *PostgresStore) SetSchedConfig(cfg sched.Config) {
	p.schedCfg = cfg
}

// placementLockKey serialises placements across control-plane replicas
// (pg_advisory_xact_lock, released at commit or rollback).
const placementLockKey int64 = 0x41535031 // "ASP1"

// placementCandidates reads nodes and their usage through tx, after the placement
// lock: under READ COMMITTED each statement then sees every committed placement.
func placementCandidates(ctx context.Context, tx pgx.Tx) ([]sched.Candidate, error) {
	usage := map[string]NodeUsage{}
	rows, err := tx.Query(ctx, `
		SELECT node_id, COALESCE(SUM(cpu_millis),0)::bigint, COALESCE(SUM(memory_mib),0)::bigint, COUNT(*)
		FROM sandboxes WHERE node_id IS NOT NULL AND state = ANY($1)
		GROUP BY node_id`, occupyingStateNames())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var u NodeUsage
		if err := rows.Scan(&id, &u.CPUMillis, &u.MemoryMiB, &u.Sandboxes); err != nil {
			rows.Close()
			return nil, err
		}
		usage[id] = u
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	nrows, err := tx.Query(ctx, `SELECT `+nodeColumns+` FROM nodes`)
	if err != nil {
		return nil, err
	}
	defer nrows.Close()
	var out []sched.Candidate
	for nrows.Next() {
		n, err := scanNode(nrows)
		if err != nil {
			return nil, err
		}
		out = append(out, Candidate(n, usage[n.ID]))
	}
	return out, nrows.Err()
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
	if AutoProvisionEnabled() {
		// The stub provisioner assigns the local-dev row; real placement never uses it.
		if err := p.EnsureBootstrapNode(ctx); err != nil {
			return Sandbox{}, fmt.Errorf("ensure bootstrap node: %w", err)
		}
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

	var placedOn *string
	if !AutoProvisionEnabled() {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, placementLockKey); err != nil {
			return Sandbox{}, fmt.Errorf("placement lock: %w", err)
		}
		cands, err := placementCandidates(ctx, tx)
		if err != nil {
			return Sandbox{}, fmt.Errorf("placement candidates: %w", err)
		}
		picked, err := sched.Place(p.schedCfg, placementRequest(input, vmm), cands, now)
		if err != nil {
			return Sandbox{}, err
		}
		placedOn = &picked
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO sandboxes (
			id, tenant_id, node_id, state, vmm_profile, image_ref,
			cpu_millis, memory_mib, state_version, created_at, updated_at,
			owner_sub, owner_email, last_activity_at, stop_reason, workspace_host_path,
			local_net, local_net_state, local_net_attached_at, local_net_grant_expires_at, local_net_grant_hash, local_net_client_public
		) VALUES ($1,$2,$14,'requested',$3,$4,$5,$6,1,$7,$7,$8,$9,$7,'',$10,$11,$12,NULL,NULL,$13,'')`,
		id, input.TenantID, vmm, input.ImageRef, input.CPUMillis, input.MemoryMiB, now,
		ownerSub, ownerEmail, input.WorkspaceHostPath, lnOn, lnState, "", placedOn,
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
	} else if err := emitEventTx(ctx, tx, EmitEventInput{
		SandboxID: id,
		TenantID:  input.TenantID,
		EventType: "sandbox.placed",
		Actor:     "scheduler",
		Payload:   placedEventPayload(*placedOn, p.schedCfg, input),
	}); err != nil {
		return Sandbox{}, err
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
		       cpu_millis, memory_mib, state_version, created_at, updated_at,
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
			       cpu_millis, memory_mib, state_version, created_at, updated_at,
			       owner_sub, owner_email, last_activity_at, stop_reason, workspace_host_path,
			local_net, local_net_state, local_net_attached_at, local_net_grant_expires_at, local_net_grant_hash, local_net_client_public, local_net_node_public
			FROM sandboxes ORDER BY created_at DESC`)
	} else {
		rows, err = p.pool.Query(ctx, `
			SELECT id, tenant_id, node_id, state, vmm_profile, image_ref,
			       cpu_millis, memory_mib, state_version, created_at, updated_at,
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

func (p *PostgresStore) ClaimSandbox(id, nodeID string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(nodeID) == "" {
		return Sandbox{}, fmt.Errorf("%w: id and node_id required", ErrInvalidInput)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Sandbox{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Only the node the scheduler placed the sandbox on can claim it (ADR-0011).
	var tenantID string
	err = tx.QueryRow(ctx, `
		UPDATE sandboxes
		SET state='starting', state_version=state_version+1, updated_at=$3
		WHERE id=$1 AND state='requested' AND node_id=$2
		RETURNING tenant_id`,
		id, nodeID, now).Scan(&tenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		sb, gerr := p.GetSandbox(id)
		if gerr != nil {
			return Sandbox{}, gerr
		}
		if sb.NodeID == nil || *sb.NodeID != nodeID {
			return Sandbox{}, fmt.Errorf("%w: sandbox is not assigned to node %s", ErrConflict, nodeID)
		}
		return Sandbox{}, fmt.Errorf("%w: sandbox state %s not claimable", ErrConflict, sb.State)
	}
	if err != nil {
		return Sandbox{}, err
	}
	from := string(SandboxRequested)
	if err := emitEventTx(ctx, tx, EmitEventInput{
		SandboxID: id,
		TenantID:  tenantID,
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

func (p *PostgresStore) ListNodeWork(nodeID string) (NodeWork, error) {
	if strings.TrimSpace(nodeID) == "" {
		return NodeWork{}, fmt.Errorf("%w: node_id required", ErrInvalidInput)
	}
	ctx := context.Background()
	rows, err := p.pool.Query(ctx, `
		SELECT id, tenant_id, node_id, state, vmm_profile, image_ref,
		       cpu_millis, memory_mib, state_version, created_at, updated_at,
		       owner_sub, owner_email, last_activity_at, stop_reason, workspace_host_path,
			local_net, local_net_state, local_net_attached_at, local_net_grant_expires_at, local_net_grant_hash, local_net_client_public, local_net_node_public
		FROM sandboxes
		WHERE node_id = $1 AND state = ANY($2)
		ORDER BY created_at ASC`, nodeID, occupyingStateNames())
	if err != nil {
		return NodeWork{}, err
	}
	defer rows.Close()
	work := NodeWork{Sandboxes: []Sandbox{}, Assigned: []string{}, Tenants: map[string]string{}}
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return NodeWork{}, err
		}
		work.Assigned = append(work.Assigned, sb.ID)
		work.Tenants[sb.ID] = sb.TenantID
		if NeedsNodeAction(sb) {
			work.Sandboxes = append(work.Sandboxes, sb)
		}
	}
	return work, rows.Err()
}

func (p *PostgresStore) UpdateSandboxStatus(id string, state SandboxState, detail string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	if !ValidAgentStatus(state) {
		return Sandbox{}, fmt.Errorf("%w: invalid status %s", ErrInvalidInput, state)
	}
	ctx := context.Background()
	// Guarded by the state the transition was checked against; retry if it moved.
	for attempt := 0; attempt < 3; attempt++ {
		sb, err := p.GetSandbox(id)
		if err != nil {
			return Sandbox{}, err
		}
		if !ValidAgentTransition(sb.State, state) {
			return Sandbox{}, fmt.Errorf("%w: cannot move sandbox from %s to %s", ErrConflict, sb.State, state)
		}
		from := string(sb.State)
		now := time.Now().UTC()
		tx, err := p.pool.Begin(ctx)
		if err != nil {
			return Sandbox{}, err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE sandboxes SET state=$2, state_version=state_version+1, updated_at=$3,
			    last_activity_at=CASE WHEN $2='running' THEN $3 ELSE last_activity_at END,
			    stop_reason=CASE WHEN $2='running' THEN '' ELSE stop_reason END,
			    local_net_state=CASE
			      WHEN $2 IN ('failed','stopped','stopping') AND local_net THEN 'withdrawn'
			      WHEN $2 IN ('failed','stopped','stopping') AND NOT local_net THEN 'off'
			      ELSE local_net_state END,
			    local_net_client_public=CASE WHEN $2 IN ('failed','stopped','stopping') THEN '' ELSE local_net_client_public END,
			    local_net_grant_hash=CASE WHEN $2 IN ('failed','stopped','stopping') THEN '' ELSE local_net_grant_hash END,
			    local_net_grant_expires_at=CASE WHEN $2 IN ('failed','stopped','stopping') THEN NULL ELSE local_net_grant_expires_at END
			WHERE id=$1 AND state=$4`, id, string(state), now, from)
		if err != nil {
			_ = tx.Rollback(ctx)
			return Sandbox{}, err
		}
		if tag.RowsAffected() == 0 {
			_ = tx.Rollback(ctx)
			continue
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
			_ = tx.Rollback(ctx)
			return Sandbox{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Sandbox{}, err
		}
		return p.GetSandbox(id)
	}
	return Sandbox{}, fmt.Errorf("%w: sandbox %s changed state concurrently; retry", ErrConflict, id)
}

func (p *PostgresStore) MarkSandboxStopping(id, actorSub string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	actorSub = strings.TrimSpace(actorSub)
	ctx := context.Background()
	// The update is guarded by the state it was decided from: a node claiming the
	// sandbox in between changes the decision, so read again and retry.
	for attempt := 0; attempt < 3; attempt++ {
		sb, err := p.GetSandbox(id)
		if err != nil {
			return Sandbox{}, err
		}
		if sb.State == SandboxStopped || sb.State == SandboxStopping {
			return sb, nil
		}
		from := string(sb.State)
		target := SandboxStopping
		payload := json.RawMessage(`{"reason":"destroy"}`)
		if sb.State == SandboxRequested {
			// Never claimed: no VM exists before a node claims it.
			target = SandboxStopped
			payload = json.RawMessage(`{"reason":"destroy_unclaimed"}`)
		} else if sb.State == SandboxFailed {
			target = SandboxStopped
			payload = json.RawMessage(`{"reason":"destroy_failed"}`)
		} else if !IsActiveLifecycle(sb.State) {
			return Sandbox{}, fmt.Errorf("%w: cannot destroy from state %s", ErrConflict, sb.State)
		}

		now := time.Now().UTC()
		tx, err := p.pool.Begin(ctx)
		if err != nil {
			return Sandbox{}, err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE sandboxes SET state=$2, state_version=state_version+1, updated_at=$3,
			    local_net_state=CASE WHEN local_net THEN 'withdrawn' ELSE 'off' END,
			    local_net_client_public='',
			    local_net_grant_hash='',
			    local_net_grant_expires_at=NULL
			WHERE id=$1 AND state=$4`, id, string(target), now, from)
		if err != nil {
			_ = tx.Rollback(ctx)
			return Sandbox{}, err
		}
		if tag.RowsAffected() == 0 {
			_ = tx.Rollback(ctx)
			continue // the state moved under us; decide again
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
			_ = tx.Rollback(ctx)
			return Sandbox{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Sandbox{}, err
		}
		return p.GetSandbox(id)
	}
	return Sandbox{}, fmt.Errorf("%w: sandbox %s changed state concurrently; retry", ErrConflict, id)
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
		       cpu_millis, memory_mib, state_version, created_at, updated_at,
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
		if sb.State == SandboxRequested { // never claimed: no VM to stop
			target = SandboxStopped
		}
		from := string(sb.State)
		tx, err := p.pool.Begin(ctx)
		if err != nil {
			return out, err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE sandboxes
			SET state=$2, stop_reason=$3,
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
	if err := validateNodeCapacity(input.CapacityCPU, input.CapacityMemMiB, input.MaxSandboxes); err != nil {
		return Node{}, err
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
	var revokedAt *time.Time
	var prevInstance string
	err = tx.QueryRow(ctx, `SELECT created_at, revoked_at, agent_instance_id FROM nodes WHERE id=$1 FOR UPDATE`, id).Scan(&createdAt, &revokedAt, &prevInstance)
	isNew := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !isNew {
		return Node{}, err
	}
	if revokedAt != nil {
		return Node{}, errNodeRevoked(id)
	}

	if isNew {
		createdAt = now
		_, err = tx.Exec(ctx, `
			INSERT INTO nodes (
				id, name, endpoint, agent_endpoint, state, vmm_profiles,
				capacity_cpu, capacity_mem_mib, fence_endpoint, fence_token,
				last_seen_at, created_at, updated_at,
				max_sandboxes, accepts_work, local_net_dial, agent_instance_id
			) VALUES ($1,$2,$3,$4,'ready',$5,$6,$7,$8,$9,$10,$10,$10,$11,$12,$13,$14)`,
			id, name, input.Endpoint, agentEndpoint, profiles, input.CapacityCPU, input.CapacityMemMiB,
			strings.TrimSpace(input.FenceEndpoint), strings.TrimSpace(input.FenceToken), now,
			input.MaxSandboxes, input.acceptsWork(), strings.TrimSpace(input.LocalNetDial),
			strings.TrimSpace(input.AgentInstanceID),
		)
	} else {
		// cordoned is an admin decision: register never touches it.
		_, err = tx.Exec(ctx, `
			UPDATE nodes SET
				name=$2, endpoint=$3,
				agent_endpoint=CASE WHEN $4 = '' THEN agent_endpoint ELSE $4 END,
				state='ready', vmm_profiles=$5,
				capacity_cpu=$6, capacity_mem_mib=$7,
				fence_endpoint=CASE WHEN $8 = '' THEN fence_endpoint ELSE $8 END,
				fence_token=CASE WHEN $9 = '' THEN fence_token ELSE $9 END,
				last_seen_at=$10, updated_at=$10,
				max_sandboxes=$11, accepts_work=$12, local_net_dial=$13,
				agent_instance_id=CASE WHEN $14 = '' THEN agent_instance_id ELSE $14 END
			WHERE id=$1`,
			id, name, input.Endpoint, agentEndpoint, profiles, input.CapacityCPU, input.CapacityMemMiB,
			strings.TrimSpace(input.FenceEndpoint), strings.TrimSpace(input.FenceToken), now,
			input.MaxSandboxes, input.acceptsWork(), strings.TrimSpace(input.LocalNetDial),
			strings.TrimSpace(input.AgentInstanceID),
		)
		if err == nil && agentRestarted(prevInstance, strings.TrimSpace(input.AgentInstanceID)) {
			err = failRestartOrphansTx(ctx, tx, id, now)
		}
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

func (p *PostgresStore) CreateEnrollToken(tok EnrollToken) error {
	if err := validateEnrollToken(tok); err != nil {
		return err
	}
	ctx := context.Background()
	now := time.Now().UTC()
	if tok.CreatedAt.IsZero() {
		tok.CreatedAt = now
	}
	_, _ = p.pool.Exec(ctx, `DELETE FROM node_enroll_tokens WHERE expires_at < $1`, now.Add(-enrollTokenRetention))
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO node_enroll_tokens (hash, node_id, expires_at, created_by, created_at)
		VALUES ($1, NULLIF($2, ''), $3, $4, $5)
		ON CONFLICT (hash) DO NOTHING`,
		tok.Hash, strings.TrimSpace(tok.NodeID), tok.ExpiresAt, tok.CreatedBy, tok.CreatedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAlreadyExists
	}
	return nil
}

func (p *PostgresStore) CheckEnroll(id string, auth EnrollAuth) error {
	_, _, err := enrollCheckTx(context.Background(), p.pool, id, auth, time.Now().UTC(), false)
	return err
}

// enrollCheckTx reads the enroll token and the node's certificate state and
// applies enrollAllowed. With lock it takes row locks (FOR UPDATE), so a
// token cannot be used by two enrollments and a node not enrolled twice at
// once. It returns the node's current certificate fingerprint ("" if none)
// and whether the node exists.
func enrollCheckTx(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, id string, auth EnrollAuth, now time.Time, lock bool) (oldFP string, exists bool, err error) {
	forUpdate := ""
	if lock {
		forUpdate = " FOR UPDATE"
	}
	var tok *EnrollToken
	if auth.TokenHash != "" {
		var t EnrollToken
		var pinned *string
		err := q.QueryRow(ctx, `SELECT node_id, expires_at, used_at FROM node_enroll_tokens WHERE hash=$1`+forUpdate,
			auth.TokenHash).Scan(&pinned, &t.ExpiresAt, &t.UsedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, ErrEnrollTokenInvalid
		}
		if err != nil {
			return "", false, err
		}
		if pinned != nil {
			t.NodeID = *pinned
		}
		tok = &t
	}
	var revokedAt *time.Time
	err = q.QueryRow(ctx, `SELECT cert_fingerprint, revoked_at FROM nodes WHERE id=$1`+forUpdate, id).Scan(&oldFP, &revokedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		oldFP, exists = "", false
	case err != nil:
		return "", false, err
	default:
		exists = true
	}
	live := exists && enrolledLive(Node{CertFingerprint: oldFP, RevokedAt: revokedAt})
	return oldFP, exists, enrollAllowed(id, tok, live, now)
}

func (p *PostgresStore) EnrollNode(input EnrollNodeInput, cert CertMeta, auth EnrollAuth) (Node, error) {
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

	oldFP, exists, err := enrollCheckTx(ctx, tx, id, auth, now, true)
	if err != nil {
		return Node{}, err
	}
	if auth.TokenHash != "" {
		if _, err := tx.Exec(ctx, `UPDATE node_enroll_tokens SET used_at=$2, used_by=$3 WHERE hash=$1`, auth.TokenHash, now, id); err != nil {
			return Node{}, err
		}
	}
	if exists {
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
				enrolled_at=$10, last_seen_at=$10, updated_at=$10, revoked_at=NULL, cert_not_after=$11
			WHERE id=$1`,
			id, name, input.Endpoint, agentEndpoint, profiles,
			input.CapacityCPU, input.CapacityMemMiB, fp, serial, now, cert.notAfterPtr(),
		)
	} else {
		var tag pgconn.CommandTag
		tag, err = tx.Exec(ctx, `
			INSERT INTO nodes (
				id, name, endpoint, agent_endpoint, state, vmm_profiles,
				capacity_cpu, capacity_mem_mib, cert_fingerprint, cert_serial, enrolled_at,
				last_seen_at, created_at, updated_at, cert_not_after
			) VALUES ($1,$2,$3,$4,'ready',$5,$6,$7,$8,$9,$10,$10,$10,$10,$11)
			ON CONFLICT (id) DO NOTHING`,
			id, name, input.Endpoint, agentEndpoint, profiles,
			input.CapacityCPU, input.CapacityMemMiB, fp, serial, now, cert.notAfterPtr(),
		)
		if err == nil && tag.RowsAffected() == 0 {
			// Another enrollment created the node first.
			return Node{}, fmt.Errorf("%w: %s", ErrNodeEnrolled, id)
		}
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
			"enroll_auth":      enrollAuthName(auth),
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
		UPDATE nodes SET cert_fingerprint=$2, cert_serial=$3, updated_at=$4, cert_not_after=$5
		WHERE id=$1`, nodeID, fp, serial, now, cert.notAfterPtr())
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
	var prevState string
	// The previous state comes from a locking sub-select in FROM, which runs
	// before the row is updated. A CTE read only in RETURNING runs after the
	// update, and its FOR UPDATE then skips the row this statement changed:
	// the subquery returns NULL.
	err := p.pool.QueryRow(ctx, `
		UPDATE nodes n SET last_seen_at=$2, updated_at=$2, state='ready'
		FROM (SELECT id, state FROM nodes WHERE id=$1 AND revoked_at IS NULL FOR UPDATE) prev
		WHERE n.id = prev.id
		RETURNING prev.state`, id, now).Scan(&prevState)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Node{}, err
	}
	if err == nil {
		p.recordNodeOnline(ctx, id, prevState)
	} else {
		var revokedAt *time.Time
		err := p.pool.QueryRow(ctx, `SELECT revoked_at FROM nodes WHERE id=$1`, id).Scan(&revokedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return Node{}, ErrNotFound
		}
		if err != nil {
			return Node{}, err
		}
		return Node{}, errNodeRevoked(id)
	}
	return p.GetNode(id)
}

func (p *PostgresStore) TouchNodePoll(id string, now time.Time) error {
	ctx := context.Background()
	var prevState string
	// As in HeartbeatNode. The throttle is in the sub-select, so a poll inside
	// the window neither locks nor writes the row.
	err := p.pool.QueryRow(ctx, `
		UPDATE nodes n SET last_seen_at=$2, updated_at=$2,
		    state=CASE WHEN n.revoked_at IS NULL THEN 'ready' ELSE n.state END
		FROM (SELECT id, state FROM nodes
		      WHERE id=$1 AND (last_seen_at IS NULL OR last_seen_at < $3) FOR UPDATE) prev
		WHERE n.id = prev.id
		RETURNING prev.state`,
		id, now, now.Add(-nodePollWriteEvery)).Scan(&prevState)
	if err == nil {
		p.recordNodeOnline(ctx, id, prevState)
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var exists bool
	if err := p.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM nodes WHERE id=$1)`, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	return nil // seen recently; nothing to write
}

func (p *PostgresStore) SetNodeCordoned(id string, cordoned bool) (Node, error) {
	ctx := context.Background()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Node{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE nodes SET cordoned=$2, updated_at=$3 WHERE id=$1`, id, cordoned, time.Now().UTC())
	if err != nil {
		return Node{}, err
	}
	if tag.RowsAffected() == 0 {
		return Node{}, ErrNotFound
	}
	eventType := "node.uncordoned"
	if cordoned {
		eventType = "node.cordoned"
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO node_events (node_id, event_type, actor, payload)
		VALUES ($1,$2,'api','{}'::jsonb)`, id, eventType); err != nil {
		return Node{}, fmt.Errorf("node event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Node{}, err
	}
	return p.GetNode(id)
}

func (p *PostgresStore) ListNodeUsage() (map[string]NodeUsage, error) {
	ctx := context.Background()
	rows, err := p.pool.Query(ctx, `
		SELECT node_id, COALESCE(SUM(cpu_millis),0)::bigint, COALESCE(SUM(memory_mib),0)::bigint, COUNT(*)
		FROM sandboxes WHERE node_id IS NOT NULL AND state = ANY($1)
		GROUP BY node_id`, occupyingStateNames())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	usage := map[string]NodeUsage{}
	for rows.Next() {
		var id string
		var u NodeUsage
		if err := rows.Scan(&id, &u.CPUMillis, &u.MemoryMiB, &u.Sandboxes); err != nil {
			return nil, err
		}
		usage[id] = u
	}
	return usage, rows.Err()
}

func (p *PostgresStore) ListNodes() ([]Node, error) {
	ctx := context.Background()
	rows, err := p.pool.Query(ctx, `
		SELECT `+nodeColumns+`
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
		SELECT `+nodeColumns+`
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
		SELECT id, tenant_id, name, scope, key_prefix, secret_hash,
		       last_used_at, expires_at, revoked_at, created_at
		FROM api_keys
		WHERE secret_hash=$1 AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > now())`, secretHash)
	var k ApiKey
	err := row.Scan(
		&k.ID, &k.TenantID, &k.Name, &k.Scope, &k.KeyPrefix, &k.SecretHash,
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

func (p *PostgresStore) EnsureAPIKey(tenantID, name, scope, keyPrefix, secretHash string) (ApiKey, error) {
	if tenantID == "" || name == "" || keyPrefix == "" || secretHash == "" {
		return ApiKey{}, fmt.Errorf("%w: tenant_id, name, key_prefix, secret_hash required", ErrInvalidInput)
	}
	scope, err := normalizeScope(scope)
	if err != nil {
		return ApiKey{}, err
	}
	ctx := context.Background()
	if err := p.ensureTenant(ctx, tenantID); err != nil {
		return ApiKey{}, err
	}
	// Return existing by tenant+name if present.
	var k ApiKey
	err = p.pool.QueryRow(ctx, `
		SELECT id, tenant_id, name, scope, key_prefix, secret_hash,
		       last_used_at, expires_at, revoked_at, created_at
		FROM api_keys WHERE tenant_id=$1 AND name=$2`, tenantID, name,
	).Scan(
		&k.ID, &k.TenantID, &k.Name, &k.Scope, &k.KeyPrefix, &k.SecretHash,
		&k.LastUsedAt, &k.ExpiresAt, &k.RevokedAt, &k.CreatedAt,
	)
	if err == nil {
		if k.SecretHash != secretHash || k.KeyPrefix != keyPrefix || k.Scope != scope {
			_, uerr := p.pool.Exec(ctx, `
				UPDATE api_keys SET secret_hash=$2, key_prefix=$3, scope=$4, revoked_at=NULL
				WHERE id=$1`, k.ID, secretHash, keyPrefix, scope)
			if uerr != nil {
				return ApiKey{}, uerr
			}
			k.SecretHash = secretHash
			k.KeyPrefix = keyPrefix
			k.Scope = scope
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
		INSERT INTO api_keys (id, tenant_id, name, scope, key_prefix, secret_hash, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		id, tenantID, name, scope, keyPrefix, secretHash, now,
	)
	if err != nil {
		return ApiKey{}, err
	}
	return ApiKey{
		ID: id, TenantID: tenantID, Name: name, Scope: scope,
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
	// The middleware already writes at most once a minute per key and process;
	// the guard keeps several control-plane replicas to the same pace.
	_, err := p.pool.Exec(ctx, `
		UPDATE api_keys SET last_used_at=now()
		WHERE id=$1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')`, id)
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
		&sb.CPUMillis, &sb.MemoryMiB, &sb.StateVersion,
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

// nodeColumns is the column list scanNode reads, in order.
const nodeColumns = `id, name, endpoint, agent_endpoint, state, vmm_profiles,
		capacity_cpu, capacity_mem_mib, max_sandboxes, cordoned, accepts_work, local_net_dial, agent_instance_id,
		cert_fingerprint, cert_serial, cert_not_after, fence_token, fence_endpoint, enrolled_at,
		revoked_at, last_seen_at, created_at, updated_at`

func scanNode(row scannable) (Node, error) {
	var n Node
	err := row.Scan(
		&n.ID, &n.Name, &n.Endpoint, &n.AgentEndpoint, &n.State, &n.VMMProfiles,
		&n.CapacityCPU, &n.CapacityMemMiB, &n.MaxSandboxes, &n.Cordoned, &n.AcceptsWork, &n.LocalNetDial, &n.AgentInstanceID,
		&n.CertFingerprint, &n.CertSerial, &n.CertNotAfter, &n.FenceToken, &n.FenceEndpoint, &n.EnrolledAt,
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

func (p *PostgresStore) ListEgressRulesForTenants(tenantIDs []string) (map[string][]EgressRule, error) {
	out := make(map[string][]EgressRule, len(tenantIDs))
	for _, t := range tenantIDs {
		out[t] = []EgressRule{}
	}
	if len(tenantIDs) == 0 {
		return out, nil
	}
	rows, err := p.pool.Query(context.Background(), `
		SELECT id, tenant_id, host_pattern, port, enabled
		FROM tenant_egress_rules WHERE tenant_id = ANY($1)
		ORDER BY tenant_id, host_pattern, port NULLS FIRST`, tenantIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r EgressRule
		if err := rows.Scan(&r.ID, &r.TenantID, &r.HostPattern, &r.Port, &r.Enabled); err != nil {
			return nil, err
		}
		out[r.TenantID] = append(out[r.TenantID], r)
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
