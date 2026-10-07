package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/sched"
)

func registerPlacementNodes(t *testing.T, s Store, maxSandboxes int, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := s.RegisterNode(context.Background(), RegisterNodeInput{
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
			sb, err := s.CreateSandbox(context.Background(), CreateSandboxInput{
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
		return s.CreateSandbox(context.Background(), CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 512, NodeID: pin})
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
	if _, err := s.StopSandbox(context.Background(), pinned.ID, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSandbox(context.Background(), pinned.ID)
	if got.State != SandboxStopped {
		t.Fatalf("destroying a never-claimed sandbox stops it at once, got %s", got.State)
	}
	again, err := create("node-a")
	if err != nil || *again.NodeID != "node-a" {
		t.Fatalf("capacity freed by a stopped sandbox: %+v %v", again, err)
	}

	events, err := s.ListEvents(context.Background(), again.ID)
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
		sb, err := s.CreateSandbox(context.Background(), CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 512})
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

// Only the node a sandbox was placed on gets it as work and can claim it.
func testClaimOnlyByAssignedNode(t *testing.T, s Store) {
	t.Helper()
	t.Setenv("ASP_AUTO_PROVISION", "0")
	registerPlacementNodes(t, s, 0, "node-a", "node-b")
	sb, err := s.CreateSandbox(context.Background(), CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 512, NodeID: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	if work, _ := s.ListNodeWork(context.Background(), "node-b"); len(work.Sandboxes) != 0 || len(work.Assigned) != 0 {
		t.Fatalf("node-b sees another node's sandbox: %+v", work)
	}
	if work, _ := s.ListNodeWork(context.Background(), "node-a"); len(work.Sandboxes) != 1 || work.Sandboxes[0].ID != sb.ID || len(work.Assigned) != 1 || work.Assigned[0] != sb.ID {
		t.Fatalf("node-a work: %+v", work)
	}
	if _, err := s.ClaimSandbox(context.Background(), sb.ID, "node-b"); !errors.Is(err, ErrConflict) {
		t.Fatalf("claim by another node: %v", err)
	}
	claimed, err := s.ClaimSandbox(context.Background(), sb.ID, "node-a")
	if err != nil || claimed.State != SandboxStarting {
		t.Fatalf("claim by the assigned node: %+v %v", claimed, err)
	}
	if _, err := s.ClaimSandbox(context.Background(), sb.ID, "node-a"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second claim: %v", err)
	}
	if _, err := s.ClaimSandbox(context.Background(), "missing", "node-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claim of an unknown sandbox: %v", err)
	}
}

func TestMemoryClaimOnlyByAssignedNode(t *testing.T) {
	testClaimOnlyByAssignedNode(t, NewMemoryStore())
}

func TestPostgresClaimOnlyByAssignedNode(t *testing.T) {
	testClaimOnlyByAssignedNode(t, newPostgresTestStore(t))
}

// A work poll keeps the node fresh (throttled) and is refused for unknown nodes.
func testTouchNodePoll(t *testing.T, s Store) {
	t.Helper()
	registerPlacementNodes(t, s, 0, "node-a")
	if err := s.TouchNodePoll(context.Background(), "ghost", time.Now().UTC()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown node: %v", err)
	}
	later := time.Now().UTC().Add(time.Minute)
	if err := s.TouchNodePoll(context.Background(), "node-a", later); err != nil {
		t.Fatal(err)
	}
	n, _ := s.GetNode(context.Background(), "node-a")
	if n.LastSeenAt == nil || n.LastSeenAt.Before(later.Add(-time.Millisecond)) {
		t.Fatalf("poll did not refresh last_seen_at: %v", n.LastSeenAt)
	}
	// Within the throttle window nothing is written.
	if err := s.TouchNodePoll(context.Background(), "node-a", later.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	n2, _ := s.GetNode(context.Background(), "node-a")
	if !n2.LastSeenAt.Equal(*n.LastSeenAt) {
		t.Fatalf("throttled poll wrote last_seen_at: %v -> %v", n.LastSeenAt, n2.LastSeenAt)
	}
	// A revoked node stays revoked and offline.
	if _, err := s.RevokeNode(context.Background(), "node-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchNodePoll(context.Background(), "node-a", later.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n3, _ := s.GetNode(context.Background(), "node-a"); n3.State != "offline" {
		t.Fatalf("poll revived a revoked node: %+v", n3)
	}
}

func TestMemoryTouchNodePoll(t *testing.T) {
	testTouchNodePoll(t, NewMemoryStore())
}

func TestPostgresTouchNodePoll(t *testing.T) {
	testTouchNodePoll(t, newPostgresTestStore(t))
}

// Cordon stops placements, survives re-registration, and usage sums what is placed.
func testCordonAndUsage(t *testing.T, s Store) {
	t.Helper()
	t.Setenv("ASP_AUTO_PROVISION", "0")
	registerPlacementNodes(t, s, 0, "node-a", "node-b")
	if _, err := s.SetNodeCordoned(context.Background(), "ghost", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cordon unknown node: %v", err)
	}
	n, err := s.SetNodeCordoned(context.Background(), "node-a", true)
	if err != nil || !n.Cordoned {
		t.Fatalf("cordon: %+v %v", n, err)
	}
	// An agent re-registering never lifts an admin's cordon.
	registerPlacementNodes(t, s, 0, "node-a")
	if n, _ := s.GetNode(context.Background(), "node-a"); !n.Cordoned {
		t.Fatal("re-register lifted the cordon")
	}
	for i := 0; i < 3; i++ {
		sb, err := s.CreateSandbox(context.Background(), CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 512})
		if err != nil || *sb.NodeID != "node-b" {
			t.Fatalf("placement must skip the cordoned node: %+v %v", sb, err)
		}
	}
	if _, err := s.CreateSandbox(context.Background(), CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 512, NodeID: "node-a"}); !errors.Is(err, ErrNodeUnavailable) {
		t.Fatalf("pin to a cordoned node: %v", err)
	}
	usage, err := s.ListNodeUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u := usage["node-b"]; u.Sandboxes != 3 || u.CPUMillis != 3000 || u.MemoryMiB != 1536 {
		t.Fatalf("usage node-b = %+v", u)
	}
	if u := usage["node-a"]; u.Sandboxes != 0 {
		t.Fatalf("usage node-a = %+v", u)
	}
	if _, err := s.SetNodeCordoned(context.Background(), "node-a", false); err != nil {
		t.Fatal(err)
	}
	sb, err := s.CreateSandbox(context.Background(), CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 512})
	if err != nil || *sb.NodeID != "node-a" {
		t.Fatalf("after uncordon spread goes to node-a: %+v %v", sb, err)
	}
}

func TestMemoryCordonAndUsage(t *testing.T) {
	testCordonAndUsage(t, NewMemoryStore())
}

func TestPostgresCordonAndUsage(t *testing.T) {
	testCordonAndUsage(t, newPostgresTestStore(t))
}

// assignedTo reports whether node's work poll lists id as assigned to it.
func assignedTo(t *testing.T, s Store, node, id string) bool {
	t.Helper()
	work, err := s.ListNodeWork(context.Background(), node)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range work.Assigned {
		if a == id {
			return true
		}
	}
	return false
}

// Late agent reports cannot bring a sandbox back, and a sandbox stays in its
// node's assigned set only while it holds the node: when it leaves, the node
// stops the VM.
func testAgentTransitionsAndAssignment(t *testing.T, s Store) {
	t.Helper()
	t.Setenv("ASP_AUTO_PROVISION", "0")
	registerPlacementNodes(t, s, 0, "node-a", "node-b")
	newClaimed := func() Sandbox {
		t.Helper()
		sb, err := s.CreateSandbox(context.Background(), CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 512, NodeID: "node-a"})
		if err != nil {
			t.Fatal(err)
		}
		if !assignedTo(t, s, "node-a", sb.ID) {
			t.Fatal("a placed sandbox is assigned to its node before the claim")
		}
		if _, err := s.ClaimSandbox(context.Background(), sb.ID, "node-a"); err != nil {
			t.Fatal(err)
		}
		return sb
	}

	sb := newClaimed()
	if _, err := s.UpdateSandboxStatus(context.Background(), sb.ID, SandboxRunning, "booted"); err != nil {
		t.Fatal(err)
	}
	if !assignedTo(t, s, "node-a", sb.ID) || assignedTo(t, s, "node-b", sb.ID) {
		t.Fatal("a running sandbox is assigned to its node only")
	}
	if work, _ := s.ListNodeWork(context.Background(), "node-a"); len(work.Sandboxes) != 0 {
		t.Fatalf("a running sandbox without local-net needs no action: %+v", work.Sandboxes)
	}
	if _, err := s.StopSandbox(context.Background(), sb.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSandboxStatus(context.Background(), sb.ID, SandboxRunning, "late"); !errors.Is(err, ErrConflict) {
		t.Fatalf("running after stopping: %v", err)
	}
	if !assignedTo(t, s, "node-a", sb.ID) {
		t.Fatal("a stopping sandbox stays assigned: its VM still exists")
	}
	if _, err := s.UpdateSandboxStatus(context.Background(), sb.ID, SandboxStopped, "cleaned"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSandboxStatus(context.Background(), sb.ID, SandboxRunning, "late"); !errors.Is(err, ErrConflict) {
		t.Fatalf("running after stopped: %v", err)
	}
	if assignedTo(t, s, "node-a", sb.ID) {
		t.Fatal("a stopped sandbox must leave the assigned set, so the node stops any VM left")
	}
	if got, _ := s.GetSandbox(context.Background(), sb.ID); got.State != SandboxStopped {
		t.Fatalf("state = %s, want stopped", got.State)
	}

	failed := newClaimed()
	if _, err := s.UpdateSandboxStatus(context.Background(), failed.ID, SandboxFailed, "boot error"); err != nil {
		t.Fatal(err)
	}
	if assignedTo(t, s, "node-a", failed.ID) {
		t.Fatal("a failed sandbox must leave the assigned set")
	}
	if _, err := s.UpdateSandboxStatus(context.Background(), failed.ID, SandboxRunning, "late"); !errors.Is(err, ErrConflict) {
		t.Fatalf("running after failed: %v", err)
	}
	if _, err := s.UpdateSandboxStatus(context.Background(), failed.ID, SandboxStopped, "cleanup"); err != nil {
		t.Fatalf("failed → stopped: %v", err)
	}
}

func TestMemoryAgentTransitionsAndAssignment(t *testing.T) {
	testAgentTransitionsAndAssignment(t, NewMemoryStore())
}

func TestPostgresAgentTransitionsAndAssignment(t *testing.T) {
	testAgentTransitionsAndAssignment(t, newPostgresTestStore(t))
}

func TestCreateRejectsGuestsBelowTheMinimumMemory(t *testing.T) {
	m := newMemoryStoreWithNodes(t, "n1")
	_, err := m.CreateSandbox(context.Background(), CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 32})
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "at least 64") {
		t.Fatalf("memory_mib 32: want invalid input naming the minimum, got %v", err)
	}
	if _, err := m.CreateSandbox(context.Background(), CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: MinSandboxMemoryMiB}); err != nil {
		t.Fatalf("memory_mib 64: %v", err)
	}
}
