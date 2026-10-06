package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

type execStateFixture struct {
	t     *testing.T
	mem   *store.MemoryStore
	mux   http.Handler
	calls *atomic.Int32
}

func newExecStateFixture(t *testing.T, agent http.HandlerFunc) *execStateFixture {
	t.Helper()
	calls := &atomic.Int32{}
	srvAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		agent(w, r)
	}))
	t.Cleanup(srvAgent.Close)
	mem := store.NewMemoryStore()
	if _, err := mem.RegisterNode(store.RegisterNodeInput{ID: "n", AgentEndpoint: srvAgent.URL}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(mem)
	srv.Client = srvAgent.Client()
	return &execStateFixture{t: t, mem: mem, mux: testMux(srv), calls: calls}
}

func (f *execStateFixture) sandbox() string {
	f.t.Helper()
	sb, err := f.mem.CreateSandbox(store.CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64, NodeID: "n"})
	if err != nil {
		f.t.Fatal(err)
	}
	return sb.ID
}

func (f *execStateFixture) post(id, path string) *httptest.ResponseRecorder {
	f.t.Helper()
	body := `{"cmd":["true"]}`
	if path == "/exec/stdin" {
		body = `{"exec_id":"e1","data":"x"}`
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+id+path, bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	f.mux.ServeHTTP(rr, req)
	return rr
}

func okAgent(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"stdout":"","stderr":"","exit_code":0}`))
}

// A sandbox that is not running has no guest yet (or any more): exec answers
// 409 before calling the node agent, instead of a 502 from an agent that does
// not know the sandbox.
func TestExecRefusesSandboxesThatAreNotRunning(t *testing.T) {
	f := newExecStateFixture(t, okAgent)
	requested := f.sandbox()
	starting := f.sandbox()
	if _, err := f.mem.ClaimSandbox(starting, "n"); err != nil {
		t.Fatal(err)
	}
	paused := f.sandbox()
	runSandbox(t, f.mem, paused)
	f.mem.SetStateForTest(paused, store.SandboxPaused)

	for name, id := range map[string]string{"requested": requested, "starting": starting, "paused": paused} {
		for _, path := range []string{"/exec", "/exec/stdin"} {
			rr := f.post(id, path)
			if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "sandbox is "+name) {
				t.Errorf("%s %s: want 409 naming the state, got %d %s", name, path, rr.Code, rr.Body.String())
			}
		}
	}
	if n := f.calls.Load(); n != 0 {
		t.Fatalf("the node agent got %d calls for sandboxes that are not running", n)
	}

	running := f.sandbox()
	runSandbox(t, f.mem, running)
	if rr := f.post(running, "/exec"); rr.Code != http.StatusOK {
		t.Fatalf("running sandbox: exec %d %s", rr.Code, rr.Body.String())
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("running sandbox: want 1 agent call, got %d", n)
	}
}

// The agent answers 404 when it has no guest for the sandbox: that is a state
// conflict for the caller, not a gateway error.
func TestExecMapsAgentNotFoundToConflict(t *testing.T) {
	f := newExecStateFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "no pod-daemon endpoint for sandbox x")
	})
	id := f.sandbox()
	runSandbox(t, f.mem, id)
	for _, path := range []string{"/exec", "/exec/stdin"} {
		rr := f.post(id, path)
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "not running on its node") {
			t.Errorf("%s: want 409, got %d %s", path, rr.Code, rr.Body.String())
		}
	}
}

func TestExecKeepsBadGatewayForOtherAgentErrors(t *testing.T) {
	f := newExecStateFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusServiceUnavailable, "pod-daemon client not configured")
	})
	id := f.sandbox()
	runSandbox(t, f.mem, id)
	if rr := f.post(id, "/exec"); rr.Code != http.StatusBadGateway {
		t.Fatalf("agent 503: want 502, got %d %s", rr.Code, rr.Body.String())
	}
}
