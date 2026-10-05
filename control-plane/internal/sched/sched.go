// Package sched places sandboxes on nodes (ADR-0011). It is pure: the stores pass
// the candidate nodes with their current allocation, inside their own lock or
// transaction, and get back a node id or a typed error that explains the refusal.
package sched

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Policy chooses among the nodes that fit.
type Policy string

const (
	// PolicySpread picks the node that is least loaded after placing the sandbox.
	PolicySpread Policy = "spread"
	// PolicyBinpack picks the most loaded node that still fits, keeping others free.
	PolicyBinpack Policy = "binpack"
)

const (
	EnvPolicy        = "ASP_SCHED_POLICY"
	EnvCPUOvercommit = "ASP_SCHED_CPU_OVERCOMMIT"
	EnvStaleAfter    = "ASP_NODE_STALE_AFTER"
)

// Config is the scheduler configuration (control-plane env).
type Config struct {
	Policy Policy
	// CPUOvercommit is how many vCPU millicores a node offers per physical
	// millicore. Agent sandboxes mostly wait on the model, so the default is 4.
	// Memory is never overcommitted.
	CPUOvercommit float64
	// StaleAfter: a node not seen for longer is not placed on.
	StaleAfter time.Duration
}

// DefaultConfig: spread, CPU overcommit 4, stale after 90s (three missed heartbeats).
func DefaultConfig() Config {
	return Config{Policy: PolicySpread, CPUOvercommit: 4, StaleAfter: 90 * time.Second}
}

// ConfigFromEnv reads ASP_SCHED_POLICY, ASP_SCHED_CPU_OVERCOMMIT and ASP_NODE_STALE_AFTER.
func ConfigFromEnv() (Config, error) {
	cfg := DefaultConfig()
	if v := strings.TrimSpace(os.Getenv(EnvPolicy)); v != "" {
		switch Policy(strings.ToLower(v)) {
		case PolicySpread, PolicyBinpack:
			cfg.Policy = Policy(strings.ToLower(v))
		default:
			return Config{}, fmt.Errorf("%s=%q: want spread or binpack", EnvPolicy, v)
		}
	}
	if v := strings.TrimSpace(os.Getenv(EnvCPUOvercommit)); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 {
			return Config{}, fmt.Errorf("%s=%q: want a number greater than 0", EnvCPUOvercommit, v)
		}
		cfg.CPUOvercommit = f
	}
	if v := strings.TrimSpace(os.Getenv(EnvStaleAfter)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("%s=%q: want a duration greater than 0 (e.g. 90s)", EnvStaleAfter, v)
		}
		cfg.StaleAfter = d
	}
	return cfg, nil
}

// Candidate is a node as the scheduler sees it, with what is already placed on it.
type Candidate struct {
	ID string
	// AgentEndpoint is where exec goes; empty or local:// (stub) cannot run sandboxes.
	AgentEndpoint string
	State         string
	Revoked       bool
	Cordoned      bool
	AcceptsWork   bool
	LastSeen      time.Time // zero: never seen
	VMMProfiles   []string  // empty: no restriction

	// Capacity; 0 means that dimension is not enforced.
	CapCPUCores  int
	CapMemMiB    int
	MaxSandboxes int

	UsedCPUMillis int64
	UsedMemMiB    int64
	UsedSandboxes int64
}

// Request is the sandbox to place.
type Request struct {
	CPUMillis    int
	MemoryMiB    int
	VMMProfile   string
	PinnedNodeID string
}

// Reason says why a node cannot take the sandbox.
type Reason string

const (
	ReasonUnknownNode      Reason = "unknown_node"
	ReasonNoAgentEndpoint  Reason = "no_agent_endpoint"
	ReasonRevoked          Reason = "revoked"
	ReasonOffline          Reason = "offline"
	ReasonStale            Reason = "stale"
	ReasonNotAcceptingWork Reason = "not_accepting_work"
	ReasonCordoned         Reason = "cordoned"
	ReasonVMMProfile       Reason = "vmm_profile"
	ReasonInsufficientCPU  Reason = "insufficient_cpu"
	ReasonInsufficientMem  Reason = "insufficient_memory"
	ReasonMaxSandboxes     Reason = "max_sandboxes"
)

// capacity reports whether waiting for sandboxes to finish could clear the reason.
func (r Reason) capacity() bool {
	return r == ReasonInsufficientCPU || r == ReasonInsufficientMem || r == ReasonMaxSandboxes
}

var (
	// ErrNoCapacity: no node can fit the request right now (HTTP 503).
	ErrNoCapacity = errors.New("no capacity")
	// ErrNodeUnavailable: the pinned node cannot take sandboxes at all (HTTP 409).
	ErrNodeUnavailable = errors.New("node unavailable")
)

// NoCapacityError explains a refusal: how many nodes were considered and why each was skipped.
type NoCapacityError struct {
	Req        Request
	Considered int
	Reasons    map[Reason]int
}

func (e *NoCapacityError) Is(target error) bool { return target == ErrNoCapacity }

func (e *NoCapacityError) Error() string {
	ask := fmt.Sprintf("cpu_millis=%d memory_mib=%d", e.Req.CPUMillis, e.Req.MemoryMiB)
	if e.Req.VMMProfile != "" {
		ask += " vmm_profile=" + e.Req.VMMProfile
	}
	if e.Req.PinnedNodeID != "" {
		return fmt.Sprintf("node %s cannot fit %s: %s", e.Req.PinnedNodeID, ask, e.reasonList())
	}
	if e.Considered == 0 {
		return "no schedulable nodes registered"
	}
	noun := "nodes"
	if e.Considered == 1 {
		noun = "node"
	}
	return fmt.Sprintf("no node can fit %s (%d %s: %s)", ask, e.Considered, noun, e.reasonList())
}

func (e *NoCapacityError) reasonList() string {
	type rc struct {
		r Reason
		n int
	}
	var list []rc
	for r, n := range e.Reasons {
		list = append(list, rc{r, n})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].r < list[j].r
	})
	parts := make([]string, 0, len(list))
	for _, x := range list {
		parts = append(parts, fmt.Sprintf("%d %s", x.n, x.r))
	}
	return strings.Join(parts, ", ")
}

// NodeUnavailableError: a pinned node that is unknown, down, revoked or cordoned.
type NodeUnavailableError struct {
	NodeID string
	Reason Reason
}

func (e *NodeUnavailableError) Is(target error) bool { return target == ErrNodeUnavailable }

func (e *NodeUnavailableError) Error() string {
	if e.Reason == ReasonUnknownNode {
		return fmt.Sprintf("node %s is not registered", e.NodeID)
	}
	return fmt.Sprintf("node %s cannot take sandboxes: %s", e.NodeID, e.Reason)
}

// Unschedulable returns why the node takes no new sandboxes regardless of size
// (liveness and admin state), or "".
func Unschedulable(cfg Config, c Candidate, now time.Time) Reason {
	ep := strings.TrimSpace(c.AgentEndpoint)
	switch {
	case ep == "" || strings.HasPrefix(ep, "local://"):
		return ReasonNoAgentEndpoint
	case c.Revoked:
		return ReasonRevoked
	case c.State == "offline":
		return ReasonOffline
	case c.LastSeen.IsZero() || (cfg.StaleAfter > 0 && now.Sub(c.LastSeen) > cfg.StaleAfter):
		return ReasonStale
	case !c.AcceptsWork:
		return ReasonNotAcceptingWork
	case c.Cordoned:
		return ReasonCordoned
	}
	return ""
}

// Allocatable is what the node offers; 0 means that dimension is not enforced.
func Allocatable(cfg Config, c Candidate) (cpuMillis, memMiB, sandboxes int64) {
	if c.CapCPUCores > 0 {
		over := cfg.CPUOvercommit
		if over <= 0 {
			over = 1
		}
		cpuMillis = int64(float64(c.CapCPUCores) * 1000 * over)
	}
	if c.CapMemMiB > 0 {
		memMiB = int64(c.CapMemMiB)
	}
	if c.MaxSandboxes > 0 {
		sandboxes = int64(c.MaxSandboxes)
	}
	return cpuMillis, memMiB, sandboxes
}

func reject(cfg Config, c Candidate, req Request, now time.Time) Reason {
	if r := Unschedulable(cfg, c, now); r != "" {
		return r
	}
	if req.VMMProfile != "" && len(c.VMMProfiles) > 0 && !slices.Contains(c.VMMProfiles, req.VMMProfile) {
		return ReasonVMMProfile
	}
	cpu, mem, slots := Allocatable(cfg, c)
	switch {
	case cpu > 0 && c.UsedCPUMillis+int64(req.CPUMillis) > cpu:
		return ReasonInsufficientCPU
	case mem > 0 && c.UsedMemMiB+int64(req.MemoryMiB) > mem:
		return ReasonInsufficientMem
	case slots > 0 && c.UsedSandboxes+1 > slots:
		return ReasonMaxSandboxes
	}
	return ""
}

// load is the dominant utilisation of the node after placing req, over the
// enforced dimensions (0 when nothing is enforced).
func load(cfg Config, c Candidate, req Request) float64 {
	cpu, mem, slots := Allocatable(cfg, c)
	var l float64
	if cpu > 0 {
		l = max(l, float64(c.UsedCPUMillis+int64(req.CPUMillis))/float64(cpu))
	}
	if mem > 0 {
		l = max(l, float64(c.UsedMemMiB+int64(req.MemoryMiB))/float64(mem))
	}
	if slots > 0 {
		l = max(l, float64(c.UsedSandboxes+1)/float64(slots))
	}
	return l
}

// Place returns the node for req. With a pin, only that node is checked: an
// unknown or unavailable node is a NodeUnavailableError, a full one a
// NoCapacityError. Ties break on sandbox count, then node id, so the result is
// deterministic.
func Place(cfg Config, req Request, nodes []Candidate, now time.Time) (string, error) {
	if pin := strings.TrimSpace(req.PinnedNodeID); pin != "" {
		for _, c := range nodes {
			if c.ID != pin {
				continue
			}
			r := reject(cfg, c, req, now)
			switch {
			case r == "":
				return c.ID, nil
			case r.capacity():
				return "", &NoCapacityError{Req: req, Considered: 1, Reasons: map[Reason]int{r: 1}}
			default:
				return "", &NodeUnavailableError{NodeID: pin, Reason: r}
			}
		}
		return "", &NodeUnavailableError{NodeID: pin, Reason: ReasonUnknownNode}
	}

	type fit struct {
		c    Candidate
		load float64
	}
	var fits []fit
	reasons := map[Reason]int{}
	considered := 0
	for _, c := range nodes {
		if r := Unschedulable(cfg, c, now); r == ReasonNoAgentEndpoint {
			continue // stub rows (auto-provision) are not nodes that run sandboxes
		}
		considered++
		if r := reject(cfg, c, req, now); r != "" {
			reasons[r]++
			continue
		}
		fits = append(fits, fit{c, load(cfg, c, req)})
	}
	if len(fits) == 0 {
		return "", &NoCapacityError{Req: req, Considered: considered, Reasons: reasons}
	}
	sort.Slice(fits, func(i, j int) bool {
		a, b := fits[i], fits[j]
		if a.load != b.load {
			if cfg.Policy == PolicyBinpack {
				return a.load > b.load
			}
			return a.load < b.load
		}
		if a.c.UsedSandboxes != b.c.UsedSandboxes {
			if cfg.Policy == PolicyBinpack {
				return a.c.UsedSandboxes > b.c.UsedSandboxes
			}
			return a.c.UsedSandboxes < b.c.UsedSandboxes
		}
		return a.c.ID < b.c.ID
	})
	return fits[0].c.ID, nil
}
