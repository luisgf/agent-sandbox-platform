package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

func stoppedFor(t *testing.T, mem *backend, ago time.Duration) string {
	t.Helper()
	id := newPlacedSandbox(t, mem)
	runSandbox(t, mem, id)
	if _, err := mem.StopSandbox(context.Background(), id, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.UpdateSandboxStatus(context.Background(), id, store.SandboxStopped, ""); err != nil {
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

	srv.sweepRetention(context.Background(), time.Now().UTC(), RetentionConfig{TTL: 7 * 24 * time.Hour, MaxStoppedPerTenant: 1})
	state := func(id string) store.SandboxState {
		sb, _ := mem.GetSandbox(context.Background(), id)
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
	if sb, _ := mem.GetSandbox(context.Background(), mid); sb.StopReason != store.StopReasonTenantCap {
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

func nodeViewOf(t *testing.T, h http.Handler, id string) nodeView {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/nodes", nil))
	var list listNodesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("%v: %s", err, rr.Body.String())
	}
	for _, n := range list.Nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("node %s not listed: %s", id, rr.Body.String())
	return nodeView{}
}

// The node list shows how many stopped sandboxes keep a disk on each node, with
// none of the CPU or memory they used to hold, and the free disk the node reports.
func TestNodeListShowsRetainedDisksAndFreeDisk(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	mem := newTestStore(t, "n1")
	h := testMux(NewServer(mem))

	if n := nodeViewOf(t, h, "n1"); n.StoppedSandboxes != 0 || n.DiskFreeMiB != nil {
		t.Fatalf("a fresh node: stopped=%d disk=%v", n.StoppedSandboxes, n.DiskFreeMiB)
	}
	id := stoppedFor(t, mem, time.Minute)
	n := nodeViewOf(t, h, "n1")
	if n.StoppedSandboxes != 1 || n.Allocated.Sandboxes != 0 || n.Allocated.CPUMillis != 0 || n.Allocated.MemoryMiB != 0 {
		t.Fatalf("one stopped sandbox: stopped=%d allocated=%+v (it holds a disk, not CPU or memory)", n.StoppedSandboxes, n.Allocated)
	}
	if _, err := mem.ResumeSandbox(context.Background(), id, ""); err != nil {
		t.Fatal(err)
	}
	if n := nodeViewOf(t, h, "n1"); n.StoppedSandboxes != 0 || n.Allocated.Sandboxes != 1 {
		t.Fatalf("after the resume: stopped=%d allocated=%+v", n.StoppedSandboxes, n.Allocated)
	}

	// The heartbeat may carry the free disk space; without a body it still works.
	beat := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/nodes/n1/heartbeat", strings.NewReader(body))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	if code := beat(""); code != http.StatusOK {
		t.Fatalf("heartbeat without a body: %d", code)
	}
	if n := nodeViewOf(t, h, "n1"); n.DiskFreeMiB != nil {
		t.Fatalf("no body, no disk figure: %v", *n.DiskFreeMiB)
	}
	if code := beat(`{"disk_free_mib": 524288}`); code != http.StatusOK {
		t.Fatalf("heartbeat with disk: %d", code)
	}
	if n := nodeViewOf(t, h, "n1"); n.DiskFreeMiB == nil || *n.DiskFreeMiB != 524288 {
		t.Fatalf("disk figure: %v", n.DiskFreeMiB)
	}
	if code := beat(`{"disk_free_mib": -5}`); code != http.StatusOK {
		t.Fatalf("a negative figure is ignored, not an error: %d", code)
	}
	if n := nodeViewOf(t, h, "n1"); *n.DiskFreeMiB != 524288 {
		t.Fatalf("a negative figure replaced the real one: %d", *n.DiskFreeMiB)
	}
	if code := beat(`{not json`); code != http.StatusBadRequest {
		t.Fatalf("a malformed body: want 400, got %d", code)
	}
}
