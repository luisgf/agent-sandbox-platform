package store

import (
	"strings"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/sched"
)

// Placement (ADR-0011): CreateSandbox picks the node with internal/sched inside the
// store's own lock (memory) or transaction (Postgres), so concurrent creates never
// overfill a node. The API maps these errors to HTTP 503 and 409.
var (
	ErrNoCapacity      = sched.ErrNoCapacity
	ErrNodeUnavailable = sched.ErrNodeUnavailable
)

type (
	NoCapacityError      = sched.NoCapacityError
	NodeUnavailableError = sched.NodeUnavailableError
)

// occupyingStates hold capacity on their node: from placement until the VM is gone.
var occupyingStates = []SandboxState{
	SandboxRequested, SandboxScheduled, SandboxStarting, SandboxRunning, SandboxPaused, SandboxStopping,
}

// OccupiesNode reports whether a sandbox in state counts against its node's capacity.
func OccupiesNode(state SandboxState) bool {
	for _, s := range occupyingStates {
		if s == state {
			return true
		}
	}
	return false
}

func occupyingStateNames() []string {
	out := make([]string, len(occupyingStates))
	for i, s := range occupyingStates {
		out[i] = string(s)
	}
	return out
}

// NodeUsage is what is placed on a node.
type NodeUsage struct {
	CPUMillis int64 `json:"cpu_millis"`
	MemoryMiB int64 `json:"memory_mib"`
	Sandboxes int64 `json:"sandboxes"`
}

func (u *NodeUsage) add(sb Sandbox) {
	u.CPUMillis += int64(sb.CPUMillis)
	u.MemoryMiB += int64(sb.MemoryMiB)
	u.Sandboxes++
}

// EffectiveAgentEndpoint is where exec goes: agent_endpoint, else endpoint.
func EffectiveAgentEndpoint(n Node) string {
	if ep := strings.TrimSpace(n.AgentEndpoint); ep != "" {
		return ep
	}
	return strings.TrimSpace(n.Endpoint)
}

// Candidate converts a node and its usage for the scheduler.
func Candidate(n Node, u NodeUsage) sched.Candidate {
	c := sched.Candidate{
		ID:            n.ID,
		AgentEndpoint: EffectiveAgentEndpoint(n),
		State:         n.State,
		Revoked:       n.RevokedAt != nil,
		Cordoned:      n.Cordoned,
		AcceptsWork:   n.AcceptsWork,
		VMMProfiles:   n.VMMProfiles,
		CapCPUCores:   n.CapacityCPU,
		CapMemMiB:     n.CapacityMemMiB,
		MaxSandboxes:  n.MaxSandboxes,
		UsedCPUMillis: u.CPUMillis,
		UsedMemMiB:    u.MemoryMiB,
		UsedSandboxes: u.Sandboxes,
	}
	if n.LastSeenAt != nil {
		c.LastSeen = *n.LastSeenAt
	}
	return c
}

func placementRequest(input CreateSandboxInput, vmmProfile string) sched.Request {
	return sched.Request{
		CPUMillis:    input.CPUMillis,
		MemoryMiB:    input.MemoryMiB,
		VMMProfile:   vmmProfile,
		PinnedNodeID: strings.TrimSpace(input.NodeID),
	}
}

func placedEventPayload(nodeID string, cfg sched.Config, input CreateSandboxInput) []byte {
	return mustJSON(map[string]any{
		"node_id": nodeID,
		"policy":  string(cfg.Policy),
		"pinned":  strings.TrimSpace(input.NodeID) != "",
	})
}
