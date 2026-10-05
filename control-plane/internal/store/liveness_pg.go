package store

import (
	"context"
	"errors"
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
		    state = CASE WHEN v.state='stopping' THEN 'stopped' ELSE 'failed' END,
		    state_version = s.state_version+1, updated_at=$3, node_lease_until=NULL, stop_reason=$4,
		    local_net_state=CASE WHEN s.local_net THEN 'withdrawn' ELSE 'off' END,
		    local_net_client_public='', local_net_grant_hash='', local_net_grant_expires_at=NULL
		FROM victims v WHERE s.id = v.id
		RETURNING s.id, s.tenant_id, v.state, s.state`,
		nodeID, silentSince, now, reason, occupyingStateNames())
	if err != nil {
		return nil, err
	}
	type lostRow struct{ id, tenant, from, to string }
	var lost []lostRow
	for rows.Next() {
		var r lostRow
		if err := rows.Scan(&r.id, &r.tenant, &r.from, &r.to); err != nil {
			rows.Close()
			return nil, err
		}
		lost = append(lost, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range lost {
		from := r.from
		if err := emitEventTx(ctx, tx, EmitEventInput{
			SandboxID: r.id,
			TenantID:  r.tenant,
			EventType: "sandbox.node_lost",
			FromState: &from,
			ToState:   strPtr(r.to),
			Actor:     "node-monitor",
			Payload:   mustJSON(map[string]string{"node_id": nodeID, "reason": reason}),
		}); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return readBackSandboxes(p, lost, func(r lostRow) string { return r.id })
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
		    stop_reason=$3, node_lease_until=NULL,
		    local_net_state=CASE WHEN local_net THEN 'withdrawn' ELSE 'off' END,
		    local_net_client_public='', local_net_grant_hash='', local_net_grant_expires_at=NULL
		WHERE state='requested' AND (node_id IS NULL OR node_id='') AND created_at < $1
		RETURNING id, tenant_id`, createdBefore, now, reason)
	if err != nil {
		return nil, err
	}
	type row struct{ id, tenant string }
	var failed []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.tenant); err != nil {
			rows.Close()
			return nil, err
		}
		failed = append(failed, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range failed {
		from := string(SandboxRequested)
		if err := emitEventTx(ctx, tx, EmitEventInput{
			SandboxID: r.id,
			TenantID:  r.tenant,
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
	return readBackSandboxes(p, failed, func(r row) string { return r.id })
}

// readBackSandboxes reads back the rows a bulk update changed.
func readBackSandboxes[T any](p *PostgresStore, items []T, id func(T) string) ([]Sandbox, error) {
	out := make([]Sandbox, 0, len(items))
	for _, it := range items {
		sb, err := p.GetSandbox(id(it))
		if err != nil {
			if errors.Is(err, ErrNotFound) || errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return nil, err
		}
		out = append(out, sb)
	}
	return out, nil
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
