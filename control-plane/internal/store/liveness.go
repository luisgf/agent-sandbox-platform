package store

import (
	"sort"
	"time"
)

// Node liveness and failover (ADR-0011). The control plane's node monitor marks
// silent nodes offline and, after the failover delay, fails their sandboxes. A
// sandbox's disk lives on its node, so failover means failing, not moving.
const (
	StopReasonNodeLost    = "node_lost"
	StopReasonUnscheduled = "unscheduled"

	NodeLostMessage = "sandbox was lost with its node; start a new sandbox (asp session start --force)"
)

// nodeLost: revoked, or not seen since silentSince.
func nodeLost(n Node, silentSince time.Time) bool {
	return n.RevokedAt != nil || n.LastSeenAt == nil || n.LastSeenAt.Before(silentSince)
}

func (m *MemoryStore) MarkNodeOffline(id string, silentSince time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[id]
	if !ok {
		return false, ErrNotFound
	}
	if n.State == "offline" || n.RevokedAt != nil || !nodeLost(n, silentSince) {
		return false, nil
	}
	n.State = "offline"
	n.UpdatedAt = time.Now().UTC()
	m.nodes[id] = n
	return true, nil
}

type lostSandbox struct {
	id, tenant string
	from, to   SandboxState
}

func (m *MemoryStore) FailNodeSandboxes(nodeID, reason string, silentSince time.Time) ([]Sandbox, error) {
	m.mu.Lock()
	n, ok := m.nodes[nodeID]
	if !ok {
		m.mu.Unlock()
		return nil, ErrNotFound
	}
	if !nodeLost(n, silentSince) {
		m.mu.Unlock()
		return nil, nil
	}
	now := time.Now().UTC()
	var out []Sandbox
	var lost []lostSandbox
	for id, sb := range m.sandboxes {
		if sb.NodeID == nil || *sb.NodeID != nodeID || !OccupiesNode(sb.State) {
			continue
		}
		from := sb.State
		to := SandboxFailed
		if from == SandboxStopping {
			to = SandboxStopped
		}
		sb.State = to
		withdrawLocalNetFields(&sb)
		sb.StopReason = reason
		sb.NodeLeaseUntil = nil
		sb.StateVersion++
		sb.UpdatedAt = now
		m.sandboxes[id] = sb
		out = append(out, cloneSandbox(sb))
		lost = append(lost, lostSandbox{id: id, tenant: sb.TenantID, from: from, to: to})
	}
	m.mu.Unlock()
	for _, l := range lost {
		from := string(l.from)
		_ = m.EmitEvent(EmitEventInput{
			SandboxID: l.id,
			TenantID:  l.tenant,
			EventType: "sandbox.node_lost",
			FromState: &from,
			ToState:   strPtr(string(l.to)),
			Actor:     "node-monitor",
			Payload:   mustJSON(map[string]string{"node_id": nodeID, "reason": reason}),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemoryStore) FailUnassignedRequested(createdBefore time.Time, reason string) ([]Sandbox, error) {
	m.mu.Lock()
	now := time.Now().UTC()
	var out []Sandbox
	for id, sb := range m.sandboxes {
		if sb.State != SandboxRequested || (sb.NodeID != nil && *sb.NodeID != "") || !sb.CreatedAt.Before(createdBefore) {
			continue
		}
		sb.State = SandboxFailed
		withdrawLocalNetFields(&sb)
		sb.StopReason = reason
		sb.StateVersion++
		sb.UpdatedAt = now
		m.sandboxes[id] = sb
		out = append(out, cloneSandbox(sb))
	}
	m.mu.Unlock()
	for _, sb := range out {
		from := string(SandboxRequested)
		_ = m.EmitEvent(EmitEventInput{
			SandboxID: sb.ID,
			TenantID:  sb.TenantID,
			EventType: "sandbox.unscheduled",
			FromState: &from,
			ToState:   strPtr(string(SandboxFailed)),
			Actor:     "node-monitor",
			Payload:   mustJSON(map[string]string{"reason": reason}),
		})
	}
	return out, nil
}

// EmitNodeEvent: the memory store keeps no node events.
func (m *MemoryStore) EmitNodeEvent(string, string, string, map[string]any) error { return nil }

// SetNodeLastSeenForTest moves a node's last_seen_at (tests only).
func (m *MemoryStore) SetNodeLastSeenForTest(id string, t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n, ok := m.nodes[id]; ok {
		n.LastSeenAt = &t
		m.nodes[id] = n
	}
}

// UnassignForTest clears a sandbox's node, like rows from before placement (tests only).
func (m *MemoryStore) UnassignForTest(id string, createdAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sb, ok := m.sandboxes[id]; ok {
		sb.NodeID = nil
		sb.CreatedAt = createdAt
		m.sandboxes[id] = sb
	}
}
