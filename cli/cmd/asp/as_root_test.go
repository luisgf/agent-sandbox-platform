package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/client"
	"github.com/luisgf/agent-sandbox-platform/cli/internal/session"
)

// --root asks for root in the guest on every command that execs; without it the
// request does not mention root, and the guest runs the command as the owner of
// the workspace (or its default user).
func TestRootFlagReachesTheExecRequest(t *testing.T) {
	t.Setenv("ASP_API_KEY", "")
	t.Setenv("ASP_ID_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	var mu sync.Mutex
	var asked []bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		var req client.ExecRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		asked = append(asked, req.AsRoot)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(client.ExecResult{Stdout: "ok\n"})
	})
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: "sb-run", State: "running", TenantID: "t"})
	})
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: r.PathValue("id"), State: "stopping"})
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
		want bool
	}{
		{"sandbox exec", []string{"sandbox", "exec", "sb-1", "--cp-url", srv.URL, "--cmd", "id"}, false},
		{"sandbox exec --root", []string{"sandbox", "exec", "sb-1", "--cp-url", srv.URL, "--root", "--cmd", "id"}, true},
		{"session exec", []string{"session", "exec", "--buffered", "--session-file", sessFile, "--", "id"}, false},
		{"session exec --root", []string{"session", "exec", "--buffered", "--root", "--session-file", sessFile, "--", "id"}, true},
		{"sandbox run", []string{"sandbox", "run", "--cp-url", srv.URL, "--cmd", "id"}, false},
		{"sandbox run --root", []string{"sandbox", "run", "--cp-url", srv.URL, "--root", "--cmd", "id"}, true},
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
			t.Errorf("%s: as_root=%v, want %v", tc.name, asked, tc.want)
		}
		mu.Unlock()
	}
}
