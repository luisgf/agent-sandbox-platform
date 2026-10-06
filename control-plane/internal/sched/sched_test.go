package sched

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// node is a healthy candidate with 4 cores (16 vCPU at 4x), 8 GiB and no slot limit.
func node(id string) Candidate {
	return Candidate{
		ID: id, AgentEndpoint: "https://" + id + ":9443", State: "ready",
		AcceptsWork: true, LastSeen: now.Add(-5 * time.Second),
		VMMProfiles: []string{"cloud-hypervisor"},
		CapCPUCores: 4, CapMemMiB: 8192,
	}
}

func req(cpu, mem int) Request {
	return Request{CPUMillis: cpu, MemoryMiB: mem, VMMProfile: "cloud-hypervisor"}
}

func TestEachReasonExcludesTheNode(t *testing.T) {
	cfg := DefaultConfig()
	cases := []struct {
		reason Reason
		edit   func(*Candidate)
		r      Request
	}{
		{ReasonRevoked, func(c *Candidate) { c.Revoked = true }, req(1000, 512)},
		{ReasonOffline, func(c *Candidate) { c.State = "offline" }, req(1000, 512)},
		{ReasonStale, func(c *Candidate) { c.LastSeen = now.Add(-91 * time.Second) }, req(1000, 512)},
		{ReasonStale, func(c *Candidate) { c.LastSeen = time.Time{} }, req(1000, 512)},
		{ReasonNotAcceptingWork, func(c *Candidate) { c.AcceptsWork = false }, req(1000, 512)},
		{ReasonCordoned, func(c *Candidate) { c.Cordoned = true }, req(1000, 512)},
		{ReasonVMMProfile, func(c *Candidate) { c.VMMProfiles = []string{"firecracker"} }, req(1000, 512)},
		{ReasonInsufficientCPU, func(c *Candidate) { c.UsedCPUMillis = 15500 }, req(1000, 512)},
		{ReasonInsufficientMem, func(c *Candidate) { c.UsedMemMiB = 7800 }, req(1000, 512)},
		{ReasonMaxSandboxes, func(c *Candidate) { c.MaxSandboxes = 2; c.UsedSandboxes = 2 }, req(1000, 512)},
	}
	for _, tc := range cases {
		c := node("n1")
		tc.edit(&c)
		_, err := Place(cfg, tc.r, []Candidate{c}, now)
		var nc *NoCapacityError
		if !errors.As(err, &nc) || nc.Reasons[tc.reason] != 1 || nc.Considered != 1 {
			t.Errorf("%s: got %v", tc.reason, err)
		}
		if !errors.Is(err, ErrNoCapacity) {
			t.Errorf("%s: unpinned refusals are ErrNoCapacity, got %v", tc.reason, err)
		}
	}
}

func TestStubNodesAreNotCandidates(t *testing.T) {
	stub := node("local-dev")
	stub.AgentEndpoint = "local://stub"
	_, err := Place(DefaultConfig(), req(1000, 512), []Candidate{stub}, now)
	if err == nil || err.Error() != "no schedulable nodes registered" {
		t.Fatalf("got %v", err)
	}
	if _, err := Place(DefaultConfig(), req(1000, 512), nil, now); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("no nodes: %v", err)
	}
}

func TestSpreadPicksTheLeastLoadedNode(t *testing.T) {
	a, b, c := node("a"), node("b"), node("c")
	a.UsedMemMiB, a.UsedSandboxes = 4096, 4
	b.UsedMemMiB, b.UsedSandboxes = 1024, 1
	c.UsedMemMiB, c.UsedSandboxes = 2048, 2
	got, err := Place(DefaultConfig(), req(1000, 512), []Candidate{a, b, c}, now)
	if err != nil || got != "b" {
		t.Fatalf("spread: got %q %v, want b", got, err)
	}
}

func TestBinpackPicksTheMostLoadedNodeThatFits(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Policy = PolicyBinpack
	a, b, c := node("a"), node("b"), node("c")
	a.UsedMemMiB = 8000 // full for 512 MiB
	b.UsedMemMiB = 4096
	c.UsedMemMiB = 1024
	got, err := Place(cfg, req(1000, 512), []Candidate{a, b, c}, now)
	if err != nil || got != "b" {
		t.Fatalf("binpack: got %q %v, want b", got, err)
	}
}

func TestTiesBreakOnSandboxCountThenNodeID(t *testing.T) {
	// Nothing enforced: load is 0 everywhere, so sandbox count decides, then id.
	a, b, c := node("c"), node("a"), node("b")
	for _, n := range []*Candidate{&a, &b, &c} {
		n.CapCPUCores, n.CapMemMiB = 0, 0
	}
	a.UsedSandboxes, b.UsedSandboxes, c.UsedSandboxes = 1, 1, 0
	if got, _ := Place(DefaultConfig(), req(1000, 512), []Candidate{a, b, c}, now); got != "b" {
		t.Fatalf("fewest sandboxes first: got %q, want b", got)
	}
	c.UsedSandboxes = 1
	if got, _ := Place(DefaultConfig(), req(1000, 512), []Candidate{a, b, c}, now); got != "a" {
		t.Fatalf("then node id: got %q, want a", got)
	}
}

func TestCPUOvercommit(t *testing.T) {
	n := node("n1") // 4 cores
	n.UsedCPUMillis = 15000
	cfg := DefaultConfig() // 4x = 16000 millis
	if _, err := Place(cfg, req(1000, 512), []Candidate{n}, now); err != nil {
		t.Fatalf("16000 millis at 4x must fit 15000+1000: %v", err)
	}
	cfg.CPUOvercommit = 1 // 4000 millis
	if _, err := Place(cfg, req(1000, 512), []Candidate{n}, now); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("at 1x the node is full: %v", err)
	}
	if cpu, mem, slots := Allocatable(DefaultConfig(), node("x")); cpu != 16000 || mem != 8192 || slots != 0 {
		t.Fatalf("Allocatable = %d %d %d", cpu, mem, slots)
	}
}

func TestPins(t *testing.T) {
	cfg := DefaultConfig()
	full := node("full")
	full.UsedMemMiB = 8192
	down := node("down")
	down.State = "offline"
	nodes := []Candidate{node("ok"), full, down}

	if got, err := Place(cfg, Request{CPUMillis: 1000, MemoryMiB: 512, PinnedNodeID: "ok"}, nodes, now); err != nil || got != "ok" {
		t.Fatalf("pin ok: %q %v", got, err)
	}
	_, err := Place(cfg, Request{CPUMillis: 1000, MemoryMiB: 512, PinnedNodeID: "full"}, nodes, now)
	if !errors.Is(err, ErrNoCapacity) || !strings.Contains(err.Error(), "node full cannot fit") {
		t.Fatalf("pin to a full node: %v", err)
	}
	_, err = Place(cfg, Request{CPUMillis: 1000, MemoryMiB: 512, PinnedNodeID: "down"}, nodes, now)
	var nu *NodeUnavailableError
	if !errors.As(err, &nu) || nu.Reason != ReasonOffline || !errors.Is(err, ErrNodeUnavailable) {
		t.Fatalf("pin to an offline node: %v", err)
	}
	_, err = Place(cfg, Request{CPUMillis: 1000, MemoryMiB: 512, PinnedNodeID: "ghost"}, nodes, now)
	if !errors.As(err, &nu) || nu.Reason != ReasonUnknownNode || err.Error() != "node ghost is not registered" {
		t.Fatalf("pin to an unknown node: %v", err)
	}
}

func TestNoCapacityMessageCountsReasons(t *testing.T) {
	a, b, c := node("a"), node("b"), node("c")
	a.UsedMemMiB, b.UsedMemMiB = 8192, 8000
	c.Cordoned = true
	_, err := Place(DefaultConfig(), req(1000, 4096), []Candidate{a, b, c}, now)
	want := "no node can fit cpu_millis=1000 memory_mib=4096 vmm_profile=cloud-hypervisor (3 nodes: 2 insufficient_memory, 1 cordoned)"
	if err == nil || err.Error() != want {
		t.Fatalf("message:\n got %v\nwant %s", err, want)
	}
}

func TestConfigFromEnv(t *testing.T) {
	cfg, err := ConfigFromEnv()
	if err != nil || cfg != DefaultConfig() {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	t.Setenv(EnvPolicy, "BinPack")
	t.Setenv(EnvCPUOvercommit, "2.5")
	t.Setenv(EnvStaleAfter, "30s")
	cfg, err = ConfigFromEnv()
	if err != nil || cfg.Policy != PolicyBinpack || cfg.CPUOvercommit != 2.5 || cfg.StaleAfter != 30*time.Second {
		t.Fatalf("from env: %+v %v", cfg, err)
	}
	for env, bad := range map[string]string{EnvPolicy: "random", EnvCPUOvercommit: "0", EnvStaleAfter: "-1s"} {
		t.Setenv(env, bad)
		if _, err := ConfigFromEnv(); err == nil {
			t.Errorf("%s=%s must be rejected", env, bad)
		}
		t.Setenv(env, "")
	}
}

// Memory is never overcommitted, so each VM's own overhead counts: a node with
// room for the guests' memory alone is full once the overhead is added.
func TestVMOverheadCountsAgainstMemory(t *testing.T) {
	cfg := DefaultConfig()
	c := node("n1")
	c.CapMemMiB = 1024
	c.UsedSandboxes = 3
	c.UsedMemMiB = 3 * 256 // 768 MiB of guests + 3*64 overhead = 960 MiB held
	if _, err := Place(cfg, req(1000, 64), []Candidate{c}, now); err == nil {
		t.Fatal("960 MiB held + 64 asked + 64 overhead exceeds 1024 MiB")
	}
	cfg.VMOverheadMiB = 0
	if _, err := Place(cfg, req(1000, 64), []Candidate{c}, now); err != nil {
		t.Fatalf("without overhead 768+64 fits in 1024: %v", err)
	}
	if got := MemoryUsed(DefaultConfig(), c); got != 768+3*64 {
		t.Fatalf("MemoryUsed=%d", got)
	}
}

func TestConfigFromEnvVMOverhead(t *testing.T) {
	t.Setenv(EnvVMOverhead, "")
	if cfg, err := ConfigFromEnv(); err != nil || cfg.VMOverheadMiB != DefaultVMOverheadMiB {
		t.Fatalf("default: %+v %v", cfg, err)
	}
	t.Setenv(EnvVMOverhead, "128")
	if cfg, err := ConfigFromEnv(); err != nil || cfg.VMOverheadMiB != 128 {
		t.Fatalf("128: %+v %v", cfg, err)
	}
	t.Setenv(EnvVMOverhead, "-1")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("negative overhead must be rejected")
	}
}
