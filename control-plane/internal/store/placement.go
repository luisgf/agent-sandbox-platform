package store

import (
	"sort"
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

// occupyingStates hold capacity on their node: from placement until the node says
// the VM is gone. That includes deleting: DELETE only asks the node to remove the
// VM, and the node does it after its next poll and the stop grace; the capacity
// is released when it reports deleted, not before, or a create racing the delete
// could overcommit the node. (Deleting a stopped sandbox, which has no VM, also
// holds its size for the second the node takes to remove the disk.)
var occupyingStates = []SandboxState{
	SandboxRequested, SandboxScheduled, SandboxStarting, SandboxRunning, SandboxPaused, SandboxStopping, SandboxDeleting,
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

// NodeWork is what a node agent gets on each poll: the sandboxes that need
// action (NeedsNodeAction) and the ids of every sandbox assigned to the node
// (OccupiesNode). A VM the node runs that is not in Assigned was failed over,
// destroyed or never placed there: the node stops it.
type NodeWork struct {
	Sandboxes []Sandbox
	Assigned  []string
	// Retained are the stopped sandboxes on the node: their disks stay. Never nil
	// in a response, so a node can tell a control plane that has the list from
	// one that predates it (ADR-0012).
	Retained []string
	// Tenants maps every assigned sandbox to its tenant (for its egress policy).
	Tenants map[string]string
}

// NeedsNodeAction reports whether a sandbox on a node needs its node agent to
// act: claim and start it, stop it, delete it (the VM, if any, and the disk), or follow a running full-tunnel
// local-net session (pending → up → withdrawn).
func NeedsNodeAction(sb Sandbox) bool {
	switch sb.State {
	case SandboxRequested, SandboxStarting, SandboxStopping, SandboxDeleting:
		return true
	case SandboxRunning:
		return sb.LocalNet
	}
	return false
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

// add files a sandbox of the node under what the node is told about it: held
// capacity (assigned), work to do, or a disk to keep (retained).
func (w *NodeWork) add(sb Sandbox) {
	switch {
	case OccupiesNode(sb.State):
		w.Assigned = append(w.Assigned, sb.ID)
		w.Tenants[sb.ID] = sb.TenantID
		if NeedsNodeAction(sb) {
			w.Sandboxes = append(w.Sandboxes, sb)
		}
	case sb.State == SandboxStopped:
		w.Retained = append(w.Retained, sb.ID)
	}
}

func (w *NodeWork) sortWork() {
	sort.Slice(w.Sandboxes, func(i, j int) bool { return w.Sandboxes[i].CreatedAt.Before(w.Sandboxes[j].CreatedAt) })
	sort.Strings(w.Assigned)
	sort.Strings(w.Retained)
}

// recordsDetail: the detail a node sends is kept for a start that could not
// finish: failed, or a resume that went back to stopped. A plain stop's detail
// ("vmm stopped") says nothing worth keeping.
func recordsDetail(prev, to SandboxState) bool {
	switch to {
	case SandboxFailed:
		return true
	case SandboxStopped:
		return prev == SandboxRequested || prev == SandboxStarting
	}
	return false
}
