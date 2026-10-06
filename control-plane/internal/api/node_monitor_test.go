package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/fence"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

var monCfg = NodeMonitorConfig{Interval: 15 * time.Second, StaleAfter: 90 * time.Second, FailoverAfter: 5 * time.Minute}

// monitorFixture: nodes n1 and n2 with one running sandbox each.
func monitorFixture(t *testing.T) (*store.MemoryStore, *Server, map[string]string) {
	t.Helper()
	t.Setenv("ASP_AUTO_PROVISION", "0")
	mem := newTestStore(t, "n1", "n2")
	ids := map[string]string{}
	for _, n := range []string{"n1", "n2"} {
		sb, err := mem.CreateSandbox(store.CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64, NodeID: n})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := mem.ClaimSandbox(sb.ID, n); err != nil {
			t.Fatal(err)
		}
		if _, err := mem.UpdateSandboxStatus(sb.ID, store.SandboxRunning, ""); err != nil {
			t.Fatal(err)
		}
		ids[n] = sb.ID
	}
	return mem, NewServer(mem), ids
}

func state(t *testing.T, mem *store.MemoryStore, id string) store.Sandbox {
	t.Helper()
	sb, err := mem.GetSandbox(id)
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

func TestNodeMonitorMarksOfflineThenFailsSandboxes(t *testing.T) {
	mem, srv, ids := monitorFixture(t)
	now := time.Now().UTC()
	started := now.Add(-time.Hour)

	mem.SetNodeLastSeenForTest("n1", now.Add(-2*time.Minute)) // stale, not yet lost
	srv.sweepNodes(context.Background(), now, started, monCfg)
	if n, _ := mem.GetNode("n1"); n.State != "offline" {
		t.Fatalf("n1 after the stale window: %s", n.State)
	}
	if sb := state(t, mem, ids["n1"]); sb.State != store.SandboxRunning {
		t.Fatalf("sandbox failed before the failover delay: %s", sb.State)
	}

	mem.SetNodeLastSeenForTest("n1", now.Add(-6*time.Minute)) // lost
	srv.sweepNodes(context.Background(), now, started, monCfg)
	if sb := state(t, mem, ids["n1"]); sb.State != store.SandboxFailed || sb.StopReason != store.StopReasonNodeLost {
		t.Fatalf("lost node's sandbox: %s/%s", sb.State, sb.StopReason)
	}
	if sb := state(t, mem, ids["n2"]); sb.State != store.SandboxRunning {
		t.Fatalf("healthy node's sandbox changed: %s", sb.State)
	}
	if n, _ := mem.GetNode("n2"); n.State != "ready" {
		t.Fatalf("healthy node went %s", n.State)
	}

	// Exec on the lost sandbox explains what happened.
	rr := httptest.NewRecorder()
	testMux(srv).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+ids["n1"]+"/exec", bytes.NewBufferString(`{"cmd":["true"]}`)))
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "lost with its node") {
		t.Fatalf("exec on a lost sandbox: %d %s", rr.Code, rr.Body.String())
	}
}

// After a control-plane restart, old last_seen values do not fail everything at once.
func TestNodeMonitorGivesGraceAfterStart(t *testing.T) {
	mem, srv, ids := monitorFixture(t)
	now := time.Now().UTC()
	started := now.Add(-30 * time.Second)
	mem.SetNodeLastSeenForTest("n1", now.Add(-time.Hour))

	srv.sweepNodes(context.Background(), now, started, monCfg)
	if n, _ := mem.GetNode("n1"); n.State == "offline" {
		t.Fatal("marked offline within the grace after start")
	}
	if sb := state(t, mem, ids["n1"]); sb.State != store.SandboxRunning {
		t.Fatalf("failed within the grace after start: %s", sb.State)
	}

	later := started.Add(6 * time.Minute) // still silent
	srv.sweepNodes(context.Background(), later, started, monCfg)
	if sb := state(t, mem, ids["n1"]); sb.State != store.SandboxFailed {
		t.Fatalf("still silent after the grace: %s", sb.State)
	}
}

func TestNodeMonitorFailoverOffStillHandlesRevokedNodes(t *testing.T) {
	mem, srv, ids := monitorFixture(t)
	now := time.Now().UTC()
	cfg := monCfg
	cfg.FailoverAfter = 0
	mem.SetNodeLastSeenForTest("n1", now.Add(-time.Hour))

	srv.sweepNodes(context.Background(), now, now.Add(-2*time.Hour), cfg)
	if n, _ := mem.GetNode("n1"); n.State != "offline" {
		t.Fatalf("n1 should be offline: %s", n.State)
	}
	if sb := state(t, mem, ids["n1"]); sb.State != store.SandboxRunning {
		t.Fatalf("failover is off, the sandbox must stay: %s", sb.State)
	}

	if _, err := mem.RevokeNode("n2"); err != nil {
		t.Fatal(err)
	}
	srv.sweepNodes(context.Background(), now, now.Add(-2*time.Hour), cfg)
	if sb := state(t, mem, ids["n2"]); sb.State != store.SandboxFailed {
		t.Fatalf("a revoked node's sandbox must fail at once: %s", sb.State)
	}
}

func TestNodeMonitorConfigFromEnv(t *testing.T) {
	cfg, err := NodeMonitorConfigFromEnv(90 * time.Second)
	if err != nil || cfg.Interval != 15*time.Second || cfg.FailoverAfter != 5*time.Minute {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	t.Setenv(EnvNodeFailoverAfter, "off")
	if cfg, err := NodeMonitorConfigFromEnv(90 * time.Second); err != nil || cfg.FailoverAfter != 0 {
		t.Fatalf("off: %+v %v", cfg, err)
	}
	t.Setenv(EnvNodeFailoverAfter, "30s")
	if _, err := NodeMonitorConfigFromEnv(90 * time.Second); err == nil {
		t.Fatal("failover shorter than the stale window must be rejected")
	}
	t.Setenv(EnvNodeFailoverAfter, "")
	t.Setenv(EnvNodeMonitorInterval, "2m")
	if _, err := NodeMonitorConfigFromEnv(90 * time.Second); err == nil {
		t.Fatal("a stale window shorter than the interval must be rejected")
	}
	t.Setenv(EnvNodeMonitorInterval, "nope")
	if _, err := NodeMonitorConfigFromEnv(90 * time.Second); err == nil {
		t.Fatal("an invalid interval must be rejected")
	}
}

func TestNodeMonitorFencesALostNodeOncePerOutage(t *testing.T) {
	t.Setenv("ASP_FENCE_PROVIDER", "http_webhook")
	var hits []string
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()

	t.Setenv("ASP_AUTO_PROVISION", "0")
	mem := store.NewMemoryStore()
	for _, id := range []string{"busy", "idle"} {
		if _, err := mem.RegisterNode(store.RegisterNodeInput{
			ID: id, AgentEndpoint: "http://127.0.0.1:9100", FenceEndpoint: webhook.URL, FenceToken: "tok-" + id,
		}); err != nil {
			t.Fatal(err)
		}
	}
	sb, err := mem.CreateSandbox(store.CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64, NodeID: "busy"})
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(mem)
	srv.Fence = fence.FromEnv()
	now := time.Now().UTC()
	for _, id := range []string{"busy", "idle"} {
		mem.SetNodeLastSeenForTest(id, now.Add(-time.Hour))
	}

	srv.sweepNodes(context.Background(), now, now.Add(-2*time.Hour), monCfg)
	if len(hits) != 1 || hits[0] != "Bearer tok-busy" {
		t.Fatalf("fence calls = %v, want one for the node with sandboxes", hits)
	}
	if got := state(t, mem, sb.ID); got.State != store.SandboxFailed {
		t.Fatalf("sandbox after fencing: %s", got.State)
	}
	srv.sweepNodes(context.Background(), now.Add(time.Minute), now.Add(-2*time.Hour), monCfg)
	if len(hits) != 1 {
		t.Fatalf("fenced again in the same outage: %v", hits)
	}
}
