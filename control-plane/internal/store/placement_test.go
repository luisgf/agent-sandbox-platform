package store

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/sched"
)

func registerPlacementNodes(t *testing.T, s Store, maxSandboxes int, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := s.RegisterNode(RegisterNodeInput{
			ID: id, AgentEndpoint: "http://127.0.0.1:9100",
			CapacityCPU: 4, CapacityMemMiB: 8192, MaxSandboxes: maxSandboxes,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func createN(s Store, n int) (placed map[string]int, refused int, other []error) {
	placed = map[string]int{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			sb, err := s.CreateSandbox(CreateSandboxInput{
				TenantID: "t", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 512,
				OwnerSub: fmt.Sprintf("user-%d", i),
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				placed[*sb.NodeID]++
			case errors.Is(err, ErrNoCapacity):
				refused++
			default:
				other = append(other, err)
			}
		}(i)
	}
	wg.Wait()
	return placed, refused, other
}

// Concurrent creates must never overfill a node, in either store.
func testConcurrentCreatesRespectCapacity(t *testing.T, s Store) {
	t.Helper()
	t.Setenv("ASP_AUTO_PROVISION", "0")
	registerPlacementNodes(t, s, 2, "node-a", "node-b")
	placed, refused, other := createN(s, 40)
	if len(other) > 0 {
		t.Fatalf("unexpected errors: %v", other)
	}
	if placed["node-a"] != 2 || placed["node-b"] != 2 || refused != 36 {
		t.Fatalf("placed=%v refused=%d, want 2 per node and 36 refused", placed, refused)
	}
}

func TestMemoryConcurrentCreatesRespectCapacity(t *testing.T) {
	testConcurrentCreatesRespectCapacity(t, NewMemoryStore())
}

func TestPostgresConcurrentCreatesRespectCapacity(t *testing.T) {
	testConcurrentCreatesRespectCapacity(t, newPostgresTestStore(t))
}

// Placement policy, pins and freed capacity, in either store.
func testPlacementPolicyAndPins(t *testing.T, s Store, setCfg func(sched.Config)) {
	t.Helper()
	t.Setenv("ASP_AUTO_PROVISION", "0")
	registerPlacementNodes(t, s, 2, "node-a", "node-b")
	create := func(pin string) (Sandbox, error) {
		return s.CreateSandbox(CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 512, NodeID: pin})
	}

	// spread: the second sandbox goes to the other node.
	first, err := create("")
	if err != nil {
		t.Fatal(err)
	}
	second, err := create("")
	if err != nil {
		t.Fatal(err)
	}
	if *first.NodeID == *second.NodeID {
		t.Fatalf("spread placed both on %s", *first.NodeID)
	}

	// Pins: to a node with room, to an unknown node (409), to a full node (503).
	pinned, err := create("node-a")
	if err != nil || *pinned.NodeID != "node-a" {
		t.Fatalf("pin with room: %+v %v", pinned, err)
	}
	if _, err := create("ghost"); !errors.Is(err, ErrNodeUnavailable) {
		t.Fatalf("pin to an unknown node: %v", err)
	}
	if _, err := create("node-a"); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("pin to a full node: %v", err)
	}

	// Stopping does not free capacity (the VM may still run); stopped does.
	if _, err := s.MarkSandboxStopping(pinned.ID, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSandbox(pinned.ID)
	if got.State != SandboxStopped {
		t.Fatalf("destroying a never-claimed sandbox stops it at once, got %s", got.State)
	}
	again, err := create("node-a")
	if err != nil || *again.NodeID != "node-a" {
		t.Fatalf("capacity freed by a stopped sandbox: %+v %v", again, err)
	}

	events, err := s.ListEvents(again.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.EventType == "sandbox.placed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing sandbox.placed event: %+v", events)
	}
}

// binpack fills the most loaded node that still fits, keeping others free.
func testBinpackStacksSandboxes(t *testing.T, s Store, setCfg func(sched.Config)) {
	t.Helper()
	t.Setenv("ASP_AUTO_PROVISION", "0")
	cfg := sched.DefaultConfig()
	cfg.Policy = sched.PolicyBinpack
	setCfg(cfg)
	registerPlacementNodes(t, s, 3, "node-c", "node-d")
	var got []string
	for i := 0; i < 4; i++ {
		sb, err := s.CreateSandbox(CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 512})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, *sb.NodeID)
	}
	// Equal load at first: node id breaks the tie; then node-c fills up (3 slots).
	if got[0] != "node-c" || got[1] != "node-c" || got[2] != "node-c" || got[3] != "node-d" {
		t.Fatalf("binpack order = %v", got)
	}
}

func TestMemoryPlacementPolicyAndPins(t *testing.T) {
	s := NewMemoryStore()
	testPlacementPolicyAndPins(t, s, s.SetSchedConfig)
}

func TestPostgresPlacementPolicyAndPins(t *testing.T) {
	s := newPostgresTestStore(t)
	testPlacementPolicyAndPins(t, s, s.SetSchedConfig)
}

func TestMemoryBinpackStacksSandboxes(t *testing.T) {
	s := NewMemoryStore()
	testBinpackStacksSandboxes(t, s, s.SetSchedConfig)
}

func TestPostgresBinpackStacksSandboxes(t *testing.T) {
	s := newPostgresTestStore(t)
	testBinpackStacksSandboxes(t, s, s.SetSchedConfig)
}
