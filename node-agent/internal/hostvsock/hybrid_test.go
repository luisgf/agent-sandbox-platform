package hostvsock

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/sshagent"
)

func TestHybridGuestPath(t *testing.T) {
	got := HybridGuestPath("/run/asp/vsock-sb1.sock", 26501)
	want := "/run/asp/vsock-sb1.sock_26501"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestHybridAttachSSHAgentFake(t *testing.T) {
	dir := t.TempDir()
	muxer := filepath.Join(dir, "vsock-sb-test.sock")
	// Muxer itself need not exist; CH creates it. Hybrid paths are separate.
	svc := &Service{
		SkipGlobalListeners: true,
		SSHHostSock:         "",
	}
	t.Setenv("SSH_AUTH_SOCK", "")
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	if err := svc.AttachSandbox("sb-test", muxer); err != nil {
		t.Fatal(err)
	}
	defer svc.DetachSandbox("sb-test")

	path := HybridGuestPath(muxer, PortSSHAgent)
	client := dialUnixEventually(t, path, 2*time.Second)
	defer client.Close()

	count, err := sshagent.RequestIdentities(client)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("FakeAgent identities count=%d want 0", count)
	}
}

func TestHybridAttachIdentityHTTP(t *testing.T) {
	dir := t.TempDir()
	muxer := filepath.Join(dir, "vsock-id.sock")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("POST /v1/tokens/oidc", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "tok",
			"token_type":   "Bearer",
			"expires_in":   60,
		})
	})
	svc := &Service{
		SkipGlobalListeners: true,
		IdentityHandler:     mux,
	}
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if err := svc.AttachSandbox("sb-id", muxer); err != nil {
		t.Fatal(err)
	}
	defer svc.DetachSandbox("sb-id")

	path := HybridGuestPath(muxer, PortIdentity)
	client := &http.Client{
		Transport: &http.Transport{
			Dial: func(_, _ string) (net.Conn, error) {
				return net.Dial("unix", path)
			},
		},
		Timeout: 2 * time.Second,
	}
	resp, err := client.Get("http://unix/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestHybridDetachRemovesSockets(t *testing.T) {
	dir := t.TempDir()
	muxer := filepath.Join(dir, "vsock-detach.sock")
	svc := &Service{SkipGlobalListeners: true}
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if err := svc.AttachSandbox("sb-d", muxer); err != nil {
		t.Fatal(err)
	}
	sshPath := HybridGuestPath(muxer, PortSSHAgent)
	idPath := HybridGuestPath(muxer, PortIdentity)
	svc.DetachSandbox("sb-d")
	// After detach, dial should fail (socket removed / not listening).
	if _, err := net.Dial("unix", sshPath); err == nil {
		t.Fatal("expected dial fail after detach ssh")
	}
	if _, err := net.Dial("unix", idPath); err == nil {
		t.Fatal("expected dial fail after detach identity")
	}
}

func dialUnixEventually(t *testing.T, path string, timeout time.Duration) net.Conn {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		c, err := net.Dial("unix", path)
		if err == nil {
			return c
		}
		last = err
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("dial %s: %v", path, last)
	return nil
}
