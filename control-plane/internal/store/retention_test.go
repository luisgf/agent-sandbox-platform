package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestParseRetentionTTL(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{"", DefaultStoppedSandboxTTL, false},
		{"  ", DefaultStoppedSandboxTTL, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"48h", 48 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"0", 0, false},
		{"off", 0, false},
		{"Disabled", 0, false},
		{"none", 0, false},
		{"-1h", 0, true},
		{"-2d", 0, true},
		{"soon", 0, true},
		{"1.5d", 0, true},
	} {
		got, err := ParseRetentionTTL(tc.in)
		if tc.bad {
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("ParseRetentionTTL(%q): want an invalid input error, got %v", tc.in, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ParseRetentionTTL(%q)=%v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	if DefaultStoppedSandboxTTL != 7*24*time.Hour {
		t.Fatalf("the default TTL is 7 days, got %v", DefaultStoppedSandboxTTL)
	}
}

func TestParseMaxStoppedPerTenant(t *testing.T) {
	for in, want := range map[string]int{"": 0, "0": 0, "5": 5, " 12 ": 12} {
		if got, err := ParseMaxStoppedPerTenant(in); err != nil || got != want {
			t.Errorf("ParseMaxStoppedPerTenant(%q)=%d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"-1", "many", "1.5"} {
		if _, err := ParseMaxStoppedPerTenant(in); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("ParseMaxStoppedPerTenant(%q): want an invalid input error, got %v", in, err)
		}
	}
}

// stoppedOn runs a sandbox of tenant on node and stops it, with stopped_at at ago.
func stoppedOn(t *testing.T, s Store, node, tenant string, ago time.Duration, setStoppedAt func(string, time.Time)) Sandbox {
	t.Helper()
	sb, err := s.CreateSandbox(context.Background(), CreateSandboxInput{TenantID: tenant, ImageRef: "img", CPUMillis: 100, MemoryMiB: 64, OwnerSub: "user:a", NodeID: node})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimSandbox(context.Background(), sb.ID, node); err != nil {
		t.Fatal(err)
	}
	report(t, s, sb.ID, SandboxRunning, "")
	if _, err := s.StopSandbox(context.Background(), sb.ID, ""); err != nil {
		t.Fatal(err)
	}
	report(t, s, sb.ID, SandboxStopped, "")
	setStoppedAt(sb.ID, time.Now().Add(-ago))
	return sandboxOf(t, s, sb.ID)
}

// The TTL deletes what has been stopped for long enough, and only that.
func testExpireStoppedSandboxes(t *testing.T, s Store, setStoppedAt func(string, time.Time)) {
	lifecycleStore(t, s, 0, "node-a")
	old := stoppedOn(t, s, "node-a", "t", 8*24*time.Hour, setStoppedAt)
	fresh := stoppedOn(t, s, "node-a", "t", time.Hour, setStoppedAt)
	running := runningOn(t, s, "node-a")

	now := time.Now().UTC()
	if out, err := s.ExpireStoppedSandboxes(context.Background(), now, 0); err != nil || len(out) != 0 {
		t.Fatalf("ttl 0 keeps stopped sandboxes for ever: %v %v", out, err)
	}
	out, err := s.ExpireStoppedSandboxes(context.Background(), now, 7*24*time.Hour)
	if err != nil || len(out) != 1 || out[0].ID != old.ID {
		t.Fatalf("expired: %+v %v; want only %s", out, err, old.ID)
	}
	got := sandboxOf(t, s, old.ID)
	if got.State != SandboxDeleting || got.StopReason != StopReasonRetention {
		t.Fatalf("expired sandbox: state=%s reason=%q", got.State, got.StopReason)
	}
	if w := workOf(t, s, "node-a"); contains(w.Retained, old.ID) || !inWork(w, old.ID) {
		t.Fatalf("the node must be told to delete it, and stop keeping it: %+v", w)
	}
	if sandboxOf(t, s, fresh.ID).State != SandboxStopped || sandboxOf(t, s, running.ID).State != SandboxRunning {
		t.Fatal("retention touched a sandbox it should not")
	}
	var found bool
	events, _ := s.ListEvents(context.Background(), old.ID)
	for _, e := range events {
		found = found || (e.EventType == "sandbox.deleted" && e.Actor == "retention-reaper")
	}
	if !found {
		t.Fatalf("no sandbox.deleted event: %+v", events)
	}
	// It does not expire twice, and the node finishing the delete ends it.
	if again, _ := s.ExpireStoppedSandboxes(context.Background(), now, 7*24*time.Hour); len(again) != 0 {
		t.Fatalf("second sweep: %+v", again)
	}
	if done := report(t, s, old.ID, SandboxDeleted, "vmm and disk removed"); done.State != SandboxDeleted {
		t.Fatalf("deleted: %+v", done)
	}
}

func TestMemoryExpireStoppedSandboxes(t *testing.T) {
	s := NewMemoryStore()
	testExpireStoppedSandboxes(t, s, s.SetStoppedAtForTest)
}

func TestPostgresExpireStoppedSandboxes(t *testing.T) {
	s := newPostgresTestStore(t)
	testExpireStoppedSandboxes(t, s, func(id string, at time.Time) {
		if _, err := s.pool.Exec(context.Background(), `UPDATE sandboxes SET stopped_at=$2 WHERE id=$1`, id, at); err != nil {
			t.Fatal(err)
		}
	})
}

// The cap deletes each tenant's oldest stopped sandboxes beyond it.
func testEvictStoppedOverCap(t *testing.T, s Store, setStoppedAt func(string, time.Time)) {
	lifecycleStore(t, s, 0, "node-a")
	a1 := stoppedOn(t, s, "node-a", "tenant-a", 5*time.Hour, setStoppedAt) // oldest
	a2 := stoppedOn(t, s, "node-a", "tenant-a", 4*time.Hour, setStoppedAt)
	a3 := stoppedOn(t, s, "node-a", "tenant-a", 3*time.Hour, setStoppedAt)
	a4 := stoppedOn(t, s, "node-a", "tenant-a", 2*time.Hour, setStoppedAt)
	b1 := stoppedOn(t, s, "node-a", "tenant-b", 9*time.Hour, setStoppedAt) // alone in its tenant

	if out, err := s.EvictStoppedOverCap(context.Background(), 0); err != nil || len(out) != 0 {
		t.Fatalf("cap 0 is no cap: %v %v", out, err)
	}
	out, err := s.EvictStoppedOverCap(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, sb := range out {
		ids[sb.ID] = true
		if sb.StopReason != StopReasonTenantCap || sb.State != SandboxDeleting {
			t.Fatalf("evicted: %+v", sb)
		}
	}
	if len(out) != 2 || !ids[a1.ID] || !ids[a2.ID] {
		t.Fatalf("evicted %v, want the two oldest of tenant-a (%s, %s)", ids, a1.ID, a2.ID)
	}
	for _, keep := range []Sandbox{a3, a4, b1} {
		if sandboxOf(t, s, keep.ID).State != SandboxStopped {
			t.Fatalf("%s was evicted", keep.ID)
		}
	}
	if again, _ := s.EvictStoppedOverCap(context.Background(), 2); len(again) != 0 {
		t.Fatalf("second sweep: %+v", again)
	}
}

func TestMemoryEvictStoppedOverCap(t *testing.T) {
	s := NewMemoryStore()
	testEvictStoppedOverCap(t, s, s.SetStoppedAtForTest)
}

func TestPostgresEvictStoppedOverCap(t *testing.T) {
	s := newPostgresTestStore(t)
	testEvictStoppedOverCap(t, s, func(id string, at time.Time) {
		if _, err := s.pool.Exec(context.Background(), `UPDATE sandboxes SET stopped_at=$2 WHERE id=$1`, id, at); err != nil {
			t.Fatal(err)
		}
	})
}

func TestIsMemory(t *testing.T) {
	if !IsMemory(NewMemoryStore()) {
		t.Fatal("the memory store is memory")
	}
}

// A node's reported free disk space is kept, survives a register (a restart of
// its agent), and the stopped sandboxes on each node are counted.
func testNodeDiskFreeAndStoppedCount(t *testing.T, s Store) {
	lifecycleStore(t, s, 0, "node-a", "node-b")
	if err := s.SetNodeDiskFree(context.Background(), "ghost", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown node: %v", err)
	}
	if n, _ := s.GetNode(context.Background(), "node-a"); n.DiskFreeMiB != nil {
		t.Fatalf("not reported yet: %v", *n.DiskFreeMiB)
	}
	if err := s.SetNodeDiskFree(context.Background(), "node-a", 700000); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.GetNode(context.Background(), "node-a"); n.DiskFreeMiB == nil || *n.DiskFreeMiB != 700000 {
		t.Fatalf("disk free: %v", n.DiskFreeMiB)
	}
	// The agent restarts and registers again: the last figure stays until its next heartbeat.
	registerPlacementNodes(t, s, 0, "node-a")
	if n, _ := s.GetNode(context.Background(), "node-a"); n.DiskFreeMiB == nil || *n.DiskFreeMiB != 700000 {
		t.Fatalf("a register lost the disk figure: %v", n.DiskFreeMiB)
	}

	noop := func(string, time.Time) {}
	stoppedOn(t, s, "node-a", "t", time.Hour, noop)
	stoppedOn(t, s, "node-a", "t", time.Hour, noop)
	stoppedOn(t, s, "node-b", "t", time.Hour, noop)
	runningOn(t, s, "node-b")
	counts, err := s.CountStoppedByNode(context.Background())
	if err != nil || counts["node-a"] != 2 || counts["node-b"] != 1 || len(counts) != 2 {
		t.Fatalf("counts=%v err=%v", counts, err)
	}
}

func TestMemoryNodeDiskFreeAndStoppedCount(t *testing.T) {
	testNodeDiskFreeAndStoppedCount(t, NewMemoryStore())
}
func TestPostgresNodeDiskFreeAndStoppedCount(t *testing.T) {
	testNodeDiskFreeAndStoppedCount(t, newPostgresTestStore(t))
}
