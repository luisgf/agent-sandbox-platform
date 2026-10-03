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
	code = run([]string{"session", "exec", "--buffered", "--session-file", sessFile, "--cmd", "true"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "idle timeout") {
		t.Fatalf("exec exit=%d stderr=%q", code, stderr.String())
	}
	if execs.Load() != 0 {
		t.Fatalf("exec should not be proxied when already reaped, execs=%d", execs.Load())
	}
}

func TestNamedSessionsAreIndependent(t *testing.T) {
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
		"--cp-url", srv.URL, "--timeout", "2s", "--workspace", ws,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("alpha start exit=%d stderr=%q", code, stderr.String())
	}
	alphaID := strings.TrimSpace(stdout.String())
	stdout.Reset()
	stderr.Reset()
	code = run([]string{
		"session", "start", "--name", "beta", "--session-dir", dir,
		"--cp-url", srv.URL, "--timeout", "2s",
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
	code := run([]string{"session", "start", "--cp-url", srv.URL, "--session-file", sess, "--tenant", "acme", "--image", "img", "--local-net", "--timeout", "2s"}, &stdout, &stderr)
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
	code = run([]string{"session", "local-net", "up", "--cp-url", srv.URL, "--session-file", sess}, &stdout, &stderr)
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
