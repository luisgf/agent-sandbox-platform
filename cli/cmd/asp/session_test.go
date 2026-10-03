package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/client"
	"github.com/luisgf/agent-sandbox-platform/cli/internal/session"
)

func TestSessionStartExecStop(t *testing.T) {
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	t.Setenv("ASP_API_KEY", "")

	var mu sync.Mutex
	state := "requested"
	var creates atomic.Int32
	var execs atomic.Int32
	var deletes atomic.Int32
	var gotAuth string
	var gotCmd []string
	var gotCwd string

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		creates.Add(1)
		gotAuth = r.Header.Get("Authorization")
		var in client.CreateInput
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.TenantID != "acme" || in.ImageRef != "img:test" || in.VMMProfile != "fake" || in.NodeID != "n1" {
			t.Errorf("create input=%+v", in)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(client.Sandbox{
			ID: "sess-1", TenantID: in.TenantID, State: "requested", ImageRef: in.ImageRef,
			VMMProfile: in.VMMProfile,
		})
	})
	mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "sess-1" {
			t.Errorf("get id=%s", r.PathValue("id"))
		}
		mu.Lock()
		defer mu.Unlock()
		if state == "requested" {
			state = "running"
		}
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: "sess-1", TenantID: "acme", State: state, ImageRef: "img:test"})
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		execs.Add(1)
		if r.PathValue("id") != "sess-1" {
			t.Errorf("exec id=%s", r.PathValue("id"))
		}
		if got := r.Header.Get("Authorization"); got != "Bearer jwt-session" {
			t.Errorf("exec auth=%q", got)
		}
		var req client.ExecRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotCmd = append([]string{}, req.Cmd...)
		gotCwd = req.Cwd
		_ = json.NewEncoder(w).Encode(client.ExecResult{Stdout: "hello-session\n", Stderr: "warn\n", ExitCode: 3})
	})
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		deletes.Add(1)
		mu.Lock()
		state = "stopping"
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: "sess-1", State: "stopping"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sessFile := filepath.Join(t.TempDir(), "session.json")
	var stdout, stderr strings.Builder
	code := run([]string{
		"session", "start",
		"--cp-url", srv.URL,
		"--session-file", sessFile,
		"--tenant", "acme",
		"--image", "img:test",
		"--vmm-profile", "fake",
		"--node-id", "n1",
		"--timeout", "2s",
		"--id-token", "jwt-session",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("start exit=%d stderr=%q", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "sess-1" {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if gotAuth != "Bearer jwt-session" {
		t.Fatalf("create auth=%q", gotAuth)
	}
	fi, err := os.Stat(sessFile)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o", fi.Mode().Perm())
	}
	st, err := session.Load(sessFile)
	if err != nil {
		t.Fatal(err)
	}
	if st.SandboxID != "sess-1" || st.CPURL != srv.URL || st.TenantID != "acme" {
		t.Fatalf("state=%+v", st)
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{
		"session", "exec",
		"--session-file", sessFile,
		"--id-token", "jwt-session",
		"--cwd", "/work",
		"--cmd", "echo hello-session",
	}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exec exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.String() != "hello-session\n" {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "warn") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if strings.Join(gotCmd, " ") != "echo hello-session" || gotCwd != "/work" {
		t.Fatalf("cmd=%v cwd=%q", gotCmd, gotCwd)
	}
	if creates.Load() != 1 {
		t.Fatalf("creates=%d want 1 (exec must reuse)", creates.Load())
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{
		"session", "status",
		"--session-file", sessFile,
		"--id-token", "jwt-session",
		"--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("status exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"state": "running"`) && !strings.Contains(stdout.String(), `"state":"running"`) {
		t.Fatalf("status json=%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{
		"session", "stop",
		"--session-file", sessFile,
		"--id-token", "jwt-session",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("stop exit=%d stderr=%q", code, stderr.String())
	}
	if _, err := session.Load(sessFile); !errors.Is(err, session.ErrNoSession) {
		t.Fatalf("after stop: %v", err)
	}
	if deletes.Load() != 1 || execs.Load() != 1 {
		t.Fatalf("deletes=%d execs=%d", deletes.Load(), execs.Load())
	}
}

func TestSessionExecRequiresSession(t *testing.T) {
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	sessFile := filepath.Join(t.TempDir(), "missing.json")
	var stdout, stderr strings.Builder
	code := run([]string{
		"session", "exec",
		"--session-file", sessFile,
		"--cmd", "true",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no active session") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestSessionExecDashDash(t *testing.T) {
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		var req client.ExecRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Cmd) != 3 || req.Cmd[0] != "echo" || req.Cmd[1] != "a b" || req.Cmd[2] != "--flag" {
			t.Errorf("cmd=%v", req.Cmd)
		}
		_ = json.NewEncoder(w).Encode(client.ExecResult{Stdout: "a b\n", ExitCode: 0})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	sessFile := filepath.Join(t.TempDir(), "session.json")
	if err := session.Save(sessFile, session.State{SandboxID: "s2", CPURL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	code := run([]string{
		"session", "exec",
		"--session-file", sessFile,
		"--", "echo", "a b", "--flag",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "a b") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestSessionStartRefusesExistingUnlessForce(t *testing.T) {
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	var deletes atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		deletes.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"sandbox not found"}`))
	})
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: "new-1", State: "running", TenantID: "t"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	sessFile := filepath.Join(t.TempDir(), "session.json")
	if err := session.Save(sessFile, session.State{SandboxID: "old-1", CPURL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	code := run([]string{"session", "start", "--session-file", sessFile, "--cp-url", srv.URL}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "active session") {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"session", "start", "--force", "--session-file", sessFile, "--cp-url", srv.URL, "--timeout", "2s"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("force exit=%d stderr=%q", code, stderr.String())
	}
	if deletes.Load() != 1 {
		t.Fatalf("deletes=%d", deletes.Load())
	}
	st, err := session.Load(sessFile)
	if err != nil || st.SandboxID != "new-1" {
		t.Fatalf("state=%+v err=%v", st, err)
	}
}

func TestSessionStopKeepsFileOnAPIError(t *testing.T) {
	t.Setenv("ASP_IDP_REQUIRED", "")
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	sessFile := filepath.Join(t.TempDir(), "session.json")
	if err := session.Save(sessFile, session.State{SandboxID: "s3", CPURL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	code := run([]string{"session", "stop", "--session-file", sessFile}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "boom") {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	st, err := session.Load(sessFile)
	if err != nil || st.SandboxID != "s3" {
		t.Fatalf("state should remain: %+v %v", st, err)
	}
}

func TestSessionStatusAndExecIdleReaped(t *testing.T) {
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	var execs atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(client.Sandbox{
			ID: "gone", State: "stopping", TenantID: "acme", StopReason: client.StopReasonIdle,
		})
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		execs.Add(1)
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"sandbox was stopped after idle timeout (reaped)"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	sessFile := filepath.Join(t.TempDir(), "session.json")
	if err := session.Save(sessFile, session.State{SandboxID: "gone", CPURL: srv.URL, TenantID: "acme"}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	code := run([]string{"session", "status", "--session-file", sessFile}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "idle timeout") || !strings.Contains(stdout.String(), "idle_reaped=true") {
		t.Fatalf("status exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"session", "status", "--json", "--session-file", sessFile}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stdout.String(), `"idle_reaped": true`) {
		t.Fatalf("json status exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"session", "exec", "--session-file", sessFile, "--cmd", "true"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "idle timeout") {
		t.Fatalf("exec exit=%d stderr=%q", code, stderr.String())
	}
	if execs.Load() != 0 {
		t.Fatalf("exec should not be proxied when already reaped, execs=%d", execs.Load())
	}
}
