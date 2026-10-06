package hostvsock

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/identity"
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

// A guest reaches the identity proxy through its own sandbox's {muxer}_26502,
// so it gets that sandbox's token whatever X-ASP-Sandbox-ID says.
func TestHybridIdentityBindsSandbox(t *testing.T) {
	// The lab flag only concerns listeners without a binding.
	for name, trust := range map[string]bool{"strict": false, "lab": true} {
		t.Run(name, func(t *testing.T) {
			cp, minted := mintRecorder(t)
			svc := &Service{
				SkipGlobalListeners: true,
				IdentityHandler: (&identity.Proxy{
					ControlPlaneURL:    cp.URL,
					HTTP:               cp.Client(),
					TrustSandboxHeader: trust,
				}).Handler(),
			}
			if err := svc.Start(); err != nil {
				t.Fatal(err)
			}
			defer svc.Close()
			dir := t.TempDir()
			socks := map[string]string{}
			for _, id := range []string{"sb-a", "sb-b"} {
				muxer := filepath.Join(dir, "vsock-"+id+".sock")
				if err := svc.AttachSandbox(id, muxer); err != nil {
					t.Fatal(err)
				}
				socks[id] = HybridGuestPath(muxer, PortIdentity)
			}

			for _, tc := range []struct {
				guest, header string
				want          int
			}{
				{"sb-a", "sb-b", http.StatusForbidden},
				{"sb-b", "sb-a", http.StatusForbidden},
				{"sb-a", "sb-a", http.StatusOK},
				{"sb-a", "", http.StatusOK},
				{"sb-b", "", http.StatusOK},
			} {
				before := len(minted())
				status, token := requestToken(t, socks[tc.guest], tc.header)
				got := minted()[before:]
				switch {
				case status != tc.want:
					t.Errorf("guest of %s claiming %q: status %d, want %d", tc.guest, tc.header, status, tc.want)
				case status != http.StatusOK && len(got) > 0:
					t.Errorf("guest of %s claiming %q: refused, yet the control plane minted %v", tc.guest, tc.header, got)
				case status == http.StatusOK && (fmt.Sprint(got) != "["+tc.guest+"]" || token != "tok-"+tc.guest):
					t.Errorf("guest of %s claiming %q: minted %v, token %q; want %s's", tc.guest, tc.header, got, token, tc.guest)
				}
			}
		})
	}
}

// The identity binding relies on one sandbox per muxer path.
func TestHybridAttachRefusesSharedMuxer(t *testing.T) {
	cp, minted := mintRecorder(t)
	svc := &Service{
		SkipGlobalListeners: true,
		IdentityHandler:     (&identity.Proxy{ControlPlaneURL: cp.URL, HTTP: cp.Client()}).Handler(),
	}
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	muxer := filepath.Join(t.TempDir(), "vsock-a.sock")
	if err := svc.AttachSandbox("sb-a", muxer); err != nil {
		t.Fatal(err)
	}
	if err := svc.AttachSandbox("sb-b", muxer); err == nil {
		t.Fatal("attached sb-b to sb-a's muxer")
	}
	// sb-a's VMM still reaches a listener bound to sb-a.
	if status, _ := requestToken(t, HybridGuestPath(muxer, PortIdentity), ""); status != http.StatusOK || fmt.Sprint(minted()) != "[sb-a]" {
		t.Fatalf("status %d, minted %v; want sb-a's token", status, minted())
	}
	svc.DetachSandbox("sb-a")
	if err := svc.AttachSandbox("sb-b", muxer); err != nil {
		t.Fatalf("muxer still held after detach: %v", err)
	}
}

// The guest's identity socket refuses large headers rather than buffering them
// (net/http's default allows 1 MiB).
func TestHybridIdentityRefusesLargeHeaders(t *testing.T) {
	cp, minted := mintRecorder(t)
	svc := &Service{
		SkipGlobalListeners: true,
		IdentityHandler:     (&identity.Proxy{ControlPlaneURL: cp.URL, HTTP: cp.Client()}).Handler(),
	}
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	muxer := filepath.Join(t.TempDir(), "vsock-a.sock")
	if err := svc.AttachSandbox("sb-a", muxer); err != nil {
		t.Fatal(err)
	}
	conn := dialUnixEventually(t, HybridGuestPath(muxer, PortIdentity), 2*time.Second)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// Written in the background: the server answers before reading it all.
	body := `{"aud":"https://api.example.com"}`
	go fmt.Fprintf(conn, "POST /v1/tokens/oidc HTTP/1.1\r\nHost: guest\r\nX-Pad: %s\r\nContent-Length: %d\r\n\r\n%s",
		strings.Repeat("a", 64<<10), len(body), body)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge || len(minted()) != 0 {
		t.Fatalf("status %d, minted %v; want 431 and no token", resp.StatusCode, minted())
	}
}

// mintRecorder is a control plane that mints "tok-{sandbox}" and records
// which sandbox each token was for.
func mintRecorder(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SandboxID string `json:"sandbox_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen = append(seen, body.SandboxID)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "tok-" + body.SandboxID,
			"token_type":   "Bearer",
			"expires_in":   60,
		})
	}))
	t.Cleanup(cp.Close)
	return cp, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// requestToken asks for a token over a new connection to a guest→host
// identity socket, naming sandboxHeader unless empty.
func requestToken(t *testing.T, path, sandboxHeader string) (int, string) {
	t.Helper()
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", path)
			},
		},
		Timeout: 2 * time.Second,
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequest(http.MethodPost, "http://guest/v1/tokens/oidc",
		strings.NewReader(`{"aud":"https://api.example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	if sandboxHeader != "" {
		req.Header.Set(identity.SandboxHeader, sandboxHeader)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.AccessToken
}

// Re-attaching replaces the previous listeners. Closing them must not unlink
// the sockets the new ones bound on the same paths.
func TestHybridReattach(t *testing.T) {
	cp, _ := mintRecorder(t)
	svc := &Service{
		SkipGlobalListeners: true,
		IdentityHandler:     (&identity.Proxy{ControlPlaneURL: cp.URL, HTTP: cp.Client()}).Handler(),
	}
	t.Setenv("SSH_AUTH_SOCK", "")
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	dir := t.TempDir()
	muxer := filepath.Join(dir, "vsock-r.sock")
	moved := filepath.Join(dir, "vsock-r2.sock")
	for _, path := range []string{muxer, muxer, moved} {
		if err := svc.AttachSandbox("sb-r", path); err != nil {
			t.Fatal(err)
		}
		ssh, err := net.Dial("unix", HybridGuestPath(path, PortSSHAgent))
		if err != nil {
			t.Fatalf("ssh-agent socket after attach to %s: %v", path, err)
		}
		count, err := sshagent.RequestIdentities(ssh)
		ssh.Close()
		if err != nil || count != 0 {
			t.Fatalf("ssh-agent after attach to %s: count %d, err %v", path, count, err)
		}
		if status, token := requestToken(t, HybridGuestPath(path, PortIdentity), ""); status != http.StatusOK || token != "tok-sb-r" {
			t.Fatalf("identity after attach to %s: status %d, token %q; want sb-r's", path, status, token)
		}
	}
	// The move released the old paths.
	for _, port := range []uint32{PortSSHAgent, PortIdentity} {
		if _, err := os.Stat(HybridGuestPath(muxer, port)); !os.IsNotExist(err) {
			t.Errorf("%s left behind after the move: %v", HybridGuestPath(muxer, port), err)
		}
	}
	if err := svc.AttachSandbox("sb-other", muxer); err != nil {
		t.Fatalf("old muxer still held after the move: %v", err)
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

func TestHybridAttachUsesRegistrySock(t *testing.T) {
	dir := t.TempDir()
	up := filepath.Join(dir, "owner-alice.sock")
	uln, err := net.Listen("unix", up)
	if err != nil {
		t.Fatal(err)
	}
	defer uln.Close()
	go func() {
		for {
			c, err := uln.Accept()
			if err != nil {
				return
			}
			go sshagent.ServeFakeAgentConn(c)
		}
	}()

	reg := sshagent.NewRegistry(filepath.Join(dir, "owner-{owner_sub}.sock"), "/should-not-use")
	reg.Bind("sb-alice", "alice")

	muxer := filepath.Join(dir, "vsock-alice.sock")
	svc := &Service{
		SkipGlobalListeners: true,
		SSHHostSock:         "/legacy-global.sock",
		SSHRegistry:         reg,
	}
	t.Setenv("SSH_AUTH_SOCK", "/env-should-not-leak.sock")
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if err := svc.AttachSandbox("sb-alice", muxer); err != nil {
		t.Fatal(err)
	}
	defer svc.DetachSandbox("sb-alice")

	got, ok := svc.HostSockFor("sb-alice")
	if !ok || got != up {
		t.Fatalf("HostSockFor=%q ok=%v want %q", got, ok, up)
	}

	path := HybridGuestPath(muxer, PortSSHAgent)
	client := dialUnixEventually(t, path, 2*time.Second)
	defer client.Close()
	count, err := sshagent.RequestIdentities(client)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("count=%d", count)
	}
}

func TestHybridAttachTemplateMissingIsFakeAgent(t *testing.T) {
	dir := t.TempDir()
	reg := sshagent.NewRegistry(filepath.Join(dir, "missing-{owner_sub}.sock"), "/should-not-use")
	reg.Bind("sb-x", "nobody")

	muxer := filepath.Join(dir, "vsock-x.sock")
	svc := &Service{SkipGlobalListeners: true, SSHRegistry: reg}
	t.Setenv("SSH_AUTH_SOCK", "") // even if set, scoped must not use env for missing path
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if err := svc.AttachSandbox("sb-x", muxer); err != nil {
		t.Fatal(err)
	}
	defer svc.DetachSandbox("sb-x")

	path := HybridGuestPath(muxer, PortSSHAgent)
	client := dialUnixEventually(t, path, 2*time.Second)
	defer client.Close()
	count, err := sshagent.RequestIdentities(client)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("FakeAgent count=%d", count)
	}
}
