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

func TestSessionStartExecRemove(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
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
		"--control-plane-url", srv.URL,
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
		"--buffered",
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
		"session", "rm",
		"--session-file", sessFile,
		"--id-token", "jwt-session",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("rm exit=%d stderr=%q", code, stderr.String())
	}
	if _, err := session.Load(sessFile); !errors.Is(err, session.ErrNoSession) {
		t.Fatalf("after rm: %v", err)
	}
	if deletes.Load() != 1 || execs.Load() != 1 {
		t.Fatalf("deletes=%d execs=%d", deletes.Load(), execs.Load())
	}
}

func TestSessionExecRequiresSession(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	sessFile := filepath.Join(t.TempDir(), "missing.json")
	var stdout, stderr strings.Builder
	code := run([]string{
		"session", "exec",
		"--buffered",
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
	t.Setenv("ASP_REQUIRE_TOKEN", "")
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
		"--buffered",
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
	t.Setenv("ASP_REQUIRE_TOKEN", "")
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
	code := run([]string{"session", "start", "--session-file", sessFile, "--control-plane-url", srv.URL}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "active session") {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"session", "start", "--force", "--session-file", sessFile, "--control-plane-url", srv.URL, "--timeout", "2s"}, &stdout, &stderr)
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

func TestSessionRemoveKeepsFileOnAPIError(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
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
	code := run([]string{"session", "rm", "--session-file", sessFile}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "boom") {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	st, err := session.Load(sessFile)
	if err != nil || st.SandboxID != "s3" {
		t.Fatalf("state should remain: %+v %v", st, err)
	}
}

func TestSessionStatusAndExecIdleReaped(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
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
	code = run([]string{"session", "exec", "--buffered", "--session-file", sessFile, "--cmd", "true"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "idle timeout") {
		t.Fatalf("exec exit=%d stderr=%q", code, stderr.String())
	}
	if execs.Load() != 1 {
		t.Fatalf("exec should be sent once and explained from the 409, execs=%d", execs.Load())
	}
}

func TestNamedSessionsAreIndependent(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	t.Setenv("ASP_SESSION_FILE", "")
	dir := t.TempDir()
	var mu sync.Mutex
	ids := map[string]string{}
	mux := http.NewServeMux()
	var n atomic.Int32
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		var in client.CreateInput
		_ = json.NewDecoder(r.Body).Decode(&in)
		id := fmtID(n.Add(1))
		mu.Lock()
		ids[id] = in.WorkspaceHostPath
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: id, State: "running", TenantID: in.TenantID, ImageRef: in.ImageRef, WorkspaceHostPath: in.WorkspaceHostPath})
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stream") == "1" {
			t.Errorf("buffered exec should not set stream, raw=%s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(client.ExecResult{Stdout: r.PathValue("id") + "\n", ExitCode: 0})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ws := t.TempDir()
	var stdout, stderr strings.Builder
	code := run([]string{
		"session", "start", "--name", "alpha", "--session-dir", dir,
		"--control-plane-url", srv.URL, "--timeout", "2s", "--workspace", ws,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("alpha start exit=%d stderr=%q", code, stderr.String())
	}
	alphaID := strings.TrimSpace(stdout.String())
	stdout.Reset()
	stderr.Reset()
	code = run([]string{
		"session", "start", "--name", "beta", "--session-dir", dir,
		"--control-plane-url", srv.URL, "--timeout", "2s",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("beta start exit=%d stderr=%q", code, stderr.String())
	}
	betaID := strings.TrimSpace(stdout.String())
	if alphaID == "" || betaID == "" || alphaID == betaID {
		t.Fatalf("ids alpha=%q beta=%q", alphaID, betaID)
	}
	st, err := session.Load(filepath.Join(dir, "alpha.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Name != "alpha" || st.SandboxID != alphaID || st.Workspace != ws {
		t.Fatalf("alpha state=%+v", st)
	}
	if _, err := session.Load(filepath.Join(dir, "default.json")); !errors.Is(err, session.ErrNoSession) {
		t.Fatalf("default should be absent: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = run([]string{
		"session", "exec", "--buffered", "--name", "beta", "--session-dir", dir, "--cmd", "true",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("beta exec exit=%d stderr=%q", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != betaID {
		t.Fatalf("exec stdout=%q want %s", stdout.String(), betaID)
	}
	mu.Lock()
	gotWS := ids[alphaID]
	mu.Unlock()
	if gotWS != ws {
		t.Fatalf("create workspace=%q want %s", gotWS, ws)
	}
}

func fmtID(n int32) string { return "sb-" + string(rune('0'+n)) }

func TestSessionExecStreamsAsChunksArrive(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	t.Setenv("ASP_SESSION_FILE", "")
	dir := t.TempDir()
	if err := session.Save(filepath.Join(dir, "default.json"), session.State{
		Name: "default", SandboxID: "live", CPURL: "placeholder",
	}); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: "live", State: "running"})
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stream") != "1" {
			t.Errorf("query=%s", r.URL.RawQuery)
		}
		if acc := r.Header.Get("Accept"); !strings.Contains(acc, "application/x-ndjson") {
			t.Errorf("accept=%q", acc)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		fl, _ := w.(http.Flusher)
		_, _ = ioWrite(w, "{\"type\":\"stdout\",\"data\":\"one\\n\"}\n")
		if fl != nil {
			fl.Flush()
		}
		close(started)
		<-release
		_, _ = ioWrite(w, "{\"type\":\"stderr\",\"data\":\"err\\n\"}\n")
		_, _ = ioWrite(w, "{\"type\":\"exit\",\"exit_code\":4}\n")
		if fl != nil {
			fl.Flush()
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	st, err := session.Load(filepath.Join(dir, "default.json"))
	if err != nil {
		t.Fatal(err)
	}
	st.CPURL = srv.URL
	if err := session.Save(filepath.Join(dir, "default.json"), st); err != nil {
		t.Fatal(err)
	}

	out := &chunkWriter{got: make(chan string, 4)}
	errOut := &chunkWriter{got: make(chan string, 4)}
	done := make(chan int, 1)
	go func() {
		done <- run([]string{
			"session", "exec", "--session-dir", dir, "--cmd", "echo one",
		}, out, errOut)
	}()
	select {
	case <-started:
	case code := <-done:
		t.Fatalf("exec returned %d before first chunk", code)
	}
	select {
	case chunk := <-out.got:
		if !strings.Contains(chunk, "one") {
			t.Fatalf("first stdout chunk=%q", chunk)
		}
	case code := <-done:
		t.Fatalf("exec returned %d before stdout chunk", code)
	}
	close(release)
	code := <-done
	if code != 4 {
		t.Fatalf("exit=%d", code)
	}
	select {
	case chunk := <-errOut.got:
		if !strings.Contains(chunk, "err") {
			t.Fatalf("stderr chunk=%q", chunk)
		}
	default:
		// stderr may already have been consumed if the channel filled; read buffer.
		if !strings.Contains(errOut.String(), "err") {
			t.Fatalf("stderr=%q", errOut.String())
		}
	}
}

type chunkWriter struct {
	mu  sync.Mutex
	buf strings.Builder
	got chan string
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if w.got != nil {
		select {
		case w.got <- string(p):
		default:
		}
	}
	return n, err
}

func (w *chunkWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func ioWrite(w interface{ Write([]byte) (int, error) }, s string) (int, error) {
	return w.Write([]byte(s))
}

func TestSessionLocalNetFlagAndHandshake(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	t.Setenv("ASP_API_KEY", "")

	var gotLocal *bool
	var heartbeats int
	var detaches int
	pubSeen := ""

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		var in client.CreateInput
		_ = json.NewDecoder(r.Body).Decode(&in)
		gotLocal = in.LocalNet
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(client.Sandbox{
			ID: "ln-1", TenantID: in.TenantID, State: "running", ImageRef: in.ImageRef,
			LocalNet: true, LocalNetState: "pending",
		})
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/local-net/grant", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-ASP-Caller") == "guest" {
			http.Error(w, "guest", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"grant": "grant-clear-not-for-disk", "dial": "", "expires_at": "2026-10-03T18:00:00Z",
			"tunnel_iface": "wg-asp-ln-1", "transport": "wireguard-skeleton",
		})
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/local-net/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		heartbeats++
		var body struct {
			Grant string `json:"grant"`
			Pub   string `json:"client_public_key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Grant != "grant-clear-not-for-disk" || body.Pub == "" {
			t.Errorf("heartbeat body grant=%q pub=%q", body.Grant, body.Pub)
		}
		pubSeen = body.Pub
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: "ln-1", State: "running", LocalNet: true, LocalNetState: "up"})
	})
	mux.HandleFunc("DELETE /v1/sandboxes/{id}/local-net/attach", func(w http.ResponseWriter, r *http.Request) {
		detaches++
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: "ln-1", State: "running", LocalNet: true, LocalNetState: "withdrawn"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	sess := filepath.Join(dir, "s.json")
	var stdout, stderr strings.Builder
	code := run([]string{"session", "start", "--control-plane-url", srv.URL, "--session-file", sess, "--tenant", "acme", "--image", "img", "--local-net", "--timeout", "2s"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("start %d %s", code, stderr.String())
	}
	if gotLocal == nil || !*gotLocal {
		t.Fatalf("create local_net=%v", gotLocal)
	}
	st, err := session.Load(sess)
	if err != nil || !st.LocalNet {
		t.Fatalf("session state %+v %v", st, err)
	}
	raw, _ := os.ReadFile(sess)
	if strings.Contains(string(raw), "grant-clear") || strings.Contains(strings.ToLower(string(raw)), "private") {
		t.Fatalf("session json leaked secret:\n%s", raw)
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"session", "local-net", "up", "--control-plane-url", srv.URL, "--session-file", sess}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("up %d %s", code, stderr.String())
	}
	if heartbeats != 1 {
		t.Fatalf("heartbeats=%d", heartbeats)
	}
	keyPath := sess + ".local-net.key"
	fi, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %o", fi.Mode().Perm())
	}
	keyRaw, _ := os.ReadFile(keyPath)
	if strings.TrimSpace(string(keyRaw)) == "" {
		t.Fatal("empty key")
	}
	sessRaw, _ := os.ReadFile(sess)
	if strings.Contains(string(sessRaw), strings.TrimSpace(string(keyRaw))) {
		t.Fatal("private key stored in session json")
	}
	if strings.TrimSpace(stdout.String()) != pubSeen {
		t.Fatalf("stdout pub %q heartbeat %q", stdout.String(), pubSeen)
	}

	stdout.Reset()
	code = run([]string{"session", "local-net", "down", "--session-file", sess}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("down %d %s", code, stderr.String())
	}
	if detaches != 1 || strings.TrimSpace(stdout.String()) != "withdrawn" {
		t.Fatalf("detaches=%d stdout=%q", detaches, stdout.String())
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("key still present: %v", err)
	}

	// Unknown allow-list flag is rejected, not an implicit opt-in.
	code = run([]string{"session", "start", "--session-file", filepath.Join(dir, "no.json"), "--local-net-allow", "192.168.0.0/16"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("allow flag exit=%d", code)
	}
}

func TestLocalNetUpAppliesMockWireGuardAndDownDeletesIt(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	t.Setenv("ASP_API_KEY", "")
	t.Setenv("ASP_LOCAL_NET_APPLY", "1")
	// The Linux recipe on any host; darwin has its own test.
	t.Setenv("ASP_LOCAL_NET_OS", "linux")

	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv.log")
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// wg logs the size of what it reads on stdin: the private key arrives
	// through a pipe, never as a path (Ubuntu's AppArmor profile denies those).
	script := "#!/bin/sh\n" +
		"n=$0; n=${n##*/}\n" +
		"printf '%s' \"$n\" >> \"$ASP_MOCK_LOG\"\n" +
		"for a in \"$@\"; do printf ' %s' \"$a\" >> \"$ASP_MOCK_LOG\"; done\n" +
		"if [ \"$n\" = wg ]; then IFS= read -r k; printf ' <stdin:%s>' \"${#k}\" >> \"$ASP_MOCK_LOG\"; fi\n" +
		"printf '\\n' >> \"$ASP_MOCK_LOG\"\n" +
		"exit 0\n"
	for _, name := range []string{"ip", "wg"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ASP_MOCK_LOG", logPath)
	t.Setenv("PATH", bin)

	nodePub := "ERERERERERERERERERERERERERERERERERERERERERE="
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes/{id}/local-net/grant", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"grant": "grant-clear-not-for-disk", "dial": "203.0.113.10:51024",
			"expires_at": "2026-10-03T18:00:00Z", "tunnel_iface": "wg-asp-ln-1",
			"transport": "wireguard", "node_public_key": nodePub, "listen_port": 51024,
			"node_tunnel_addr": "10.188.17.97/30", "client_tunnel_addr": "10.188.17.98/30",
		})
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/local-net/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: "ln-1", State: "running", LocalNet: true, LocalNetState: "up"})
	})
	mux.HandleFunc("DELETE /v1/sandboxes/{id}/local-net/attach", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: "ln-1", State: "running", LocalNet: true, LocalNetState: "withdrawn"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sess := filepath.Join(dir, "s.json")
	st := session.State{SandboxID: "ln-1", CPURL: srv.URL, LocalNet: true}
	if err := session.Save(sess, st); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	code := run([]string{"session", "local-net", "up", "--control-plane-url", srv.URL, "--session-file", sess}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("up %d %s", code, stderr.String())
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(log)
	for _, want := range []string{
		"ip link add dev wg-asp-ln-1 type wireguard",
		"ip address add 10.188.17.98/30 dev wg-asp-ln-1",
		"wg set wg-asp-ln-1 private-key /dev/stdin ",
		"peer " + nodePub,
		"allowed-ips 0.0.0.0/0,::/0",
		"endpoint 203.0.113.10:51024",
		"<stdin:44>",
		"ip link set wg-asp-ln-1 up",
		"ip route add 10.200.0.0/16 dev wg-asp-ln-1",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q\nlog:\n%s\nstderr:\n%s", want, text, stderr.String())
		}
	}
	if strings.Contains(text, "8888") || strings.Contains(text, "asp_egress") {
		t.Fatalf("client argv proxies:\n%s", text)
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "ip route") && (strings.Contains(line, " default") || strings.Contains(line, " 0.0.0.0/0") || strings.Contains(line, " ::/0")) {
			t.Fatalf("client argv routes the host default: %s", line)
		}
	}
	keyRaw, _ := os.ReadFile(sess + ".local-net.key")
	if strings.Contains(text, strings.TrimSpace(string(keyRaw))) {
		t.Fatal("private key in argv")
	}
	sessRaw, _ := os.ReadFile(sess)
	if strings.Contains(string(sessRaw), strings.TrimSpace(string(keyRaw))) || strings.Contains(string(sessRaw), "grant-clear") {
		t.Fatal("secret in session json")
	}
	cfi, err := os.Stat(sess + ".local-net.conf")
	if err != nil {
		t.Fatal(err)
	}
	if cfi.Mode().Perm() != 0o600 {
		t.Fatalf("conf mode %o", cfi.Mode().Perm())
	}

	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	code = run([]string{"session", "local-net", "down", "--session-file", sess}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("down %d %s", code, stderr.String())
	}
	text = string(mustReadFile(t, logPath))
	if !strings.Contains(text, "ip link delete dev wg-asp-ln-1") {
		t.Fatalf("down log:\n%s", text)
	}
	if strings.Contains(text, "8888") {
		t.Fatalf("down proxy:\n%s", text)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A healthy exec is one request: no GetSandbox before it.
func TestSessionExecIsOneRequest(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	var requests atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stream") == "1" {
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = w.Write([]byte(`{"type":"stdout","data":"hi\n"}` + "\n" + `{"type":"exit","exit_code":0}` + "\n"))
			return
		}
		_ = json.NewEncoder(w).Encode(client.ExecResult{Stdout: "hi\n"})
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		mux.ServeHTTP(w, r)
	}))
	defer srv.Close()
	sessFile := filepath.Join(t.TempDir(), "session.json")
	if err := session.Save(sessFile, session.State{SandboxID: "s1", CPURL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range [][]string{{"--buffered"}, {}} {
		requests.Store(0)
		var stdout, stderr strings.Builder
		args := append([]string{"session", "exec", "--session-file", sessFile}, mode...)
		code := run(append(args, "--cmd", "echo hi"), &stdout, &stderr)
		if code != 0 || !strings.Contains(stdout.String(), "hi") {
			t.Fatalf("exec %v: exit=%d stdout=%q stderr=%q", mode, code, stdout.String(), stderr.String())
		}
		if n := requests.Load(); n != 1 {
			t.Fatalf("exec %v made %d requests, want 1", mode, n)
		}
	}
}

func TestSessionExecExplainsSandboxLostWithNode(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	mux := http.NewServeMux()
	node := "node-b"
	mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: "lost", State: "failed", NodeID: &node, StopReason: client.StopReasonNodeLost})
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"sandbox was lost with its node; start a new sandbox (asp session start --force)"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	sessFile := filepath.Join(t.TempDir(), "session.json")
	if err := session.Save(sessFile, session.State{SandboxID: "lost", CPURL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range [][]string{{"--buffered"}, {}} {
		var stdout, stderr strings.Builder
		args := append([]string{"session", "exec", "--session-file", sessFile}, mode...)
		code := run(append(args, "--cmd", "true"), &stdout, &stderr)
		if code != 1 || !strings.Contains(stderr.String(), "on node node-b was lost") || !strings.Contains(stderr.String(), "--force") {
			t.Fatalf("exec %v: exit=%d stderr=%q", mode, code, stderr.String())
		}
	}
}

// lifecycleServer is a control plane with one sandbox whose state the test sets.
type lifecycleServer struct {
	mu     sync.Mutex
	state  string
	detail string
	boots  int
	calls  []string
	// next is the state a stop or a resume moves to on the next GET: the node's work.
	stopTo, resumeTo string
	refuse           int // status for POST /start; 0 = accept
	srv              *httptest.Server
}

func newLifecycleServer(t *testing.T, initial string) *lifecycleServer {
	t.Helper()
	l := &lifecycleServer{state: initial, boots: 1, stopTo: "stopped", resumeTo: "running"}
	mux := http.NewServeMux()
	view := func() client.Sandbox {
		node := "n1"
		return client.Sandbox{ID: "sb-1", State: l.state, TenantID: "acme", NodeID: &node, BootCount: l.boots, StatusDetail: l.detail}
	}
	mux.HandleFunc("GET /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.calls = append(l.calls, "LIST "+r.URL.RawQuery)
		l.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []client.Sandbox{}})
	})
	mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		// The node acts between polls.
		switch l.state {
		case "stopping":
			l.state = l.stopTo
		case "requested", "starting":
			l.state = l.resumeTo
		}
		_ = json.NewEncoder(w).Encode(view())
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.calls = append(l.calls, "STOP")
		if l.state == "running" {
			l.state = "stopping"
		}
		_ = json.NewEncoder(w).Encode(view())
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.calls = append(l.calls, "START")
		if l.refuse != 0 {
			w.WriteHeader(l.refuse)
			_, _ = w.Write([]byte(`{"error":"node n1 cannot fit 1000m/512MiB: insufficient memory"}`))
			return
		}
		l.state = "requested"
		l.boots++
		_ = json.NewEncoder(w).Encode(view())
	})
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.calls = append(l.calls, "DELETE")
		l.state = "deleting"
		_ = json.NewEncoder(w).Encode(view())
	})
	l.srv = httptest.NewServer(mux)
	t.Cleanup(l.srv.Close)
	return l
}

func (l *lifecycleServer) sawCalls() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.calls, ",")
}

func sessionFile(t *testing.T, l *lifecycleServer) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "session.json")
	if err := session.Save(f, session.State{Name: "work", SandboxID: "sb-1", CPURL: l.srv.URL, TenantID: "acme"}); err != nil {
		t.Fatal(err)
	}
	return f
}

// stop keeps the session file, waits for the node to power the sandbox off, and
// says its disk is kept; resume boots it again and waits for running.
func TestSessionStopKeepsTheSessionAndResumeBootsIt(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	l := newLifecycleServer(t, "running")
	f := sessionFile(t, l)

	var stdout, stderr strings.Builder
	if code := run([]string{"session", "stop", "--session-file", f, "--timeout", "5s"}, &stdout, &stderr); code != 0 {
		t.Fatalf("stop exit=%d stderr=%q", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "sb-1" || !strings.Contains(stderr.String(), "disk is kept") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if l.state != "stopped" {
		t.Fatalf("state=%s: stop must wait for stopped", l.state)
	}
	if _, err := session.Load(f); err != nil {
		t.Fatalf("a stop must keep the session file: %v", err)
	}
	if strings.Contains(l.sawCalls(), "DELETE") {
		t.Fatalf("a stop must not delete: %s", l.sawCalls())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"session", "resume", "--session-file", f, "--timeout", "5s"}, &stdout, &stderr); code != 0 {
		t.Fatalf("resume exit=%d stderr=%q", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "sb-1" || !strings.Contains(stderr.String(), "boot=2") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if l.state != "running" {
		t.Fatalf("state=%s after resume", l.state)
	}
	if got := l.sawCalls(); got != "STOP,START" {
		t.Fatalf("calls=%s", got)
	}
}

// --no-wait returns as soon as the stop is requested.
func TestSessionStopNoWait(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	l := newLifecycleServer(t, "running")
	f := sessionFile(t, l)
	var stdout, stderr strings.Builder
	if code := run([]string{"session", "stop", "--no-wait", "--session-file", f}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if l.state != "stopping" {
		t.Fatalf("state=%s: --no-wait must not poll", l.state)
	}
}

// A resume that no node can take says so, and the session and the sandbox stay.
func TestSessionResumeRefusals(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	for status, want := range map[int]string{
		http.StatusServiceUnavailable: "no capacity on the node that holds the disk",
		http.StatusConflict:           "session file kept",
		http.StatusNotFound:           "asp session rm clears",
	} {
		l := newLifecycleServer(t, "stopped")
		l.refuse = status
		f := sessionFile(t, l)
		var stdout, stderr strings.Builder
		code := run([]string{"session", "resume", "--session-file", f}, &stdout, &stderr)
		if code != 1 || !strings.Contains(stderr.String(), want) {
			t.Fatalf("status %d: exit=%d stderr=%q, want %q", status, code, stderr.String(), want)
		}
		if _, err := session.Load(f); err != nil {
			t.Fatalf("status %d: the session file must stay: %v", status, err)
		}
	}
}

// A resume whose start fails ends stopped again: the CLI says why, and that the
// disk is kept.
func TestSessionResumeThatGoesBackToStopped(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	l := newLifecycleServer(t, "stopped")
	l.resumeTo = "stopped"
	l.detail = "resume failed: vm.boot failed"
	f := sessionFile(t, l)
	var stdout, stderr strings.Builder
	code := run([]string{"session", "resume", "--session-file", f, "--timeout", "5s"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "resume failed: vm.boot failed") || !strings.Contains(stderr.String(), "disk is kept") {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
}

// rm deletes the sandbox and its disk and clears the file; --local only clears the file.
func TestSessionRemoveDeletesAndClears(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	l := newLifecycleServer(t, "stopped")
	f := sessionFile(t, l)
	var stdout, stderr strings.Builder
	if code := run([]string{"session", "rm", "--local", "--session-file", f}, &stdout, &stderr); code != 0 {
		t.Fatalf("rm --local exit=%d stderr=%q", code, stderr.String())
	}
	if l.sawCalls() != "" {
		t.Fatalf("--local called the control plane: %s", l.sawCalls())
	}
	f = sessionFile(t, l)
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"session", "rm", "--session-file", f}, &stdout, &stderr); code != 0 {
		t.Fatalf("rm exit=%d stderr=%q", code, stderr.String())
	}
	if l.sawCalls() != "DELETE" || !strings.Contains(stderr.String(), "deleted sandbox sb-1 state=deleting") {
		t.Fatalf("calls=%s stderr=%q", l.sawCalls(), stderr.String())
	}
	if _, err := session.Load(f); !errors.Is(err, session.ErrNoSession) {
		t.Fatalf("after rm: %v", err)
	}
}

// stop on a sandbox that cannot be stopped explains, and keeps the file.
func TestSessionStopOfAFailedOrGoneSandbox(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	for status, want := range map[int]string{
		http.StatusConflict: "asp session rm clears",
		http.StatusNotFound: "is gone",
	} {
		mux := http.NewServeMux()
		mux.HandleFunc("POST /v1/sandboxes/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"conflict: cannot stop from state failed"}`))
		})
		srv := httptest.NewServer(mux)
		f := filepath.Join(t.TempDir(), "session.json")
		if err := session.Save(f, session.State{SandboxID: "s1", CPURL: srv.URL}); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr strings.Builder
		code := run([]string{"session", "stop", "--session-file", f}, &stdout, &stderr)
		srv.Close()
		if code != 1 || !strings.Contains(stderr.String(), want) {
			t.Fatalf("status %d: exit=%d stderr=%q, want %q", status, code, stderr.String(), want)
		}
		if _, err := session.Load(f); err != nil {
			t.Fatalf("status %d: the file must stay: %v", status, err)
		}
	}
}

// exec and status on a stopped session say to resume it, not to start a new one.
func TestSessionExecAndStatusOfAStoppedSandbox(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	l := newLifecycleServer(t, "stopped")
	l.srv.Config.Handler.(*http.ServeMux).HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"sandbox is stopped; its disk is kept: resume it (asp session resume)"}`))
	})
	f := sessionFile(t, l)

	var stdout, stderr strings.Builder
	code := run([]string{"session", "exec", "--buffered", "--session-file", f, "--cmd", "true"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "its disk is kept on node n1") || !strings.Contains(stderr.String(), "asp session resume") {
		t.Fatalf("exec exit=%d stderr=%q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"session", "status", "--session-file", f}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "state=stopped") || !strings.Contains(stderr.String(), "asp session resume") {
		t.Fatalf("status exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

// The idle-reaped text points at resume, not at a new sandbox.
func TestIdleReapedTextSaysResume(t *testing.T) {
	if got := idleReapedText("sb-1", "/f"); !strings.Contains(got, "asp session resume") || strings.Contains(got, "--force") {
		t.Fatalf("text=%q", got)
	}
}

// sandbox stop/start/list --all hit the matching routes.
func TestSandboxStopStartAndListAll(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	l := newLifecycleServer(t, "running")
	var stdout, stderr strings.Builder
	for _, args := range [][]string{
		{"sandbox", "stop", "sb-1", "--control-plane-url", l.srv.URL},
		{"sandbox", "start", "sb-1", "--control-plane-url", l.srv.URL},
		{"sandbox", "list", "--all", "--control-plane-url", l.srv.URL},
		{"sandbox", "delete", "sb-1", "--control-plane-url", l.srv.URL},
	} {
		stdout.Reset()
		stderr.Reset()
		l.mu.Lock()
		if args[1] == "start" {
			l.state = "stopped"
		}
		l.mu.Unlock()
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v exit=%d stderr=%q", args, code, stderr.String())
		}
	}
	if got := l.sawCalls(); got != "STOP,START,LIST include_deleted=1,DELETE" {
		t.Fatalf("calls=%s", got)
	}
}

// The status detail of a failed resume already starts with "resume failed:": the
// text around it must not say it twice.
func TestStoppedTextDoesNotRepeatResumeFailed(t *testing.T) {
	node := "n1"
	sb := client.Sandbox{ID: "sb-1", State: "stopped", NodeID: &node, StatusDetail: "resume failed: the guest did not answer within 1m0s"}
	got := stoppedText(sb, "/f")
	if strings.Contains(got, "resume failed: resume failed") || !strings.Contains(got, "the last resume failed: the guest did not answer within 1m0s") {
		t.Fatalf("text=%q", got)
	}
	sb.StatusDetail = "vm.boot failed"
	if got := stoppedText(sb, "/f"); !strings.Contains(got, "the last resume failed: vm.boot failed") {
		t.Fatalf("text=%q", got)
	}
}

// When a resume ends failed (the retained disk was gone) the CLI must not say
// the disk is kept or suggest trying again: there is nothing to try.
func TestSessionResumeThatEndsFailedSaysItCannotBeResumed(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	l := newLifecycleServer(t, "stopped")
	l.resumeTo = "failed"
	l.detail = "disk_lost: the retained disk of this sandbox is missing on this node"
	f := sessionFile(t, l)
	var stdout, stderr strings.Builder
	code := run([]string{"session", "resume", "--session-file", f, "--timeout", "5s"}, &stdout, &stderr)
	out := stderr.String()
	if code != 1 || !strings.Contains(out, "disk_lost") || !strings.Contains(out, "cannot be resumed") || !strings.Contains(out, "asp session rm") {
		t.Fatalf("exit=%d stderr=%q", code, out)
	}
	if strings.Contains(out, "disk is kept") || strings.Contains(out, "try again") {
		t.Fatalf("a failed resume has no disk to keep: %q", out)
	}
}
