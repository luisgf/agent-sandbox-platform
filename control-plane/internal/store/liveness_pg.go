package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

func (p *PostgresStore) MarkNodeOffline(id string, silentSince time.Time) (bool, error) {
	ctx := context.Background()
	now := time.Now().UTC()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE nodes SET state='offline', updated_at=$3
		WHERE id=$1 AND state<>'offline' AND revoked_at IS NULL
		  AND (last_seen_at IS NULL OR last_seen_at < $2)`, id, silentSince, now)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM nodes WHERE id=$1)`, id).Scan(&exists); err != nil {
			return false, err
		}
		if !exists {
			return false, ErrNotFound
		}
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO node_events (node_id, event_type, actor, payload)
		VALUES ($1,'node.offline','node-monitor',$2)`,
		id, mustJSON(map[string]any{"silent_since": silentSince})); err != nil {
		return false, fmt.Errorf("node event: %w", err)
	}
	return true, tx.Commit(ctx)
}

func (p *PostgresStore) FailNodeSandboxes(nodeID, reason string, silentSince time.Time) ([]Sandbox, error) {
	ctx := context.Background()
	now := time.Now().UTC()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM nodes WHERE id=$1)`, nodeID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	// The node must still be lost when the rows are locked: a heartbeat that lands
	// first keeps the sandboxes.
	rows, err := tx.Query(ctx, `
		WITH lost AS (
		    SELECT 1 FROM nodes
		    WHERE id=$1 AND (revoked_at IS NOT NULL OR last_seen_at IS NULL OR last_seen_at < $2)
		), victims AS (
		    SELECT id, state FROM sandboxes
		    WHERE node_id=$1 AND state = ANY($5) AND EXISTS (SELECT 1 FROM lost)
		    FOR UPDATE
		)
		UPDATE sandboxes s SET
		    state = CASE WHEN v.state='stopping' THEN 'stopped' WHEN v.state='deleting' THEN 'deleted' ELSE 'failed' END,
		    stopped_at = CASE WHEN v.state='stopping' THEN $3::timestamptz ELSE s.stopped_at END,
		    state_version = s.state_version+1, updated_at=$3, stop_reason=$4,
		    local_net_state=CASE WHEN s.local_net THEN 'withdrawn' ELSE 'off' END,
		    local_net_client_public='', local_net_grant_hash='', local_net_grant_expires_at=NULL
		FROM victims v WHERE s.id = v.id
		RETURNING v.state, `+sandboxColumnsS,
		nodeID, silentSince, now, reason, occupyingStateNames())
	if err != nil {
		return nil, err
	}
	lost, froms, err := collectChanged(rows)
	if err != nil {
		return nil, err
	}
	for i, sb := range lost {
		if err := emitEventTx(ctx, tx, EmitEventInput{
			SandboxID: sb.ID,
			TenantID:  sb.TenantID,
			EventType: "sandbox.node_lost",
			FromState: &froms[i],
			ToState:   strPtr(string(sb.State)),
			Actor:     "node-monitor",
			Payload:   mustJSON(map[string]string{"node_id": nodeID, "reason": reason}),
		}); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return lost, nil
}

func (p *PostgresStore) FailUnassignedRequested(createdBefore time.Time, reason string) ([]Sandbox, error) {
	ctx := context.Background()
	now := time.Now().UTC()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		UPDATE sandboxes SET state='failed', state_version=state_version+1, updated_at=$2,
		    stop_reason=$3,
		    local_net_state=CASE WHEN local_net THEN 'withdrawn' ELSE 'off' END,
		    local_net_client_public='', local_net_grant_hash='', local_net_grant_expires_at=NULL
		WHERE state='requested' AND (node_id IS NULL OR node_id='') AND created_at < $1
		RETURNING 'requested'::text, `+sandboxColumns, createdBefore, now, reason)
	if err != nil {
		return nil, err
	}
	failed, _, err := collectChanged(rows)
	if err != nil {
		return nil, err
	}
	for _, sb := range failed {
		from := string(SandboxRequested)
		if err := emitEventTx(ctx, tx, EmitEventInput{
			SandboxID: sb.ID,
			TenantID:  sb.TenantID,
			EventType: "sandbox.unscheduled",
			FromState: &from,
			ToState:   strPtr(string(SandboxFailed)),
			Actor:     "node-monitor",
			Payload:   mustJSON(map[string]string{"reason": reason}),
		}); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return failed, nil
}

// collectChanged reads the rows a bulk update returned as
// "<previous state>, <sandboxColumns>". The rows are consumed before the
// caller writes events in the same transaction.
func collectChanged(rows pgx.Rows) ([]Sandbox, []string, error) {
	defer rows.Close()
	out := []Sandbox{}
	var froms []string
	for rows.Next() {
		var from string
		sb, err := scanSandbox(rows, &from)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, sb)
		froms = append(froms, from)
	}
	return out, froms, rows.Err()
}

// recordNodeOnline notes a node that was offline and is seen again.
func (p *PostgresStore) recordNodeOnline(ctx context.Context, id, prevState string) {
	if prevState != "offline" {
		return
	}
	_, _ = p.pool.Exec(ctx, `
		INSERT INTO node_events (node_id, event_type, actor, payload)
		VALUES ($1,'node.online','node-agent','{}'::jsonb)`, id)
}

func (p *PostgresStore) EmitNodeEvent(nodeID, eventType, actor string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	_, err := p.pool.Exec(context.Background(), `
		INSERT INTO node_events (node_id, event_type, actor, payload)
		VALUES ($1,$2,$3,$4)`, nodeID, eventType, actor, mustJSON(payload))
	return err
}

// failRestartOrphansTx fails, inside the register transaction, the sandboxes a
// restarted agent lost track of (see restartOrphanTarget).
func failRestartOrphansTx(ctx context.Context, tx pgx.Tx, nodeID string, now time.Time) error {
	rows, err := tx.Query(ctx, `
		WITH victims AS (
		    SELECT id, state FROM sandboxes
		    WHERE node_id=$1 AND state IN ('running','paused','stopping')
		    FOR UPDATE
		)
		UPDATE sandboxes s SET
		    state = 'stopped', stopped_at = $2::timestamptz,
		    state_version = s.state_version+1, updated_at=$2, stop_reason=$3,
		    local_net_state=CASE WHEN s.local_net THEN 'withdrawn' ELSE 'off' END,
		    local_net_client_public='', local_net_grant_hash='', local_net_grant_expires_at=NULL
		FROM victims v WHERE s.id = v.id
		RETURNING s.id, s.tenant_id, v.state, s.state`, nodeID, now, StopReasonAgentRestarted)
	if err != nil {
		return err
	}
	type row struct{ id, tenant, from, to string }
	var orphaned []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.tenant, &r.from, &r.to); err != nil {
			rows.Close()
			return err
		}
		orphaned = append(orphaned, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range orphaned {
		from := r.from
		if err := emitEventTx(ctx, tx, EmitEventInput{
			SandboxID: r.id,
			TenantID:  r.tenant,
			EventType: "sandbox.agent_restarted",
			FromState: &from,
			ToState:   strPtr(r.to),
			Actor:     "node-agent",
			Payload:   mustJSON(map[string]string{"node_id": nodeID, "reason": StopReasonAgentRestarted}),
		}); err != nil {
			return err
		}
	}
	return nil
}
