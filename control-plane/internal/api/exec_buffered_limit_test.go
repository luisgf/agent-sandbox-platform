package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

func TestExecLimit(t *testing.T) {
	cases := []struct {
		name      string
		serverCap time.Duration
		asked     int
		stream    bool
		want      time.Duration
	}{
		{"the cap when nothing is asked", 10 * time.Minute, 0, false, 10 * time.Minute},
		{"a request below the cap", 10 * time.Minute, 120, false, 2 * time.Minute},
		{"a request above the cap is held to it", 10 * time.Minute, 99999, false, 10 * time.Minute},
		{"no cap: what is asked", 0, 30, false, 30 * time.Second},
		{"no cap and nothing asked: none", 0, 0, false, 0},
		{"a stream has none", 10 * time.Minute, 120, true, 0},
	}
	for _, tc := range cases {
		s := &Server{BufferedExecTimeout: tc.serverCap}
		if got := s.execLimit(tc.asked, tc.stream); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	if DefaultBufferedExecTimeout != 10*time.Minute {
		t.Fatalf("the default is %v", DefaultBufferedExecTimeout)
	}
	if NewServer(newBackend(t)).BufferedExecTimeout != DefaultBufferedExecTimeout {
		t.Fatal("NewServer does not use the default")
	}
}

type bufferedEnv struct {
	mux    http.Handler
	sbID   string
	mu     sync.Mutex
	bodies []map[string]any
	delay  time.Duration
}

func newBufferedEnv(t *testing.T, serverCap time.Duration) *bufferedEnv {
	t.Helper()
	e := &bufferedEnv{}
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		e.mu.Lock()
		e.bodies = append(e.bodies, body)
		delay := e.delay
		e.mu.Unlock()
		if r.URL.Query().Get("stream") == "1" {
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = io.WriteString(w, "{\"type\":\"exit\",\"exit_code\":0}\n")
			return
		}
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stdout":"ok","stderr":"","exit_code":0}`))
	}))
	t.Cleanup(agent.Close)
	mem := newBackend(t)
	mem.SetProvisionNodeID("n")
	if _, err := mem.RegisterNode(context.Background(), store.RegisterNodeInput{ID: "n", Name: "n", Endpoint: agent.URL, AgentEndpoint: agent.URL}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(mem)
	srv.Client = agent.Client()
	srv.BufferedExecTimeout = serverCap
	e.mux = testMux(srv)
	sb, err := mem.CreateSandbox(context.Background(), store.CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64, NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	runSandbox(t, mem, sb.ID)
	e.sbID = sb.ID
	return e
}

func (e *bufferedEnv) exec(query, body string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+e.sbID+"/exec"+query, strings.NewReader(body)))
	return rr
}

func (e *bufferedEnv) last() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.bodies[len(e.bodies)-1]
}

// The control plane tells the agent how long a buffered exec may run, so the
// guest's own timeout matches: what was asked, within the cap.
func TestBufferedExecForwardsItsTimeout(t *testing.T) {
	e := newBufferedEnv(t, 10*time.Minute)
	for _, tc := range []struct {
		query, body string
		want        float64
	}{
		{"", `{"cmd":["id"]}`, 600},
		{"", `{"cmd":["id"],"timeout_seconds":120}`, 120},
		{"", `{"cmd":["id"],"timeout_seconds":99999}`, 600},
		{"?stream=1", `{"cmd":["id"],"timeout_seconds":120}`, 0}, // a stream has no limit
	} {
		if rr := e.exec(tc.query, tc.body); rr.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", tc.query, tc.body, rr.Code, rr.Body.String())
		}
		if got, _ := e.last()["timeout_seconds"].(float64); got != tc.want {
			t.Errorf("%s %s: the agent got timeout_seconds=%v, want %v", tc.query, tc.body, got, tc.want)
		}
	}
	if rr := e.exec("", `{"cmd":["id"],"timeout_seconds":-1}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("a negative timeout: %d %s", rr.Code, rr.Body.String())
	}
}

// A buffered exec the control plane cuts is a 504 that says why and what to do,
// not a 502 blaming the node.
func TestBufferedExecTimeoutIsA504WithAdvice(t *testing.T) {
	e := newBufferedEnv(t, 300*time.Millisecond)
	e.delay = 3 * time.Second
	started := time.Now()
	rr := e.exec("", `{"cmd":["sleep","9"]}`)
	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"300ms", "buffered exec", "stream", "ASP_BUFFERED_EXEC_TIMEOUT"} {
		if !strings.Contains(body, want) {
			t.Errorf("the message lacks %q: %s", want, body)
		}
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("took %v", time.Since(started))
	}
}

// A request may ask for less than the cap, and that is what bounds the call.
func TestBufferedExecHonoursAShorterRequest(t *testing.T) {
	e := newBufferedEnv(t, 10*time.Minute)
	e.delay = 3 * time.Second
	started := time.Now()
	rr := e.exec("", `{"cmd":["sleep","9"],"timeout_seconds":1}`)
	if rr.Code != http.StatusGatewayTimeout || time.Since(started) > 2500*time.Millisecond {
		t.Fatalf("status %d after %v: %s", rr.Code, time.Since(started), rr.Body.String())
	}
}

// The client the control plane really uses to reach an agent gives the agent
// 30 s to start answering. A buffered exec is answered only when the command ends,
// so that wait must not cut it: this is the regression a test with a bare
// httptest client missed (the lab host answered 502 at 30 s).
func TestBufferedExecIsNotCutByTheResponseHeaderTimeout(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stream") == "1" {
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = io.WriteString(w, "{\"type\":\"exit\",\"exit_code\":0}\n")
			return
		}
		time.Sleep(700 * time.Millisecond) // the command takes longer than the header wait below
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stdout":"done","stderr":"","exit_code":0}`))
	}))
	defer agent.Close()
	mem := newBackend(t)
	mem.SetProvisionNodeID("n")
	if _, err := mem.RegisterNode(context.Background(), store.RegisterNodeInput{ID: "n", Name: "n", Endpoint: agent.URL, AgentEndpoint: agent.URL}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(mem)
	srv.Client = newAgentHTTPClient(AgentTimeouts{Dial: time.Second, TLSHandshake: time.Second, ResponseHeader: 200 * time.Millisecond})
	srv.BufferedExecTimeout = 10 * time.Second
	mux := testMux(srv)
	sb, err := mem.CreateSandbox(context.Background(), store.CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64, NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	runSandbox(t, mem, sb.ID)

	post := func(query string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/exec"+query, strings.NewReader(`{"cmd":["make"]}`)))
		return rr
	}
	if rr := post(""); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "done") {
		t.Fatalf("buffered exec behind a 200 ms header wait: %d %s", rr.Code, rr.Body.String())
	}
	// The twin is made once per client and reused.
	first := srv.bufferedTwin(srv.Client)
	if first == srv.Client || srv.bufferedTwin(srv.Client) != first {
		t.Fatal("the buffered twin is not cached")
	}
	// A stream still gets the wait: its agent answers at once.
	if rr := post("?stream=1"); rr.Code != http.StatusOK {
		t.Fatalf("stream: %d %s", rr.Code, rr.Body.String())
	}
	// A client with no such wait is its own twin.
	plain := &http.Client{Transport: &http.Transport{}}
	if srv.bufferedTwin(plain) != plain || srv.bufferedTwin(nil) != nil {
		t.Fatal("a client without a header wait was copied")
	}
}
