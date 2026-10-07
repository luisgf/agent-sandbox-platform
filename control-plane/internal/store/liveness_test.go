package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

type livenessHooks struct {
	setLastSeen func(id string, t time.Time)
	unassign    func(id string, createdAt time.Time)
}

func testNodeLoss(t *testing.T, s Store, h livenessHooks) {
	t.Helper()
	t.Setenv("ASP_AUTO_PROVISION", "0")
	registerPlacementNodes(t, s, 0, "node-a", "node-b")
	create := func(node string) Sandbox {
		t.Helper()
		sb, err := s.CreateSandbox(context.Background(), CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64, NodeID: node})
		if err != nil {
			t.Fatal(err)
		}
		return sb
	}
	requested := create("node-a")
	starting := create("node-a")
	if _, err := s.ClaimSandbox(context.Background(), starting.ID, "node-a"); err != nil {
		t.Fatal(err)
	}
	running := create("node-a")
	if _, err := s.ClaimSandbox(context.Background(), running.ID, "node-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSandboxStatus(context.Background(), running.ID, SandboxRunning, ""); err != nil {
		t.Fatal(err)
	}
	stopping := create("node-a")
	if _, err := s.ClaimSandbox(context.Background(), stopping.ID, "node-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StopSandbox(context.Background(), stopping.ID, ""); err != nil {
		t.Fatal(err)
	}
	other := create("node-b")

	now := time.Now().UTC()
	h.setLastSeen("node-a", now.Add(-10*time.Minute))

	// Offline after the stale window; once.
	if changed, err := s.MarkNodeOffline(context.Background(), "node-a", now.Add(-90*time.Second)); err != nil || !changed {
		t.Fatalf("mark offline: %v %v", changed, err)
	}
	if changed, _ := s.MarkNodeOffline(context.Background(), "node-a", now.Add(-90*time.Second)); changed {
		t.Fatal("marked offline twice")
	}
	if changed, _ := s.MarkNodeOffline(context.Background(), "node-b", now.Add(-90*time.Second)); changed {
		t.Fatal("a fresh node went offline")
	}
	if _, err := s.MarkNodeOffline(context.Background(), "ghost", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown node: %v", err)
	}

	// A fresh node keeps its sandboxes.
	if out, err := s.FailNodeSandboxes(context.Background(), "node-b", StopReasonNodeLost, now.Add(-5*time.Minute)); err != nil || len(out) != 0 {
		t.Fatalf("fresh node lost sandboxes: %v %v", out, err)
	}

	out, err := s.FailNodeSandboxes(context.Background(), "node-a", StopReasonNodeLost, now.Add(-5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 {
		t.Fatalf("failed %d sandboxes, want 4: %+v", len(out), out)
	}
	want := map[string]SandboxState{requested.ID: SandboxFailed, starting.ID: SandboxFailed, running.ID: SandboxFailed, stopping.ID: SandboxStopped}
	for id, st := range want {
		got, _ := s.GetSandbox(context.Background(), id)
		if got.State != st || got.StopReason != StopReasonNodeLost {
			t.Errorf("%s: %s/%s, want %s/node_lost", id, got.State, got.StopReason, st)
		}
	}
	if got, _ := s.GetSandbox(context.Background(), other.ID); got.State != SandboxRequested {
		t.Fatalf("node-b sandbox changed: %+v", got)
	}
	events, _ := s.ListEvents(context.Background(), running.ID)
	seen := false
	for _, ev := range events {
		seen = seen || ev.EventType == "sandbox.node_lost"
	}
	if !seen {
		t.Fatal("missing sandbox.node_lost event")
	}

	// The node comes back: ready again; its old sandboxes stay failed.
	if n, err := s.HeartbeatNode(context.Background(), "node-a"); err != nil || n.State != "ready" {
		t.Fatalf("heartbeat after loss: %+v %v", n, err)
	}
	if got, _ := s.GetSandbox(context.Background(), running.ID); got.State != SandboxFailed {
		t.Fatalf("a heartbeat revived a lost sandbox: %s", got.State)
	}

	// A node seen after silentSince is not lost (the heartbeat won the race).
	again := create("node-a")
	if out, _ := s.FailNodeSandboxes(context.Background(), "node-a", StopReasonNodeLost, time.Now().UTC().Add(-5*time.Minute)); len(out) != 0 {
		t.Fatalf("a node that just heartbeated lost sandboxes: %+v", out)
	}
	// A revoked node is lost at once.
	if _, err := s.RevokeNode(context.Background(), "node-a"); err != nil {
		t.Fatal(err)
	}
	if out, _ := s.FailNodeSandboxes(context.Background(), "node-a", StopReasonNodeLost, time.Now().UTC()); len(out) != 1 || out[0].ID != again.ID {
		t.Fatalf("revoked node: %+v", out)
	}

	// Rows from before placement at create are failed after a while.
	legacy := create("node-b")
	h.unassign(legacy.ID, now.Add(-time.Hour))
	if out, err := s.FailUnassignedRequested(context.Background(), now.Add(-time.Minute), StopReasonUnscheduled); err != nil || len(out) != 1 || out[0].ID != legacy.ID {
		t.Fatalf("unassigned legacy row: %+v %v", out, err)
	}
	if got, _ := s.GetSandbox(context.Background(), legacy.ID); got.State != SandboxFailed || got.StopReason != StopReasonUnscheduled {
		t.Fatalf("legacy row: %+v", got)
	}
}

func TestMemoryNodeLoss(t *testing.T) {
	s := NewMemoryStore()
	testNodeLoss(t, s, livenessHooks{setLastSeen: s.SetNodeLastSeenForTest, unassign: s.UnassignForTest})
}

func TestPostgresNodeLoss(t *testing.T) {
	s := newPostgresTestStore(t)
	ctx := context.Background()
	testNodeLoss(t, s, livenessHooks{
		setLastSeen: func(id string, ts time.Time) {
			if _, err := s.pool.Exec(ctx, `UPDATE nodes SET last_seen_at=$2 WHERE id=$1`, id, ts); err != nil {
				t.Fatal(err)
			}
		},
		unassign: func(id string, createdAt time.Time) {
			if _, err := s.pool.Exec(ctx, `UPDATE sandboxes SET node_id=NULL, created_at=$2 WHERE id=$1`, id, createdAt); err != nil {
				t.Fatal(err)
			}
		},
	})
}

// A new agent process id on register: the VM of a running or paused sandbox is
// gone with the old process but its disk is intact, so the sandbox becomes
// stopped (retained, resumable); stopping finishes; requested and starting are
// booted again.
func testAgentRestartOrphans(t *testing.T, s Store) {
	t.Helper()
	t.Setenv("ASP_AUTO_PROVISION", "0")
	register := func(instance string) {
		t.Helper()
		if _, err := s.RegisterNode(context.Background(), RegisterNodeInput{ID: "node-a", AgentEndpoint: "http://127.0.0.1:9100", AgentInstanceID: instance}); err != nil {
			t.Fatal(err)
		}
	}
	register("i1")
	create := func() Sandbox {
		t.Helper()
		sb, err := s.CreateSandbox(context.Background(), CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64})
		if err != nil {
			t.Fatal(err)
		}
		return sb
	}
	requested := create()
	starting := create()
	running := create()
	stopping := create()
	for _, sb := range []Sandbox{starting, running, stopping} {
		if _, err := s.ClaimSandbox(context.Background(), sb.ID, "node-a"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.UpdateSandboxStatus(context.Background(), running.ID, SandboxRunning, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSandboxStatus(context.Background(), stopping.ID, SandboxRunning, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StopSandbox(context.Background(), stopping.ID, ""); err != nil {
		t.Fatal(err)
	}

	register("i1") // same process re-registering (e.g. after a 404)
	register("")   // an agent that predates instance ids
	if got, _ := s.GetSandbox(context.Background(), running.ID); got.State != SandboxRunning {
		t.Fatalf("same agent re-registering failed a sandbox: %s", got.State)
	}

	register("i2") // the agent restarted
	want := map[string]SandboxState{
		requested.ID: SandboxRequested, starting.ID: SandboxStarting,
		running.ID: SandboxStopped, stopping.ID: SandboxStopped,
	}
	for id, st := range want {
		got, _ := s.GetSandbox(context.Background(), id)
		if got.State != st {
			t.Errorf("%s: %s, want %s", id, got.State, st)
		}
	}
	got, _ := s.GetSandbox(context.Background(), running.ID)
	if got.StopReason != StopReasonAgentRestarted || got.StoppedAt == nil {
		t.Fatalf("stop_reason = %q stopped_at = %v", got.StopReason, got.StoppedAt)
	}
	// The node keeps the disk of what it lost track of: it is in the retained list
	// (a failed sandbox would be in neither list and the node's GC would remove
	// its disk), and it can be resumed.
	work, err := s.ListNodeWork(context.Background(), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{running.ID, stopping.ID} {
		if !slices.Contains(work.Retained, id) {
			t.Errorf("%s is not in the retained list %v", id, work.Retained)
		}
		if slices.Contains(work.Assigned, id) {
			t.Errorf("%s is stopped yet assigned", id)
		}
	}
	resumed, err := s.ResumeSandbox(context.Background(), running.ID, "")
	if err != nil {
		t.Fatalf("resume of a sandbox orphaned by a restart: %v", err)
	}
	if resumed.State != SandboxRequested || resumed.BootCount < 2 {
		t.Fatalf("resumed: state=%s boot_count=%d", resumed.State, resumed.BootCount)
	}
	if n, _ := s.GetNode(context.Background(), "node-a"); n.AgentInstanceID != "i2" {
		t.Fatalf("agent_instance_id = %q", n.AgentInstanceID)
	}
}

func TestMemoryAgentRestartOrphans(t *testing.T) {
	testAgentRestartOrphans(t, NewMemoryStore())
}

func TestPostgresAgentRestartOrphans(t *testing.T) {
	testAgentRestartOrphans(t, newPostgresTestStore(t))
}

// An agent that restarted and took over some VMs says so on register: those
// sandboxes stay as they are, the others are orphaned as before, and a sandbox of
// another node in the list is not touched either way.
func testAgentRestartKeepsAdoptedSandboxes(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	t.Setenv("ASP_AUTO_PROVISION", "0")
	register := func(node, instance string, adopted ...string) {
		t.Helper()
		if _, err := s.RegisterNode(ctx, RegisterNodeInput{ID: node, AgentEndpoint: "http://127.0.0.1:9100", AgentInstanceID: instance, AdoptedSandboxes: adopted}); err != nil {
			t.Fatal(err)
		}
	}
	register("node-a", "a1")
	register("node-b", "b1")
	run := func(node string) Sandbox {
		t.Helper()
		sb, err := s.CreateSandbox(ctx, CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64, NodeID: node})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimSandbox(ctx, sb.ID, node); err != nil {
			t.Fatal(err)
		}
		out, err := s.UpdateSandboxStatus(ctx, sb.ID, SandboxRunning, "")
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	kept := run("node-a")
	lost := run("node-a")
	stopping := run("node-a")
	if _, err := s.StopSandbox(ctx, stopping.ID, ""); err != nil {
		t.Fatal(err)
	}
	elsewhere := run("node-b")

	// node-a restarts and says it still runs two of its three sandboxes, and, by mistake,
	// one that belongs to node-b.
	register("node-a", "a2", kept.ID, stopping.ID, elsewhere.ID)
	for id, want := range map[string]SandboxState{
		kept.ID:      SandboxRunning,  // adopted: its VM kept running
		stopping.ID:  SandboxStopping, // adopted: the node finishes the stop it had begun
		lost.ID:      SandboxStopped,  // not adopted: its VM is gone
		elsewhere.ID: SandboxRunning,  // another node's, and node-b did not restart
	} {
		if got := sandboxOf(t, s, id); got.State != want {
			t.Errorf("%s: %s, want %s", id, got.State, want)
		}
	}
	if got := sandboxOf(t, s, lost.ID); got.StopReason != StopReasonAgentRestarted {
		t.Errorf("the one that was not adopted: stop_reason=%q", got.StopReason)
	}
	if got := sandboxOf(t, s, kept.ID); got.StopReason != "" {
		t.Errorf("the adopted one has stop_reason=%q", got.StopReason)
	}

	// node-b restarts too, adopting nothing, and cannot save node-a's sandbox by name.
	register("node-b", "b2", kept.ID)
	if got := sandboxOf(t, s, elsewhere.ID); got.State != SandboxStopped {
		t.Errorf("node-b adopted nothing of its own: %s", got.State)
	}
	if got := sandboxOf(t, s, kept.ID); got.State != SandboxRunning {
		t.Errorf("node-b's list changed node-a's sandbox: %s", got.State)
	}
}

func TestMemoryAgentRestartKeepsAdoptedSandboxes(t *testing.T) {
	testAgentRestartKeepsAdoptedSandboxes(t, NewMemoryStore())
}

func TestPostgresAgentRestartKeepsAdoptedSandboxes(t *testing.T) {
	testAgentRestartKeepsAdoptedSandboxes(t, newPostgresTestStore(t))
}
