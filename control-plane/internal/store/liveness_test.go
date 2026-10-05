package store

import (
	"context"
	"errors"
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
		sb, err := s.CreateSandbox(CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64, NodeID: node})
		if err != nil {
			t.Fatal(err)
		}
		return sb
	}
	requested := create("node-a")
	starting := create("node-a")
	if _, err := s.ClaimSandbox(starting.ID, "node-a"); err != nil {
		t.Fatal(err)
	}
	running := create("node-a")
	if _, err := s.ClaimSandbox(running.ID, "node-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSandboxStatus(running.ID, SandboxRunning, ""); err != nil {
		t.Fatal(err)
	}
	stopping := create("node-a")
	if _, err := s.ClaimSandbox(stopping.ID, "node-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkSandboxStopping(stopping.ID, ""); err != nil {
		t.Fatal(err)
	}
	other := create("node-b")

	now := time.Now().UTC()
	h.setLastSeen("node-a", now.Add(-10*time.Minute))

	// Offline after the stale window; once.
	if changed, err := s.MarkNodeOffline("node-a", now.Add(-90*time.Second)); err != nil || !changed {
		t.Fatalf("mark offline: %v %v", changed, err)
	}
	if changed, _ := s.MarkNodeOffline("node-a", now.Add(-90*time.Second)); changed {
		t.Fatal("marked offline twice")
	}
	if changed, _ := s.MarkNodeOffline("node-b", now.Add(-90*time.Second)); changed {
		t.Fatal("a fresh node went offline")
	}
	if _, err := s.MarkNodeOffline("ghost", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown node: %v", err)
	}

	// A fresh node keeps its sandboxes.
	if out, err := s.FailNodeSandboxes("node-b", StopReasonNodeLost, now.Add(-5*time.Minute)); err != nil || len(out) != 0 {
		t.Fatalf("fresh node lost sandboxes: %v %v", out, err)
	}

	out, err := s.FailNodeSandboxes("node-a", StopReasonNodeLost, now.Add(-5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 {
		t.Fatalf("failed %d sandboxes, want 4: %+v", len(out), out)
	}
	want := map[string]SandboxState{requested.ID: SandboxFailed, starting.ID: SandboxFailed, running.ID: SandboxFailed, stopping.ID: SandboxStopped}
	for id, st := range want {
		got, _ := s.GetSandbox(id)
		if got.State != st || got.StopReason != StopReasonNodeLost || got.NodeLeaseUntil != nil {
			t.Errorf("%s: %s/%s, want %s/node_lost", id, got.State, got.StopReason, st)
		}
	}
	if got, _ := s.GetSandbox(other.ID); got.State != SandboxRequested {
		t.Fatalf("node-b sandbox changed: %+v", got)
	}
	events, _ := s.ListEvents(running.ID)
	seen := false
	for _, ev := range events {
		seen = seen || ev.EventType == "sandbox.node_lost"
	}
	if !seen {
		t.Fatal("missing sandbox.node_lost event")
	}

	// The node comes back: ready again; its old sandboxes stay failed.
	if n, err := s.HeartbeatNode("node-a"); err != nil || n.State != "ready" {
		t.Fatalf("heartbeat after loss: %+v %v", n, err)
	}
	if got, _ := s.GetSandbox(running.ID); got.State != SandboxFailed {
		t.Fatalf("a heartbeat revived a lost sandbox: %s", got.State)
	}

	// A node seen after silentSince is not lost (the heartbeat won the race).
	again := create("node-a")
	if out, _ := s.FailNodeSandboxes("node-a", StopReasonNodeLost, time.Now().UTC().Add(-5*time.Minute)); len(out) != 0 {
		t.Fatalf("a node that just heartbeated lost sandboxes: %+v", out)
	}
	// A revoked node is lost at once.
	if _, err := s.RevokeNode("node-a"); err != nil {
		t.Fatal(err)
	}
	if out, _ := s.FailNodeSandboxes("node-a", StopReasonNodeLost, time.Now().UTC()); len(out) != 1 || out[0].ID != again.ID {
		t.Fatalf("revoked node: %+v", out)
	}

	// Rows from before placement at create are failed after a while.
	legacy := create("node-b")
	h.unassign(legacy.ID, now.Add(-time.Hour))
	if out, err := s.FailUnassignedRequested(now.Add(-time.Minute), StopReasonUnscheduled); err != nil || len(out) != 1 || out[0].ID != legacy.ID {
		t.Fatalf("unassigned legacy row: %+v %v", out, err)
	}
	if got, _ := s.GetSandbox(legacy.ID); got.State != SandboxFailed || got.StopReason != StopReasonUnscheduled {
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
