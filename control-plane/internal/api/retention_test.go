package api

import (
	"context"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

func TestRetentionConfigFromEnv(t *testing.T) {
	t.Setenv(store.EnvStoppedSandboxTTL, "")
	t.Setenv(store.EnvMaxStoppedPerTenant, "")
	t.Setenv(store.EnvRetentionSweep, "")
	cfg, err := RetentionConfigFromEnv()
	if err != nil || cfg.TTL != 7*24*time.Hour || cfg.MaxStoppedPerTenant != 0 || cfg.Interval != time.Minute || !cfg.Enabled() {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	t.Setenv(store.EnvStoppedSandboxTTL, "off")
	cfg, err = RetentionConfigFromEnv()
	if err != nil || cfg.Enabled() {
		t.Fatalf("ttl off, no cap: %+v %v", cfg, err)
	}
	t.Setenv(store.EnvMaxStoppedPerTenant, "3")
	t.Setenv(store.EnvRetentionSweep, "10s")
	cfg, err = RetentionConfigFromEnv()
	if err != nil || !cfg.Enabled() || cfg.MaxStoppedPerTenant != 3 || cfg.Interval != 10*time.Second {
		t.Fatalf("cap only: %+v %v", cfg, err)
	}
	t.Setenv(store.EnvStoppedSandboxTTL, "soon")
	if _, err := RetentionConfigFromEnv(); err == nil {
		t.Fatal("a bad ttl must not be ignored")
	}
	t.Setenv(store.EnvStoppedSandboxTTL, "7d")
	t.Setenv(store.EnvMaxStoppedPerTenant, "-2")
	if _, err := RetentionConfigFromEnv(); err == nil {
		t.Fatal("a bad cap must not be ignored")
	}
}

func stoppedFor(t *testing.T, mem *store.MemoryStore, ago time.Duration) string {
	t.Helper()
	id := newPlacedSandbox(t, mem)
	runSandbox(t, mem, id)
	if _, err := mem.StopSandbox(id, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.UpdateSandboxStatus(id, store.SandboxStopped, ""); err != nil {
		t.Fatal(err)
	}
	mem.SetStoppedAtForTest(id, time.Now().Add(-ago))
	return id
}

// One sweep applies the TTL and the cap.
func TestSweepRetention(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	mem := newTestStore(t, "n1")
	srv := NewServer(mem)
	old := stoppedFor(t, mem, 10*24*time.Hour)
	mid := stoppedFor(t, mem, 3*24*time.Hour)
	recent := stoppedFor(t, mem, time.Hour)

	srv.sweepRetention(time.Now().UTC(), RetentionConfig{TTL: 7 * 24 * time.Hour, MaxStoppedPerTenant: 1})
	state := func(id string) store.SandboxState {
		sb, _ := mem.GetSandbox(id)
		return sb.State
	}
	if state(old) != store.SandboxDeleting {
		t.Fatalf("the TTL did not delete %s: %s", old, state(old))
	}
	if state(mid) != store.SandboxDeleting {
		t.Fatalf("the cap (1) did not delete the older of the two left: %s", state(mid))
	}
	if state(recent) != store.SandboxStopped {
		t.Fatalf("the newest stopped sandbox was deleted: %s", state(recent))
	}
	if sb, _ := mem.GetSandbox(mid); sb.StopReason != store.StopReasonTenantCap {
		t.Fatalf("reason=%q", sb.StopReason)
	}
}

// With nothing bounded the reaper does not run; with something bounded it stops with the context.
func TestRunRetentionReaperLifecycle(t *testing.T) {
	srv := NewServer(newTestStore(t, "n1"))
	done := make(chan struct{})
	go func() { srv.RunRetentionReaper(context.Background(), RetentionConfig{}); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a disabled reaper must return at once")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() {
		srv.RunRetentionReaper(ctx, RetentionConfig{TTL: time.Hour, Interval: 10 * time.Millisecond})
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the reaper did not stop with its context")
	}
}
