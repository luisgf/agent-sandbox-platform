package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

func TestExecRejectsIdleReapedSandbox(t *testing.T) {
	mem := newTestStore(t, "n")
	sb, err := mem.CreateSandbox(context.Background(), store.CreateSandboxInput{
		TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64, NodeID: "n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mem.UpdateSandboxStatus(context.Background(), sb.ID, store.SandboxRunning, ""); err != nil {
		t.Fatal(err)
	}
	mem.SetLastActivityForTest(sb.ID, time.Now().UTC().Add(-3*time.Hour))
	reaped, err := mem.StopIdleSandboxes(context.Background(), time.Now().UTC(), 2*time.Hour)
	if err != nil || len(reaped) != 1 {
		t.Fatalf("reap: %v %+v", err, reaped)
	}
	srv := NewServer(mem)
	mux := testMux(srv)
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/exec", bytes.NewBufferString(`{"cmd":["true"]}`))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "idle timeout") {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestExecTouchesActivity(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stdout":"ok","stderr":"","exit_code":7}`))
	}))
	defer agent.Close()

	mem := newBackend(t)
	if _, err := mem.RegisterNode(context.Background(), store.RegisterNodeInput{
		ID: "n", Name: "n", Endpoint: agent.URL, AgentEndpoint: agent.URL,
	}); err != nil {
		t.Fatal(err)
	}
	sb, err := mem.CreateSandbox(context.Background(), store.CreateSandboxInput{
		TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64, NodeID: "n",
	})
	if err != nil {
		t.Fatal(err)
	}
	runSandbox(t, mem, sb.ID)
	past := time.Now().UTC().Add(-30 * time.Minute)
	mem.SetLastActivityForTest(sb.ID, past)

	srv := NewServer(mem)
	srv.Client = agent.Client()
	mux := testMux(srv)
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/exec", bytes.NewBufferString(`{"cmd":["true"]}`))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("exec=%d %s", rr.Code, rr.Body.String())
	}
	var out execResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 7 {
		t.Fatalf("exit=%d", out.ExitCode)
	}
	got, err := mem.GetSandbox(context.Background(), sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastActivityAt.After(past) {
		t.Fatalf("activity not refreshed: %s", got.LastActivityAt)
	}
}
