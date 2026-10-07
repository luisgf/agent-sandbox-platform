package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// newTokenAgent is a node agent whose local API wants a bearer token, as the
// real one does. seen collects the Authorization headers of exec calls.
func newTokenAgent(t *testing.T, token string, seen *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"node agent token required"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/internal/exec":
			_, _ = w.Write([]byte(`{"stdout":"hello\n","stderr":"","exit_code":0}`))
		case "/v1/internal/exec/stdin":
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runningSandboxOn(t *testing.T, agent *httptest.Server) (http.Handler, string) {
	t.Helper()
	t.Setenv("ASP_AUTO_PROVISION", "0")
	mem := newBackend(t)
	if _, err := mem.RegisterNode(context.Background(), store.RegisterNodeInput{ID: "n1", Endpoint: agent.URL, AgentEndpoint: agent.URL}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(mem)
	srv.Client = agent.Client()
	sb, err := mem.CreateSandbox(context.Background(), store.CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64, NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	runSandbox(t, mem, sb.ID)
	return testMux(srv), sb.ID
}

func exec(t *testing.T, h http.Handler, id, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+id+path, bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// The control plane sends the secret of the agent's token file with every
// exec and stdin call to a same-host agent.
func TestExecSendsTheAgentToken(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenFile, []byte("s3cret-agent-token-0123456789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvAgentTokenFile, tokenFile)
	var seen []string
	agent := newTokenAgent(t, "s3cret-agent-token-0123456789", &seen)
	h, id := runningSandboxOn(t, agent)

	if rr := exec(t, h, id, "/exec", `{"cmd":["echo","hello"]}`); rr.Code != http.StatusOK {
		t.Fatalf("exec: %d %s", rr.Code, rr.Body.String())
	}
	if rr := exec(t, h, id, "/exec/stdin", `{"exec_id":"e1","data":"aGk="}`); rr.Code != http.StatusOK {
		t.Fatalf("stdin: %d %s", rr.Code, rr.Body.String())
	}
	if len(seen) != 2 || seen[0] != "Bearer s3cret-agent-token-0123456789" || seen[1] != seen[0] {
		t.Fatalf("Authorization headers = %q", seen)
	}
}

// Without the secret the agent answers 401, and the caller is told what to fix
// instead of getting a bare gateway error.
func TestExecExplainsAnAgentThatRefusesTheControlPlane(t *testing.T) {
	t.Setenv(EnvAgentTokenFile, filepath.Join(t.TempDir(), "missing.token"))
	var seen []string
	agent := newTokenAgent(t, "s3cret-agent-token-0123456789", &seen)
	h, id := runningSandboxOn(t, agent)

	rr := exec(t, h, id, "/exec", `{"cmd":["id"]}`)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rr.Code, rr.Body.String())
	}
	var e errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &e)
	for _, want := range []string{"401", EnvAgentTokenFile, "--agent-token-file"} {
		if !strings.Contains(e.Error, want) {
			t.Errorf("the error does not mention %q: %s", want, e.Error)
		}
	}
	if len(seen) != 1 || seen[0] != "" {
		t.Fatalf("with no token file a header was sent: %q", seen)
	}
}

// A secret that changes on disk is picked up without restarting the control plane.
func TestAgentTokenSourceFollowsTheFile(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	t.Setenv(EnvAgentTokenFile, tokenFile)
	var src agentTokenSource
	if got := src.Token(); got != "" {
		t.Fatalf("no file: %q", got)
	}
	if err := os.WriteFile(tokenFile, []byte("first-secret-value-1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src.checked = time.Time{} // do not wait for the one-second cache
	if got := src.Token(); got != "first-secret-value-1234" {
		t.Fatalf("first read: %q", got)
	}
	if err := os.WriteFile(tokenFile, []byte("second-secret-value-12345678\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src.checked = time.Time{}
	if got := src.Token(); got != "second-secret-value-12345678" {
		t.Fatalf("after the file changed: %q", got)
	}
	if err := os.Remove(tokenFile); err != nil {
		t.Fatal(err)
	}
	src.checked = time.Time{}
	if got := src.Token(); got != "" {
		t.Fatalf("after the file went away: %q", got)
	}
}

// An https:// agent is reached over mutual TLS; it gets no bearer token, so a
// compromised agent endpoint on another host never receives the local secret.
func TestAgentTokenIsNotSentOverHTTPS(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenFile, []byte("s3cret-agent-token-0123456789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvAgentTokenFile, tokenFile)
	s := NewServer(newBackend(t))
	for url, want := range map[string]bool{"http://127.0.0.1:9100/x": true, "https://node.example:9443/x": false} {
		req := httptest.NewRequest(http.MethodPost, url, nil)
		s.authorizeAgentRequest(req)
		if got := req.Header.Get("Authorization") != ""; got != want {
			t.Errorf("%s: token sent = %v, want %v", url, got, want)
		}
	}
}
