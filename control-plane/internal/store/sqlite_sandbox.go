package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/sched"
)

// The sandbox half of SQLiteStore. A method that changes a sandbox reads the row, applies the
// change in Go — the same code the memory store runs, which is the contract — and writes the
// row back, all in one transaction: SQLite has one writer, so nothing changes it in between.

// getSandboxSQL reads one sandbox; ErrNotFound when there is none.
func getSandboxSQL(ctx context.Context, q runner, id string) (Sandbox, error) {
	sb, err := scanSandboxSQL(rowOf(ctx, q, `SELECT `+sandboxColumns+` FROM sandboxes WHERE id=?`, id))
	if isNoRows(err) {
		return Sandbox{}, ErrNotFound
	}
	return sb, err
}

// saveSandboxSQL writes every column a transition can change. The tenant and the creation
// time never change.
func saveSandboxSQL(ctx context.Context, q runner, sb Sandbox) error {
	res, err := run(ctx, q, `
		UPDATE sandboxes SET
			node_id=?, state=?, vmm_profile=?, image_ref=?, cpu_millis=?, memory_mib=?, state_version=?,
			updated_at=?, owner_sub=?, owner_email=?, last_activity_at=?, stop_reason=?, status_detail=?,
			boot_count=?, booted_at=?, stopped_at=?, workspace_host_path=?,
			local_net=?, local_net_state=?, local_net_attached_at=?, local_net_grant_expires_at=?,
			local_net_grant_hash=?, local_net_client_public=?, local_net_node_public=?,
			local_net_listen_port=?, local_net_node_addr=?, local_net_client_addr=?
		WHERE id=?`,
		sb.NodeID, string(sb.State), sb.VMMProfile, sb.ImageRef, sb.CPUMillis, sb.MemoryMiB, sb.StateVersion,
		sb.UpdatedAt, sb.OwnerSub, sb.OwnerEmail, sb.LastActivityAt, sb.StopReason, sb.StatusDetail,
		sb.BootCount, sb.BootedAt, sb.StoppedAt, sb.WorkspaceHostPath,
		sb.LocalNet, sb.LocalNetState, sb.LocalNetAttachedAt, sb.LocalNetGrantExpiresAt,
		sb.LocalNetGrantHash, sb.LocalNetClientPublic, sb.LocalNetNodePublic,
		sb.LocalNetListenPort, sb.LocalNetNodeAddr, sb.LocalNetClientAddr,
		sb.ID)
	if err != nil {
		return err
	}
	if affected(res) == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) CreateSandbox(ctx context.Context, input CreateSandboxInput) (Sandbox, error) {
	if err := prepareCreateSandbox(&input); err != nil {
		return Sandbox{}, err
	}
	if err := s.ensureTenant(ctx, s.db, input.TenantID); err != nil {
		return Sandbox{}, fmt.Errorf("ensure tenant: %w", err)
	}
	if AutoProvisionEnabled() {
		// The stub provisioner assigns the local-dev row; real placement never uses it.
		if err := s.EnsureBootstrapNode(ctx); err != nil {
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

	var created Sandbox
	err := s.tx(ctx, func(tx *sql.Tx) error {
		// Decide and insert in one transaction: concurrent creates cannot overfill a node.
		var placedOn *string
		if !AutoProvisionEnabled() {
			cands, err := placementCandidatesSQL(ctx, tx)
			if err != nil {
				return fmt.Errorf("placement candidates: %w", err)
			}
			picked, err := sched.Place(s.schedCfg, placementRequest(input, vmm), cands, now)
			if err != nil {
				return err
			}
			placedOn = &picked
		}
		if _, err := run(ctx, tx, `
			INSERT INTO sandboxes (
				id, tenant_id, node_id, state, vmm_profile, image_ref,
				cpu_millis, memory_mib, state_version, boot_count, created_at, updated_at,
				owner_sub, owner_email, last_activity_at, stop_reason, workspace_host_path,
				local_net, local_net_state
			) VALUES (?,?,?,'requested',?,?,?,?,1,1,?,?,?,?,?,'',?,?,?)`,
			id, input.TenantID, placedOn, vmm, input.ImageRef, input.CPUMillis, input.MemoryMiB,
			now, now, ownerSub, ownerEmail, now, input.WorkspaceHostPath, lnOn, lnState,
		); err != nil {
			return fmt.Errorf("insert sandbox: %w", err)
		}
		if err := emitEventSQL(ctx, tx, EmitEventInput{
			SandboxID: id, TenantID: input.TenantID, EventType: "sandbox.created",
			ToState: strPtr(string(SandboxRequested)), Actor: "api", ActorSub: actorSub,
			Payload: json.RawMessage(`{}`),
		}); err != nil {
			return err
		}
		if lnOn {
			if err := emitEventSQL(ctx, tx, EmitEventInput{
				SandboxID: id, TenantID: input.TenantID, EventType: "sandbox.local_net_requested",
				Actor: "api", ActorSub: actorSub,
				Payload: mustJSON(map[string]any{"local_net": true, "local_net_state": lnState}),
			}); err != nil {
				return err
			}
		}
		if AutoProvisionEnabled() {
			// provision stub: requested → starting → running
			now2 := time.Now().UTC()
			if _, err := run(ctx, tx, `UPDATE sandboxes SET state='starting', state_version=state_version+1, updated_at=? WHERE id=?`, now2, id); err != nil {
				return err
			}
			if err := emitEventSQL(ctx, tx, EmitEventInput{
				SandboxID: id, TenantID: input.TenantID, EventType: "sandbox.state_changed",
				FromState: strPtr(string(SandboxRequested)), ToState: strPtr(string(SandboxStarting)),
				Actor: "provisioner", Payload: json.RawMessage(`{}`),
			}); err != nil {
				return err
			}
			now3 := time.Now().UTC()
			nodeID := s.provisionNodeID
			if input.NodeID != "" {
				nodeID = input.NodeID
			}
			if _, err := run(ctx, tx, `
				UPDATE sandboxes SET state='running', node_id=?, state_version=state_version+1, updated_at=?, last_activity_at=?,
				    booted_at=COALESCE(booted_at, ?)
				WHERE id=?`, nodeID, now3, now3, now3, id); err != nil {
				return err
			}
			if err := emitEventSQL(ctx, tx, EmitEventInput{
				SandboxID: id, TenantID: input.TenantID, EventType: "sandbox.state_changed",
				FromState: strPtr(string(SandboxStarting)), ToState: strPtr(string(SandboxRunning)),
				Actor: "provisioner", Payload: mustJSON(map[string]string{"node_id": nodeID}),
			}); err != nil {
				return err
			}
		} else if err := emitEventSQL(ctx, tx, EmitEventInput{
			SandboxID: id, TenantID: input.TenantID, EventType: "sandbox.placed",
			Actor: "scheduler", Payload: placedEventPayload(*placedOn, s.schedCfg, input),
		}); err != nil {
			return err
		}
		var err error
		created, err = getSandboxSQL(ctx, tx, id)
		return err
	})
	if err != nil {
		return Sandbox{}, err
	}
	return created, nil
}

func (s *SQLiteStore) GetSandbox(ctx context.Context, id string) (Sandbox, error) {
	return getSandboxSQL(ctx, s.db, id)
}

func (s *SQLiteStore) ListSandboxes(ctx context.Context, tenantID string) ([]Sandbox, error) {
	query, args := `SELECT `+sandboxColumns+` FROM sandboxes ORDER BY created_at DESC, id`, []any(nil)
	if tenantID != "" {
		query, args = `SELECT `+sandboxColumns+` FROM sandboxes WHERE tenant_id=? ORDER BY created_at DESC, id`, []any{tenantID}
	}
	rows, err := rowsOf(ctx, s.db, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Sandbox, 0)
	for rows.Next() {
		sb, err := scanSandboxSQL(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) ClaimSandbox(ctx context.Context, id, nodeID string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(nodeID) == "" {
		return Sandbox{}, fmt.Errorf("%w: id and node_id required", ErrInvalidInput)
	}
	var out Sandbox
	err := s.tx(ctx, func(tx *sql.Tx) error {
		sb, err := getSandboxSQL(ctx, tx, id)
		if err != nil {
			return err
		}
		// Only the node the scheduler placed the sandbox on can claim it (ADR-0011).
		if sb.NodeID == nil || *sb.NodeID != nodeID {
			return fmt.Errorf("%w: sandbox is not assigned to node %s", ErrConflict, nodeID)
		}
		if sb.State != SandboxRequested {
			return fmt.Errorf("%w: sandbox state %s not claimable", ErrConflict, sb.State)
		}
		from := string(sb.State)
		sb.State = SandboxStarting
		sb.StateVersion++
		sb.UpdatedAt = time.Now().UTC()
		if err := saveSandboxSQL(ctx, tx, sb); err != nil {
			return err
		}
		if err := emitEventSQL(ctx, tx, EmitEventInput{
			SandboxID: id, TenantID: sb.TenantID, EventType: "sandbox.claimed",
			FromState: &from, ToState: strPtr(string(SandboxStarting)), Actor: "node-agent",
			Payload: mustJSON(map[string]string{"node_id": nodeID}),
		}); err != nil {
			return err
		}
		out, err = getSandboxSQL(ctx, tx, id)
		return err
	})
	return out, err
}

func (s *SQLiteStore) ListNodeWork(ctx context.Context, nodeID string) (NodeWork, error) {
	if strings.TrimSpace(nodeID) == "" {
		return NodeWork{}, fmt.Errorf("%w: node_id required", ErrInvalidInput)
	}
	// What holds capacity, what is being deleted, and what a stop keeps.
	states := append(occupyingStateNames(), string(SandboxStopped))
	rows, err := rowsOf(ctx, s.db, `
		SELECT `+sandboxColumns+`
		FROM sandboxes
		WHERE node_id = ? AND state IN `+inList(len(states))+`
		ORDER BY created_at ASC`, append([]any{nodeID}, strArgs(states)...)...)
	if err != nil {
		return NodeWork{}, err
	}
	defer rows.Close()
	work := NodeWork{Sandboxes: []Sandbox{}, Assigned: []string{}, Retained: []string{}, Tenants: map[string]string{}}
	for rows.Next() {
		sb, err := scanSandboxSQL(rows)
		if err != nil {
			return NodeWork{}, err
		}
		work.add(sb)
	}
	if err := rows.Err(); err != nil {
		return NodeWork{}, err
	}
	work.sortWork()
	return work, nil
}

func (s *SQLiteStore) UpdateSandboxStatus(ctx context.Context, id string, state SandboxState, detail string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	if !ValidAgentStatus(state) {
		return Sandbox{}, fmt.Errorf("%w: invalid status %s", ErrInvalidInput, state)
	}
	var out Sandbox
	err := s.tx(ctx, func(tx *sql.Tx) error {
		sb, err := getSandboxSQL(ctx, tx, id)
		if err != nil {
			return err
		}
		if !ValidAgentTransition(sb.State, state) {
			return fmt.Errorf("%w: cannot move sandbox from %s to %s", ErrConflict, sb.State, state)
		}
		from := string(sb.State)
		prev := sb.State
		now := time.Now().UTC()
		sb.State = state
		sb.StateVersion++
		sb.UpdatedAt = now
		if state == SandboxFailed || state == SandboxStopped || state == SandboxStopping || state == SandboxDeleted {
			withdrawLocalNetFields(&sb)
		}
		switch {
		case state == SandboxRunning:
			sb.LastActivityAt = now
			sb.StopReason = ""
			sb.StatusDetail = ""
			if sb.BootedAt == nil {
				sb.BootedAt = &now
			}
		case reportsVMMExit(prev, state, detail):
			sb.StatusDetail = detail
			sb.StopReason = StopReasonVMMExited
		case detail != "" && recordsDetail(prev, state):
			sb.StatusDetail = detail
		}
		if state == SandboxStopped {
			sb.StoppedAt = &now
		}
		if err := saveSandboxSQL(ctx, tx, sb); err != nil {
			return err
		}
		payload := map[string]string{}
		if detail != "" {
			payload["detail"] = detail
		}
		if err := emitEventSQL(ctx, tx, EmitEventInput{
			SandboxID: id, TenantID: sb.TenantID, EventType: "sandbox.state_changed",
			FromState: &from, ToState: strPtr(string(state)), Actor: "node-agent", Payload: mustJSON(payload),
		}); err != nil {
			return err
		}
		out, err = getSandboxSQL(ctx, tx, id)
		return err
	})
	return out, err
}

func (s *SQLiteStore) TouchSandboxActivity(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	res, err := run(ctx, s.db, `UPDATE sandboxes SET last_activity_at=? WHERE id=?`, time.Now().UTC(), id)
	if err != nil {
		return err
	}
	if affected(res) == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) StopIdleSandboxes(ctx context.Context, now time.Time, idleFor time.Duration) ([]Sandbox, error) {
	if idleFor <= 0 {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cutoff := now.Add(-idleFor)
	rows, err := rowsOf(ctx, s.db, `
		SELECT `+sandboxColumns+`
		FROM sandboxes
		WHERE state IN ('requested','scheduled','starting','running','paused')
		  AND last_activity_at <= ?`, cutoff)
	if err != nil {
		return nil, err
	}
	candidates := make([]Sandbox, 0)
	for rows.Next() {
		sb, err := scanSandboxSQL(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, sb)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Sandbox, 0, len(candidates))
	for _, c := range candidates {
		if DecideIdle(c.LastActivityAt, now, idleFor) != IdleExpired {
			continue
		}
		var stopped Sandbox
		var skipped bool
		err := s.tx(ctx, func(tx *sql.Tx) error {
			sb, err := getSandboxSQL(ctx, tx, c.ID)
			if err != nil {
				return err
			}
			// Activity or another transition got there first.
			if sb.State != c.State || sb.LastActivityAt.After(cutoff) {
				skipped = true
				return nil
			}
			target := SandboxStopping
			if sb.State == SandboxRequested { // never claimed: no VM to stop
				target = SandboxStopped
			}
			from := string(sb.State)
			sb.State = target
			if target == SandboxStopped {
				sb.StoppedAt = &now
			}
			withdrawLocalNetFields(&sb)
			sb.StopReason = StopReasonIdle
			sb.StateVersion++
			sb.UpdatedAt = now
			if err := saveSandboxSQL(ctx, tx, sb); err != nil {
				return err
			}
			if err := emitEventSQL(ctx, tx, EmitEventInput{
				SandboxID: sb.ID, TenantID: sb.TenantID, EventType: "sandbox.idle_reaped",
				FromState: &from, ToState: strPtr(string(target)), Actor: "idle-reaper",
				Payload: mustJSON(map[string]any{"reason": StopReasonIdle, "idle_for": idleFor.String()}),
			}); err != nil {
				return err
			}
			stopped, err = getSandboxSQL(ctx, tx, sb.ID)
			return err
		})
		if err != nil {
			return out, err
		}
		if !skipped {
			out = append(out, stopped)
		}
	}
	return out, nil
}

// StopSandbox, DeleteSandbox and ResumeSandbox: the plans (stopPlan, deletePlan, resumePlan)
// are the ones the other stores use.

func (s *SQLiteStore) StopSandbox(ctx context.Context, id, actorSub string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	actorSub = strings.TrimSpace(actorSub)
	var out Sandbox
	err := s.tx(ctx, func(tx *sql.Tx) error {
		sb, err := getSandboxSQL(ctx, tx, id)
		if err != nil {
			return err
		}
		to, reason, err := stopPlan(sb.State)
		if err != nil {
			return err
		}
		if to == sb.State { // stopped or stopping already
			out = sb
			return nil
		}
		from := sb.State
		now := time.Now().UTC()
		sb.State = to
		if to == SandboxStopped {
			sb.StoppedAt = &now
		}
		withdrawLocalNetFields(&sb)
		sb.StateVersion++
		sb.UpdatedAt = now
		if err := saveSandboxSQL(ctx, tx, sb); err != nil {
			return err
		}
		if err := emitEventSQL(ctx, tx, transitionEvent(sb, from, actorSub, reason)); err != nil {
			return err
		}
		out, err = getSandboxSQL(ctx, tx, id)
		return err
	})
	return out, err
}

func (s *SQLiteStore) DeleteSandbox(ctx context.Context, id, actorSub string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	actorSub = strings.TrimSpace(actorSub)
	var out Sandbox
	err := s.tx(ctx, func(tx *sql.Tx) error {
		sb, err := getSandboxSQL(ctx, tx, id)
		if err != nil {
			return err
		}
		to, reason := deletePlan(sb)
		if to == sb.State { // deleting or deleted already
			out = sb
			return nil
		}
		from := sb.State
		sb.State = to
		withdrawLocalNetFields(&sb)
		sb.StateVersion++
		sb.UpdatedAt = time.Now().UTC()
		if err := saveSandboxSQL(ctx, tx, sb); err != nil {
			return err
		}
		if err := emitEventSQL(ctx, tx, transitionEvent(sb, from, actorSub, reason)); err != nil {
			return err
		}
		out, err = getSandboxSQL(ctx, tx, id)
		return err
	})
	return out, err
}

func (s *SQLiteStore) ResumeSandbox(ctx context.Context, id, actorSub string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	actorSub = strings.TrimSpace(actorSub)
	var out Sandbox
	err := s.tx(ctx, func(tx *sql.Tx) error {
		sb, err := getSandboxSQL(ctx, tx, id)
		if err != nil {
			return err
		}
		resume, err := resumePlan(sb)
		if err != nil {
			return err
		}
		if !resume {
			out = sb
			return nil
		}
		// Placement and the state change in one transaction: concurrent resumes and
		// creates cannot overfill the node.
		cands, err := placementCandidatesSQL(ctx, tx)
		if err != nil {
			return fmt.Errorf("placement candidates: %w", err)
		}
		now := time.Now().UTC()
		if _, err := sched.Place(s.schedCfg, resumeRequest(sb), cands, now); err != nil {
			return err
		}
		resumeFields(&sb, now)
		if err := saveSandboxSQL(ctx, tx, sb); err != nil {
			return err
		}
		if err := emitEventSQL(ctx, tx, resumedEvent(sb, actorSub)); err != nil {
			return err
		}
		out, err = getSandboxSQL(ctx, tx, id)
		return err
	})
	return out, err
}

// ---- retention (ADR-0012) ----

func (s *SQLiteStore) ExpireStoppedSandboxes(ctx context.Context, now time.Time, ttl time.Duration) ([]Sandbox, error) {
	if ttl <= 0 {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cutoff := now.Add(-ttl)
	return s.retentionSweep(ctx, now, StopReasonRetention, map[string]any{"ttl": ttl.String()},
		func(tx *sql.Tx) ([]Sandbox, error) {
			rows, err := rowsOf(ctx, tx, `
				SELECT `+sandboxColumns+` FROM sandboxes
				WHERE state='stopped' AND COALESCE(stopped_at, updated_at) <= ?`, cutoff)
			if err != nil {
				return nil, err
			}
			return collectSandboxes(rows)
		})
}

func (s *SQLiteStore) EvictStoppedOverCap(ctx context.Context, max int) ([]Sandbox, error) {
	if max <= 0 {
		return nil, nil
	}
	return s.retentionSweep(ctx, time.Now().UTC(), StopReasonTenantCap, map[string]any{"max_stopped_per_tenant": max},
		func(tx *sql.Tx) ([]Sandbox, error) {
			rows, err := rowsOf(ctx, tx, `SELECT `+sandboxColumns+` FROM sandboxes WHERE state='stopped'`)
			if err != nil {
				return nil, err
			}
			all, err := collectSandboxes(rows)
			if err != nil {
				return nil, err
			}
			byTenant := map[string][]Sandbox{}
			for _, sb := range all {
				byTenant[sb.TenantID] = append(byTenant[sb.TenantID], sb)
			}
			var victims []Sandbox
			for _, list := range byTenant {
				if len(list) <= max {
					continue
				}
				sortOldestStoppedFirst(list)
				victims = append(victims, list[:len(list)-max]...)
			}
			return victims, nil
		})
}

// retentionSweep deletes the sandboxes pick names, and records why, in one transaction.
func (s *SQLiteStore) retentionSweep(ctx context.Context, now time.Time, reason string, extra map[string]any,
	pick func(tx *sql.Tx) ([]Sandbox, error)) ([]Sandbox, error) {
	var out []Sandbox
	err := s.tx(ctx, func(tx *sql.Tx) error {
		victims, err := pick(tx)
		if err != nil {
			return err
		}
		for _, sb := range victims {
			sb.State = retentionDeleted(sb)
			sb.StopReason = reason
			withdrawLocalNetFields(&sb)
			sb.StateVersion++
			sb.UpdatedAt = now
			if err := saveSandboxSQL(ctx, tx, sb); err != nil {
				return err
			}
			if err := emitEventSQL(ctx, tx, retentionEvent(sb, reason, extra)); err != nil {
				return err
			}
			saved, err := getSandboxSQL(ctx, tx, sb.ID)
			if err != nil {
				return err
			}
			out = append(out, saved)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortByID(out)
	return out, nil
}

// collectSandboxes reads and closes rows of sandboxColumns.
func collectSandboxes(rows *sql.Rows) ([]Sandbox, error) {
	defer rows.Close()
	out := []Sandbox{}
	for rows.Next() {
		sb, err := scanSandboxSQL(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) CountSandboxes(ctx context.Context) ([]SandboxCount, error) {
	rows, err := rowsOf(ctx, s.db, `SELECT tenant_id, state, count(*) FROM sandboxes GROUP BY tenant_id, state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SandboxCount
	for rows.Next() {
		var c SandboxCount
		var state string
		if err := rows.Scan(&c.TenantID, &state, &c.Count); err != nil {
			return nil, err
		}
		c.State = SandboxState(state)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortSandboxCounts(out)
	return out, nil
}

func (s *SQLiteStore) CountStoppedByNode(ctx context.Context) (map[string]int64, error) {
	rows, err := rowsOf(ctx, s.db, `
		SELECT node_id, count(*) FROM sandboxes
		WHERE state='stopped' AND node_id IS NOT NULL AND node_id<>'' GROUP BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

func (s *SQLiteStore) ListNodeUsage(ctx context.Context) (map[string]NodeUsage, error) {
	return nodeUsageSQL(ctx, s.db)
}
