package store

import (
	"context"
	"errors"
	"testing"
)

// booted_at says whether a sandbox ever ran: until it does, nothing is worth
// keeping and a resume gets a fresh disk; after, a disk is kept and resume reuses
// it. It is never cleared, and the first run is the one it records (#112).
func testBootedAt(t *testing.T, s Store) {
	t.Helper()
	lifecycleStore(t, s, 0, "n1")

	// Created and stopped while still requested: no run, no booted_at, and a
	// resume does not invent one.
	unclaimed, err := s.CreateSandbox(context.Background(), CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64, NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	if unclaimed.BootedAt != nil {
		t.Fatalf("a new sandbox has booted_at=%v", unclaimed.BootedAt)
	}
	stopped, err := s.StopSandbox(context.Background(), unclaimed.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != SandboxStopped || stopped.BootedAt != nil {
		t.Fatalf("stopped before it ran: state=%s booted_at=%v", stopped.State, stopped.BootedAt)
	}
	resumed, err := s.ResumeSandbox(context.Background(), unclaimed.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State != SandboxRequested || resumed.BootCount != 2 || resumed.BootedAt != nil {
		t.Fatalf("resumed before it ever ran: state=%s boot_count=%d booted_at=%v", resumed.State, resumed.BootCount, resumed.BootedAt)
	}
	// It reaches the node with booted_at absent, so the node gives it a fresh disk.
	if w := workOf(t, s, "n1"); !inWork(w, unclaimed.ID) {
		t.Fatalf("the resumed sandbox is not in the node's work: %+v", w)
	}

	// The first running report sets it; later ones do not move it.
	sb := runningOn(t, s, "n1")
	if sb.BootedAt == nil {
		t.Fatal("running did not set booted_at")
	}
	first := *sb.BootedAt
	if got := sandboxOf(t, s, sb.ID); got.BootedAt == nil || !got.BootedAt.Equal(first) {
		t.Fatalf("stored booted_at=%v, want %v", got.BootedAt, first)
	}
	stopping, err := s.StopSandbox(context.Background(), sb.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if stopping.BootedAt == nil || !stopping.BootedAt.Equal(first) {
		t.Fatalf("a stop cleared booted_at: %v", stopping.BootedAt)
	}
	report(t, s, sb.ID, SandboxStopped, "")
	again, err := s.ResumeSandbox(context.Background(), sb.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if again.BootedAt == nil || !again.BootedAt.Equal(first) || again.BootCount != 2 {
		t.Fatalf("resume of a sandbox that ran: booted_at=%v boot_count=%d", again.BootedAt, again.BootCount)
	}
	if _, err := s.ClaimSandbox(context.Background(), sb.ID, "n1"); err != nil && !isConflict(err) {
		t.Fatal(err)
	}
	if run2, err := s.UpdateSandboxStatus(context.Background(), sb.ID, SandboxRunning, ""); err != nil {
		t.Fatal(err)
	} else if run2.BootedAt == nil || !run2.BootedAt.Equal(first) {
		t.Fatalf("a second run moved booted_at to %v (first %v)", run2.BootedAt, first)
	}
}

func isConflict(err error) bool { return errors.Is(err, ErrConflict) }

func TestMemoryBootedAt(t *testing.T) { testBootedAt(t, NewMemoryStore()) }

func TestPostgresBootedAt(t *testing.T) { testBootedAt(t, newPostgresTestStore(t)) }
