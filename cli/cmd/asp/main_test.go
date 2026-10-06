package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/client"
)

func TestSandboxRunLifecycle(t *testing.T) {
	var mu sync.Mutex
	state := "requested"
	deleted := false
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		var in client.CreateInput
		_ = json.NewDecoder(r.Body).Decode(&in)
		nid := in.NodeID
		sb := client.Sandbox{ID: "run-1", TenantID: in.TenantID, State: "requested", ImageRef: in.ImageRef}
		if nid != "" {
			sb.NodeID = &nid
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(sb)
	})
	mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if state == "requested" {
			state = "running"
		}
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: "run-1", State: state})
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(client.ExecResult{Stdout: "hello\n", ExitCode: 7})
	})
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		deleted = true
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: "run-1", State: "stopping"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var stdout, stderr strings.Builder
	code := run([]string{
		"sandbox", "run",
		"--cp-url", srv.URL,
		"--timeout", "2s",
		"--node-id", "n1",
		"--cmd", "echo hello",
	}, &stdout, &stderr)
	if code != 7 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "hello") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "created sandbox") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleted {
		t.Fatal("expected destroy on exit")
	}
}

func TestSandboxRunKeep(t *testing.T) {
	deleted := false
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: "k1", State: "running"})
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(client.ExecResult{Stdout: "ok\n", ExitCode: 0})
	})
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		deleted = true
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: "k1", State: "stopping"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var stdout, stderr strings.Builder
	code := run([]string{
		"sandbox", "run", "--cp-url", srv.URL, "--keep", "--", "true",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if deleted {
		t.Fatal("must not destroy with --keep")
	}
	if !strings.Contains(stderr.String(), "keeping") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestSandboxListSendsIDToken(t *testing.T) {
	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []any{}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	var stdout, stderr strings.Builder
	code := run([]string{
		"sandbox", "list",
		"--cp-url", srv.URL,
		"--id-token", "jwt-from-flag",
		"--tenant", "default",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if gotAuth != "Bearer jwt-from-flag" {
		t.Fatalf("auth=%q", gotAuth)
	}
}

// The usage puts the id first ("asp sandbox exec <id> --cmd '…'"). flag.Parse
// stopped at the id, so --cmd and the flags after it became the command.
func TestFlagsAfterTheID(t *testing.T) {
	t.Setenv("ASP_API_KEY", "")
	t.Setenv("ASP_ID_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	var mu sync.Mutex
	var gotID string
	var gotCmd []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		var req client.ExecRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		gotID, gotCmd = r.PathValue("id"), req.Cmd
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(client.ExecResult{Stdout: "ok\n"})
	})
	mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: r.PathValue("id"), State: "running"})
	})
	mux.HandleFunc("POST /v1/nodes/{id}/cordon", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"` + r.PathValue("id") + `","cordoned":true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"flags after the id", []string{"sandbox", "exec", "sb-1", "--cp-url", srv.URL, "--cmd", "echo hi"}, "echo hi"},
		{"flags before the id", []string{"sandbox", "exec", "--cp-url", srv.URL, "--cmd", "echo hi", "sb-1"}, "echo hi"},
		{"--name=value after the id", []string{"sandbox", "exec", "sb-1", "--cp-url=" + srv.URL, "-cmd=echo hi"}, "echo hi"},
		{"command after --", []string{"sandbox", "exec", "sb-1", "--cp-url", srv.URL, "--", "ls", "-la"}, "ls -la"},
		{"undelimited command keeps its flags", []string{"sandbox", "exec", "--cp-url", srv.URL, "sb-1", "ls", "-la"}, "ls -la"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			if code := run(tc.args, &stdout, &stderr); code != 0 {
				t.Fatalf("exit=%d stderr=%q", code, stderr.String())
			}
			mu.Lock()
			defer mu.Unlock()
			if gotID != "sb-1" || strings.Join(gotCmd, " ") != tc.want {
				t.Fatalf("exec id=%q cmd=%q, want sb-1 %q", gotID, gotCmd, tc.want)
			}
		})
	}

	var stdout, stderr strings.Builder
	if code := run([]string{"sandbox", "get", "sb-1", "--cp-url", srv.URL, "--json"}, &stdout, &stderr); code != 0 || !strings.HasPrefix(strings.TrimSpace(stdout.String()), "{") {
		t.Fatalf("get with --json after the id: exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if code := run([]string{"node", "cordon", "node-a", "--cp-url", srv.URL, "--json"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), `"cordoned": true`) {
		t.Fatalf("cordon with flags after the id: exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
