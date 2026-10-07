package store

import (
	"context"
	"sort"
	"time"
)

// Node liveness and failover (ADR-0011). The control plane's node monitor marks
// silent nodes offline and, after the failover delay, fails their sandboxes. A
// sandbox's disk lives on its node, so failover means failing, not moving.
const (
	StopReasonNodeLost       = "node_lost"
	StopReasonUnscheduled    = "unscheduled"
	StopReasonAgentRestarted = "node_agent_restarted"

	// NodeLostMessage is the advice for a failed sandbox whose disk went with its
	// node. A stopped one keeps its disk: see LostAdvice.
	NodeLostMessage = "sandbox was lost with its node; start a new sandbox (asp session start --force)"
)

// LostAdvice is what to tell the caller of a sandbox whose node was lost or whose
// agent restarted, by what became of it. A failed sandbox has no disk to come
// back to; a stopped one (its VM died with the agent, or a stop was under way)
// has its disk kept on its node and can be resumed, so sending it to
// "start --force" would delete the very thing that survived.
func LostAdvice(state SandboxState, reason, nodeID string) string {
	if state != SandboxStopped && state != SandboxStopping {
		return NodeLostMessage
	}
	node := "its node"
	if nodeID != "" {
		node = "node " + nodeID
	}
	if reason == StopReasonNodeLost {
		return "sandbox was stopped when its node stopped responding; its disk is kept on " + node +
			": resume it once the node is back (asp session resume), or delete it (asp session rm)"
	}
	return "sandbox was stopped when the node agent restarted; its disk is kept on " + node +
		": resume it (asp session resume), or delete it (asp session rm)"
}

// nodeLost: revoked, or not seen since silentSince.
func nodeLost(n Node, silentSince time.Time) bool {
	return n.RevokedAt != nil || n.LastSeenAt == nil || n.LastSeenAt.Before(silentSince)
}

func (m *MemoryStore) MarkNodeOffline(ctx context.Context, id string, silentSince time.Time) (bool, error) {
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

func (m *MemoryStore) FailNodeSandboxes(ctx context.Context, nodeID, reason string, silentSince time.Time) ([]Sandbox, error) {
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
		switch from {
		case SandboxStopping:
			to = SandboxStopped
			sb.StoppedAt = &now
		case SandboxDeleting: // the disk went with the node
			to = SandboxDeleted
		}
		sb.State = to
		withdrawLocalNetFields(&sb)
		sb.StopReason = reason
		sb.StateVersion++
		sb.UpdatedAt = now
		m.sandboxes[id] = sb
		out = append(out, cloneSandbox(sb))
		lost = append(lost, lostSandbox{id: id, tenant: sb.TenantID, from: from, to: to})
	}
	m.mu.Unlock()
	for _, l := range lost {
		from := string(l.from)
		_ = m.EmitEvent(ctx, EmitEventInput{
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

func (m *MemoryStore) FailUnassignedRequested(ctx context.Context, createdBefore time.Time, reason string) ([]Sandbox, error) {
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
		_ = m.EmitEvent(ctx, EmitEventInput{
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

// agentRestarted: both ids known and different.
func agentRestarted(prev, next string) bool {
	return prev != "" && next != "" && prev != next
}

// restartOrphanTarget is where a sandbox goes when its agent restarted: the VM of
// a running or paused one is gone with the old process, but its disk is intact on
// the node, so it becomes stopped and can be resumed (a failed sandbox would be
// in neither list /work sends, and the node's disk GC would remove its disk);
// stopping finishes as stopped; requested and starting are left for the new
// process to (re)boot, a resume included, since it reuses the retained disk.
func restartOrphanTarget(state SandboxState) (SandboxState, bool) {
	switch state {
	case SandboxRunning, SandboxPaused, SandboxStopping:
		return SandboxStopped, true
	}
	return "", false
}

// EmitNodeEvent: the memory store keeps no node events, but an event of a node that
// does not exist is refused, as Postgres's foreign key does.
func (m *MemoryStore) EmitNodeEvent(_ context.Context, nodeID, _, _ string, _ map[string]any) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.nodes[nodeID]; !ok {
		return ErrNotFound
	}
	return nil
}

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
