package store

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Retention of stopped sandboxes (ADR-0012). A stopped sandbox keeps its disk on
// its node. Two limits bound how long and how many: a TTL counted from stopped_at
// and a cap per tenant. Hitting either deletes the sandbox, with its disk.
const (
	EnvStoppedSandboxTTL     = "ASP_STOPPED_SANDBOX_TTL"
	EnvMaxStoppedPerTenant   = "ASP_MAX_STOPPED_PER_TENANT"
	EnvRetentionSweep        = "ASP_RETENTION_SWEEP"
	DefaultStoppedSandboxTTL = 7 * 24 * time.Hour

	// StopReasonRetention marks a sandbox deleted because it was stopped longer
	// than the TTL; StopReasonTenantCap one deleted to keep its tenant under the cap.
	StopReasonRetention = "retention_expired"
	StopReasonTenantCap = "tenant_cap"
)

// ParseRetentionTTL parses ASP_STOPPED_SANDBOX_TTL. Empty is the default (7
// days). 0, off, false, disabled, no and none keep stopped sandboxes until they
// are deleted. A Go duration, or whole days as "7d".
func ParseRetentionTTL(raw string) (time.Duration, error) {
	v := strings.TrimSpace(strings.ToLower(raw))
	switch v {
	case "":
		return DefaultStoppedSandboxTTL, nil
	case "0", "0s", "off", "false", "disabled", "no", "none":
		return 0, nil
	}
	var d time.Duration
	if days, ok := strings.CutSuffix(v, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("%w: %s %q: want a duration such as 7d or 48h", ErrInvalidInput, EnvStoppedSandboxTTL, raw)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(v); err != nil {
			return 0, fmt.Errorf("%w: %s %q: want a duration such as 7d or 48h", ErrInvalidInput, EnvStoppedSandboxTTL, raw)
		}
	}
	if d < 0 {
		return 0, fmt.Errorf("%w: %s must be >= 0", ErrInvalidInput, EnvStoppedSandboxTTL)
	}
	return d, nil
}

// ParseMaxStoppedPerTenant parses ASP_MAX_STOPPED_PER_TENANT: empty or 0 is no cap.
func ParseMaxStoppedPerTenant(raw string) (int, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%w: %s %q: want a whole number, 0 for no cap", ErrInvalidInput, EnvMaxStoppedPerTenant, raw)
	}
	return n, nil
}

// retentionDeleted is where a sandbox that retention deletes goes: its node has
// its disk, so the node deletes it; with no node there is nothing to remove.
func retentionDeleted(sb Sandbox) SandboxState {
	if sb.NodeID == nil || *sb.NodeID == "" {
		return SandboxDeleted
	}
	return SandboxDeleting
}

// stoppedSince is when the sandbox stopped; a row without stopped_at counts from
// its last update, so it is neither kept for ever nor deleted at once.
func stoppedSince(sb Sandbox) time.Time {
	if sb.StoppedAt != nil {
		return *sb.StoppedAt
	}
	return sb.UpdatedAt
}

func retentionEvent(sb Sandbox, reason string, extra map[string]any) EmitEventInput {
	from := string(SandboxStopped)
	payload := map[string]any{"reason": reason}
	for k, v := range extra {
		payload[k] = v
	}
	return EmitEventInput{
		SandboxID: sb.ID,
		TenantID:  sb.TenantID,
		EventType: "sandbox.deleted",
		FromState: &from,
		ToState:   strPtr(string(sb.State)),
		Actor:     "retention-reaper",
		Payload:   mustJSON(payload),
	}
}

func (m *MemoryStore) retainDelete(sb Sandbox, reason string, now time.Time) Sandbox {
	sb.State = retentionDeleted(sb)
	sb.StopReason = reason
	withdrawLocalNetFields(&sb)
	sb.StateVersion++
	sb.UpdatedAt = now
	m.sandboxes[sb.ID] = sb
	return cloneSandbox(sb)
}

func (m *MemoryStore) ExpireStoppedSandboxes(now time.Time, ttl time.Duration) ([]Sandbox, error) {
	if ttl <= 0 {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	m.mu.Lock()
	var out []Sandbox
	for _, sb := range m.sandboxes {
		if sb.State != SandboxStopped || stoppedSince(sb).After(now.Add(-ttl)) {
			continue
		}
		out = append(out, m.retainDelete(sb, StopReasonRetention, now))
	}
	m.mu.Unlock()
	for _, sb := range out {
		_ = m.EmitEvent(retentionEvent(sb, StopReasonRetention, map[string]any{"ttl": ttl.String()}))
	}
	sortByID(out)
	return out, nil
}

func (m *MemoryStore) EvictStoppedOverCap(max int) ([]Sandbox, error) {
	if max <= 0 {
		return nil, nil
	}
	now := time.Now().UTC()
	m.mu.Lock()
	byTenant := map[string][]Sandbox{}
	for _, sb := range m.sandboxes {
		if sb.State == SandboxStopped {
			byTenant[sb.TenantID] = append(byTenant[sb.TenantID], sb)
		}
	}
	var out []Sandbox
	for _, list := range byTenant {
		if len(list) <= max {
			continue
		}
		sortOldestStoppedFirst(list)
		for _, sb := range list[:len(list)-max] {
			out = append(out, m.retainDelete(sb, StopReasonTenantCap, now))
		}
	}
	m.mu.Unlock()
	for _, sb := range out {
		_ = m.EmitEvent(retentionEvent(sb, StopReasonTenantCap, map[string]any{"max_stopped_per_tenant": max}))
	}
	sortByID(out)
	return out, nil
}

// ---- postgres ----

func (p *PostgresStore) ExpireStoppedSandboxes(now time.Time, ttl time.Duration) ([]Sandbox, error) {
	if ttl <= 0 {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return p.retentionSweep(`
		UPDATE sandboxes s SET
		    state=CASE WHEN s.node_id IS NULL OR s.node_id='' THEN 'deleted' ELSE 'deleting' END,
		    stop_reason=$3, state_version=s.state_version+1, updated_at=$2,
		    local_net_state=CASE WHEN s.local_net THEN 'withdrawn' ELSE 'off' END,
		    local_net_client_public='', local_net_grant_hash='', local_net_grant_expires_at=NULL
		WHERE s.state='stopped' AND COALESCE(s.stopped_at, s.updated_at) <= $1
		RETURNING 'stopped'::text, `+sandboxColumnsS,
		StopReasonRetention, map[string]any{"ttl": ttl.String()}, now.Add(-ttl), now, StopReasonRetention)
}

func (p *PostgresStore) EvictStoppedOverCap(max int) ([]Sandbox, error) {
	if max <= 0 {
		return nil, nil
	}
	now := time.Now().UTC()
	return p.retentionSweep(`
		WITH ranked AS (
		    SELECT id,
		           row_number() OVER (PARTITION BY tenant_id ORDER BY COALESCE(stopped_at, updated_at), id) AS rn,
		           count(*) OVER (PARTITION BY tenant_id) AS n
		    FROM sandboxes WHERE state='stopped'
		), victims AS (SELECT id FROM ranked WHERE rn <= n - $1)
		UPDATE sandboxes s SET
		    state=CASE WHEN s.node_id IS NULL OR s.node_id='' THEN 'deleted' ELSE 'deleting' END,
		    stop_reason=$3, state_version=s.state_version+1, updated_at=$2,
		    local_net_state=CASE WHEN s.local_net THEN 'withdrawn' ELSE 'off' END,
		    local_net_client_public='', local_net_grant_hash='', local_net_grant_expires_at=NULL
		FROM victims v WHERE s.id = v.id AND s.state='stopped'
		RETURNING 'stopped'::text, `+sandboxColumnsS,
		StopReasonTenantCap, map[string]any{"max_stopped_per_tenant": max}, max, now, StopReasonTenantCap)
}

// retentionSweep runs one retention UPDATE and its events in one transaction.
// args follow the query's own $1…: the caller names the reason and event extras first.
func (p *PostgresStore) retentionSweep(query, reason string, extra map[string]any, args ...any) ([]Sandbox, error) {
	ctx := context.Background()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	out, _, err := collectChanged(rows)
	if err != nil {
		return nil, err
	}
	for _, sb := range out {
		if err := emitEventTx(ctx, tx, retentionEvent(sb, reason, extra)); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	sortByID(out)
	return out, nil
}

// IsMemory reports whether st keeps its state in process memory, so a restart of
// the control plane forgets every sandbox.
func IsMemory(st Store) bool {
	_, ok := st.(*MemoryStore)
	return ok
}

func sortByID(list []Sandbox) {
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
}

func sortOldestStoppedFirst(list []Sandbox) {
	sort.Slice(list, func(i, j int) bool {
		a, b := stoppedSince(list[i]), stoppedSince(list[j])
		if !a.Equal(b) {
			return a.Before(b)
		}
		return list[i].ID < list[j].ID
	})
}

// SetStoppedAtForTest pins stopped_at (unit tests only).
func (m *MemoryStore) SetStoppedAtForTest(id string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sb, ok := m.sandboxes[id]; ok {
		at = at.UTC()
		sb.StoppedAt = &at
		m.sandboxes[id] = sb
	}
}
