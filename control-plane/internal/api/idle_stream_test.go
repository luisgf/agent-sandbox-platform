package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

func TestTouchEvery(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{
		0:                      0,
		-time.Second:           0,
		30 * time.Second:       10 * time.Second,
		2 * time.Hour:          time.Minute,
		3 * time.Minute:        time.Minute,
		90 * time.Second:       30 * time.Second,
		3 * time.Millisecond:   10 * time.Millisecond,
		600 * time.Millisecond: 200 * time.Millisecond,
	} {
		if got := touchEvery(in); got != want {
			t.Errorf("touchEvery(%v) = %v, want %v", in, got, want)
		}
	}
}

// A command that streams for longer than the idle timeout is activity the whole
// time it runs: the reaper must not stop its sandbox until the stream ends and
// the timeout has passed after it (#113).
func TestTheReaperSparesASandboxWithAnOpenStream(t *testing.T) {
	const idle = 600 * time.Millisecond
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "{\"type\":\"stdout\",\"data\":\"start\\n\"}\n")
		if fl != nil {
			fl.Flush()
		}
		once.Do(func() { close(started) })
		<-release
		_, _ = io.WriteString(w, "{\"type\":\"exit\",\"exit_code\":0}\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	defer agent.Close()
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })

	mem := newBackend(t)
	mem.SetProvisionNodeID("n")
	if _, err := mem.RegisterNode(context.Background(), store.RegisterNodeInput{ID: "n", Name: "n", Endpoint: agent.URL, AgentEndpoint: agent.URL}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(mem)
	srv.Client = agent.Client()
	srv.IdleTimeout = idle
	mux := testMux(srv)
	sb, err := mem.CreateSandbox(context.Background(), store.CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64, NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	runSandbox(t, mem, sb.ID)

	// A reaper sweeping every 50 ms while the command runs.
	stopSweep := make(chan struct{})
	swept := make(chan []store.Sandbox, 64)
	go func() {
		tk := time.NewTicker(50 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stopSweep:
				return
			case <-tk.C:
				if got, err := mem.StopIdleSandboxes(context.Background(), time.Now().UTC(), idle); err == nil && len(got) > 0 {
					swept <- got
				}
			}
		}
	}()
	defer close(stopSweep)

	done := make(chan string, 1)
	go func() {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/exec?stream=1", strings.NewReader(`{"cmd":["sleep","9"]}`)))
		done <- rr.Body.String()
	}()
	<-started
	time.Sleep(3 * idle) // three idle timeouts with the stream open
	select {
	case got := <-swept:
		t.Fatalf("the reaper stopped a sandbox with an open stream: %+v", got)
	default:
	}
	if cur, _ := mem.GetSandbox(context.Background(), sb.ID); cur.State != store.SandboxRunning {
		t.Fatalf("state=%s while the stream was open", cur.State)
	}

	releaseOnce.Do(func() { close(release) })
	select {
	case body := <-done:
		if !strings.Contains(body, `"exit"`) {
			t.Fatalf("stream body: %s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end")
	}
	// Once it ends, the timeout counts from the end: the reaper takes the sandbox.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-swept:
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("the sandbox was never reaped after its stream ended")
}

// With no idle timeout (reaper off) exec does not start a toucher.
func TestKeepActiveWithoutAReaperOnlyTouchesOnce(t *testing.T) {
	mem := newBackend(t)
	mem.SetProvisionNodeID("n")
	srv := NewServer(mem)
	stop := srv.keepActive(context.Background(), "no-such-sandbox") // an unknown sandbox is not an error
	stop()
	stop() // stopping twice is fine
}
