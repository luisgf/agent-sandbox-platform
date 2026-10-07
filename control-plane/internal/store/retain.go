package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/sched"
)

// Stop, resume and delete (ADR-0012). A stop keeps the sandbox's disk on its
// node, a resume boots it again on that node, a delete removes it.

// stopPlan says where StopSandbox moves a sandbox in state from. to == from is
// nothing to do. Failed, deleting and deleted cannot be stopped: a failed
// sandbox has no VM, so there is nothing to stop and nothing worth resuming.
func stopPlan(from SandboxState) (to SandboxState, reason string, err error) {
	switch from {
	case SandboxStopped, SandboxStopping:
		return from, "", nil
	case SandboxRequested: // never claimed: no VM yet
		return SandboxStopped, "stop_unclaimed", nil
	case SandboxScheduled, SandboxStarting, SandboxRunning, SandboxPaused:
		return SandboxStopping, "stop", nil
	}
	return from, "", fmt.Errorf("%w: cannot stop from state %s", ErrConflict, from)
}

// deletePlan says where DeleteSandbox moves a sandbox. A node has something to
// remove (a VM, or the disk a stop kept) unless the sandbox was never claimed,
// failed (its VM is gone, and the node's disk GC takes any disk that is left)
// or was stopped without a node. to == from is nothing to do.
func deletePlan(sb Sandbox) (to SandboxState, reason string) {
	switch sb.State {
	case SandboxDeleted, SandboxDeleting:
		return sb.State, ""
	case SandboxRequested:
		return SandboxDeleted, "delete_unclaimed"
	case SandboxFailed:
		return SandboxDeleted, "delete_failed"
	case SandboxStopped:
		if sb.NodeID == nil || *sb.NodeID == "" {
			return SandboxDeleted, "delete_stopped"
		}
		return SandboxDeleting, "delete_stopped"
	}
	return SandboxDeleting, "delete"
}

// resumePlan: stopped is resumed; a sandbox already on its way up is left
// alone; anything else cannot be resumed.
func resumePlan(sb Sandbox) (resume bool, err error) {
	switch sb.State {
	case SandboxStopped:
		if sb.NodeID == nil || *sb.NodeID == "" {
			return false, fmt.Errorf("%w: sandbox has no node: nothing to resume", ErrConflict)
		}
		return true, nil
	case SandboxRequested, SandboxScheduled, SandboxStarting, SandboxRunning:
		return false, nil
	case SandboxStopping:
		return false, fmt.Errorf("%w: sandbox is stopping; wait until it is stopped", ErrConflict)
	}
	return false, fmt.Errorf("%w: cannot resume from state %s", ErrConflict, sb.State)
}

// resumeRequest is the placement a resume asks for: the sandbox's own spec, on
// the node that holds its disk.
func resumeRequest(sb Sandbox) sched.Request {
	return sched.Request{
		CPUMillis:    sb.CPUMillis,
		MemoryMiB:    sb.MemoryMiB,
		VMMProfile:   sb.VMMProfile,
		PinnedNodeID: *sb.NodeID,
	}
}

func transitionEvent(sb Sandbox, from SandboxState, actorSub, reason string) EmitEventInput {
	f := string(from)
	return EmitEventInput{
		SandboxID: sb.ID,
		TenantID:  sb.TenantID,
		EventType: "sandbox.state_changed",
		FromState: &f,
		ToState:   strPtr(string(sb.State)),
		Actor:     "api",
		ActorSub:  actorSub,
		Payload:   mustJSON(map[string]string{"reason": reason}),
	}
}

func resumedEvent(sb Sandbox, actorSub string) EmitEventInput {
	from := string(SandboxStopped)
	node := ""
	if sb.NodeID != nil {
		node = *sb.NodeID
	}
	return EmitEventInput{
		SandboxID: sb.ID,
		TenantID:  sb.TenantID,
		EventType: "sandbox.resumed",
		FromState: &from,
		ToState:   strPtr(string(SandboxRequested)),
		Actor:     "api",
		ActorSub:  actorSub,
		Payload:   mustJSON(map[string]any{"node_id": node, "boot_count": sb.BootCount}),
	}
}

// ---- memory store ----

func (m *MemoryStore) StopSandbox(ctx context.Context, id, actorSub string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	actorSub = strings.TrimSpace(actorSub)
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return Sandbox{}, ErrNotFound
	}
	to, reason, err := stopPlan(sb.State)
	if err != nil {
		m.mu.Unlock()
		return Sandbox{}, err
	}
	if to == sb.State {
		out := cloneSandbox(sb)
		m.mu.Unlock()
		return out, nil
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
	m.sandboxes[id] = sb
	out := cloneSandbox(sb)
	m.mu.Unlock()
	_ = m.EmitEvent(ctx, transitionEvent(out, from, actorSub, reason))
	return out, nil
}

func (m *MemoryStore) DeleteSandbox(ctx context.Context, id, actorSub string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	actorSub = strings.TrimSpace(actorSub)
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return Sandbox{}, ErrNotFound
	}
	to, reason := deletePlan(sb)
	if to == sb.State {
		out := cloneSandbox(sb)
		m.mu.Unlock()
		return out, nil
	}
	from := sb.State
	sb.State = to
	withdrawLocalNetFields(&sb)
	sb.StateVersion++
	sb.UpdatedAt = time.Now().UTC()
	m.sandboxes[id] = sb
	out := cloneSandbox(sb)
	m.mu.Unlock()
	_ = m.EmitEvent(ctx, transitionEvent(out, from, actorSub, reason))
	return out, nil
}

func (m *MemoryStore) ResumeSandbox(ctx context.Context, id, actorSub string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	actorSub = strings.TrimSpace(actorSub)
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return Sandbox{}, ErrNotFound
	}
	resume, err := resumePlan(sb)
	if err != nil {
		m.mu.Unlock()
		return Sandbox{}, err
	}
	if !resume {
		out := cloneSandbox(sb)
		m.mu.Unlock()
		return out, nil
	}
	// Placement and the state change under one lock: concurrent resumes and
	// creates cannot overfill the node.
	usage := m.nodeUsageLocked()
	cands := make([]sched.Candidate, 0, len(m.nodes))
	for _, n := range m.nodes {
		cands = append(cands, Candidate(n, usage[n.ID]))
	}
	now := time.Now().UTC()
	if _, err := sched.Place(m.schedCfg, resumeRequest(sb), cands, now); err != nil {
		m.mu.Unlock()
		return Sandbox{}, err
	}
	resumeFields(&sb, now)
	m.sandboxes[id] = sb
	out := cloneSandbox(sb)
	m.mu.Unlock()
	_ = m.EmitEvent(ctx, resumedEvent(out, actorSub))
	return out, nil
}

// resumeFields is what a resume sets: back to requested on the same node, one
// more boot, and a clean slate for the reasons of the last stop.
func resumeFields(sb *Sandbox, now time.Time) {
	sb.State = SandboxRequested
	sb.BootCount++
	sb.StopReason = ""
	sb.StatusDetail = ""
	sb.StoppedAt = nil
	sb.LastActivityAt = now
	sb.StateVersion++
	sb.UpdatedAt = now
}

// ---- postgres store ----

// stoppableStates are the states StopSandbox moves.
func stoppableStates() []string {
	return []string{string(SandboxRequested), string(SandboxScheduled), string(SandboxStarting),
		string(SandboxRunning), string(SandboxPaused)}
}

// deletableStates are every state but the two a delete already reached.
func deletableStates() []string {
	return []string{string(SandboxRequested), string(SandboxScheduled), string(SandboxStarting),
		string(SandboxRunning), string(SandboxPaused), string(SandboxStopping), string(SandboxStopped),
		string(SandboxFailed)}
}

func (p *PostgresStore) StopSandbox(ctx context.Context, id, actorSub string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	actorSub = strings.TrimSpace(actorSub)
	now := time.Now().UTC()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Sandbox{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// One statement, decided from the state the row had when it was locked: a
	// sandbox never claimed has no VM, so it is stopped at once; an active one
	// goes to stopping for its node to power off.
	var from string
	sb, err := scanSandbox(tx.QueryRow(ctx, `
		UPDATE sandboxes s SET
		    state=CASE WHEN prev.state='requested' THEN 'stopped' ELSE 'stopping' END,
		    stopped_at=CASE WHEN prev.state='requested' THEN $2::timestamptz ELSE s.stopped_at END,
		    state_version=s.state_version+1, updated_at=$2,
		    local_net_state=CASE WHEN s.local_net THEN 'withdrawn' ELSE 'off' END,
		    local_net_client_public='', local_net_grant_hash='', local_net_grant_expires_at=NULL
		FROM (SELECT id, state FROM sandboxes WHERE id=$1 FOR UPDATE) prev
		WHERE s.id = prev.id AND prev.state = ANY($3)
		RETURNING prev.state, `+sandboxColumnsS,
		id, now, stoppableStates()), &from)
	if errors.Is(err, pgx.ErrNoRows) {
		cur, gerr := p.GetSandbox(ctx, id)
		if gerr != nil {
			return Sandbox{}, gerr
		}
		if _, _, perr := stopPlan(cur.State); perr != nil {
			return Sandbox{}, perr
		}
		return cur, nil // stopped or stopping already
	}
	if err != nil {
		return Sandbox{}, err
	}
	_, reason, _ := stopPlan(SandboxState(from))
	if err := emitEventTx(ctx, tx, transitionEvent(sb, SandboxState(from), actorSub, reason)); err != nil {
		return Sandbox{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Sandbox{}, err
	}
	return sb, nil
}

func (p *PostgresStore) DeleteSandbox(ctx context.Context, id, actorSub string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	actorSub = strings.TrimSpace(actorSub)
	now := time.Now().UTC()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Sandbox{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// deletePlan, in SQL, from the state the row had when it was locked.
	var from string
	sb, err := scanSandbox(tx.QueryRow(ctx, `
		UPDATE sandboxes s SET
		    state=CASE
		        WHEN prev.state IN ('requested','failed') THEN 'deleted'
		        WHEN prev.state='stopped' AND (s.node_id IS NULL OR s.node_id='') THEN 'deleted'
		        ELSE 'deleting' END,
		    state_version=s.state_version+1, updated_at=$2,
		    local_net_state=CASE WHEN s.local_net THEN 'withdrawn' ELSE 'off' END,
		    local_net_client_public='', local_net_grant_hash='', local_net_grant_expires_at=NULL
		FROM (SELECT id, state FROM sandboxes WHERE id=$1 FOR UPDATE) prev
		WHERE s.id = prev.id AND prev.state = ANY($3)
		RETURNING prev.state, `+sandboxColumnsS,
		id, now, deletableStates()), &from)
	if errors.Is(err, pgx.ErrNoRows) {
		return p.GetSandbox(ctx, id) // deleting or deleted already, or ErrNotFound
	}
	if err != nil {
		return Sandbox{}, err
	}
	_, reason := deletePlan(Sandbox{State: SandboxState(from), NodeID: sb.NodeID})
	if err := emitEventTx(ctx, tx, transitionEvent(sb, SandboxState(from), actorSub, reason)); err != nil {
		return Sandbox{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Sandbox{}, err
	}
	return sb, nil
}

func (p *PostgresStore) ResumeSandbox(ctx context.Context, id, actorSub string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	actorSub = strings.TrimSpace(actorSub)
	now := time.Now().UTC()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Sandbox{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The same lock a create takes: the node's room is decided and used in one step.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, placementLockKey); err != nil {
		return Sandbox{}, fmt.Errorf("placement lock: %w", err)
	}
	cur, err := scanSandbox(tx.QueryRow(ctx, `SELECT `+sandboxColumns+` FROM sandboxes WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Sandbox{}, ErrNotFound
	}
	if err != nil {
		return Sandbox{}, err
	}
	resume, err := resumePlan(cur)
	if err != nil {
		return Sandbox{}, err
	}
	if !resume {
		return cur, nil
	}
	cands, err := placementCandidates(ctx, tx)
	if err != nil {
		return Sandbox{}, fmt.Errorf("placement candidates: %w", err)
	}
	if _, err := sched.Place(p.schedCfg, resumeRequest(cur), cands, now); err != nil {
		return Sandbox{}, err
	}
	sb, err := scanSandbox(tx.QueryRow(ctx, `
		UPDATE sandboxes SET state='requested', boot_count=boot_count+1, stop_reason='', status_detail='',
		    stopped_at=NULL, last_activity_at=$2, state_version=state_version+1, updated_at=$2
		WHERE id=$1
		RETURNING `+sandboxColumns, id, now))
	if err != nil {
		return Sandbox{}, err
	}
	if err := emitEventTx(ctx, tx, resumedEvent(sb, actorSub)); err != nil {
		return Sandbox{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Sandbox{}, err
	}
	return sb, nil
}
