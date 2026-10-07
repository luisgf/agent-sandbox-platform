package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/client"
	"github.com/luisgf/agent-sandbox-platform/cli/internal/session"
)

func TestExecSeconds(t *testing.T) {
	for in, want := range map[time.Duration]int{0: 0, -time.Second: 0, time.Second: 1, 1500 * time.Millisecond: 2, 90 * time.Second: 90, 2 * time.Hour: 7200} {
		if got := execSeconds(in); got != want {
			t.Errorf("execSeconds(%v) = %d, want %d", in, got, want)
		}
	}
}

// --exec-timeout travels as timeout_seconds on the commands that run a buffered
// exec, and is absent when not given.
func TestExecTimeoutFlagReachesTheRequest(t *testing.T) {
	t.Setenv("ASP_API_KEY", "")
	t.Setenv("ASP_ID_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	var mu sync.Mutex
	var asked []int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		var req client.ExecRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		asked = append(asked, req.TimeoutSeconds)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(client.ExecResult{Stdout: "ok\n"})
	})
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: "sb-run", State: "running", TenantID: "t"})
	})
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: r.PathValue("id"), State: "deleting"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	sessFile := filepath.Join(t.TempDir(), "session.json")
	if err := session.Save(sessFile, session.State{SandboxID: "sb-1", CPURL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"sandbox exec", []string{"sandbox", "exec", "sb-1", "--cp-url", srv.URL, "--cmd", "id"}, 0},
		{"sandbox exec --exec-timeout", []string{"sandbox", "exec", "sb-1", "--cp-url", srv.URL, "--exec-timeout", "45m", "--cmd", "id"}, 2700},
		{"session exec --buffered --exec-timeout", []string{"session", "exec", "--buffered", "--exec-timeout", "90s", "--session-file", sessFile, "--", "id"}, 90},
		{"sandbox run --exec-timeout", []string{"sandbox", "run", "--cp-url", srv.URL, "--exec-timeout", "2m", "--cmd", "id"}, 120},
	} {
		mu.Lock()
		asked = nil
		mu.Unlock()
		var stdout, stderr strings.Builder
		if code := run(tc.args, &stdout, &stderr); code != 0 {
			t.Fatalf("%s: exit=%d stderr=%q", tc.name, code, stderr.String())
		}
		mu.Lock()
		if len(asked) != 1 || asked[0] != tc.want {
			t.Errorf("%s: timeout_seconds=%v, want %d", tc.name, asked, tc.want)
		}
		mu.Unlock()
	}
}

// run tells what happens to a command whose exec failed: the sandbox goes with it.
func TestRunSaysTheSandboxIsDestroyedAfterAFailedExec(t *testing.T) {
	t.Setenv("ASP_API_KEY", "")
	t.Setenv("ASP_ID_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGatewayTimeout)
		_, _ = w.Write([]byte(`{"error":"exec exceeded the 10m0s limit of a buffered exec"}`))
	})
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: "sb-run", State: "running", TenantID: "t"})
	})
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: r.PathValue("id"), State: "deleting"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	var stdout, stderr strings.Builder
	code := run([]string{"sandbox", "run", "--cp-url", srv.URL, "--cmd", "sleep 9999"}, &stdout, &stderr)
	out := stderr.String()
	if code != 1 || !strings.Contains(out, "limit of a buffered exec") || !strings.Contains(out, "destroyed with its command") || !strings.Contains(out, "--keep") || !strings.Contains(out, "asp session exec streams") {
		t.Fatalf("exit=%d stderr=%q", code, out)
	}
	// With --keep nothing is destroyed and the extra advice is not shown.
	stderr.Reset()
	code = run([]string{"sandbox", "run", "--cp-url", srv.URL, "--keep", "--cmd", "sleep 9999"}, &stdout, &stderr)
	if code != 1 || strings.Contains(stderr.String(), "destroyed with its command") || !strings.Contains(stderr.String(), "keeping sandbox") {
		t.Fatalf("--keep: exit=%d stderr=%q", code, stderr.String())
	}
}
