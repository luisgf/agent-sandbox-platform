package hostvsock

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/identity"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/sshagent"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/sshagent/agenttest"
)

func TestServiceSSHAgentFake(t *testing.T) {
	dir := t.TempDir()
	factory := UnixFactory{Dir: dir}
	svc := &Service{
		Factory:     factory,
		SSHHostSock: "",
		Logger:      nil,
	}
	t.Setenv("SSH_AUTH_SOCK", "")
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	path := factory.PathFor(PortSSHAgent)
	deadline := time.Now().Add(2 * time.Second)
	var client net.Conn
	var err error
	for time.Now().Before(deadline) {
		client, err = net.Dial("unix", path)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if client == nil {
		t.Fatal("dial ssh:", err)
	}
	defer client.Close()

	count, err := sshagent.RequestIdentities(client)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("count=%d", count)
	}
}

// The global listener cannot tell guests apart: a sandbox's approval does not
// unlock a sign there, and a global one needs GlobalApprovals.
func TestServiceGlobalSSHListenerNeedsGlobalApprovals(t *testing.T) {
	for _, global := range []bool{false, true} {
		dir := t.TempDir()
		up := agenttest.Start(t, filepath.Join(dir, "operator-agent.sock"))
		ap := sshagent.NewApprover(time.Minute)
		ap.GlobalApprovals = global
		factory := UnixFactory{Dir: dir}
		svc := &Service{Factory: factory, SSHHostSock: up.Path, SSHConfirm: ap}
		if err := svc.Start(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := ap.Approve("sb-a", time.Minute); err != nil {
			t.Fatal(err)
		}
		want := byte(agenttest.Failure)
		if global {
			if _, _, err := ap.Approve("", time.Minute); err != nil {
				t.Fatal(err)
			}
			want = agenttest.SignResponse
		}
		c := dialUnixEventually(t, factory.PathFor(PortSSHAgent), 2*time.Second)
		got, err := agenttest.Request(c, agenttest.SignRequestMsg())
		c.Close()
		svc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("GlobalApprovals=%v: sign on the global listener got %d, want %d", global, got, want)
		}
		if ap.PendingCount("sb-a") != 1 {
			t.Fatalf("GlobalApprovals=%v: the global listener used sb-a's approval", global)
		}
	}
}

func TestServiceIdentityHTTP(t *testing.T) {
	dir := t.TempDir()
	factory := UnixFactory{Dir: dir}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("POST /v1/tokens/oidc", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = body
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "tok",
			"token_type":   "Bearer",
			"expires_in":   60,
		})
	})
	svc := &Service{
		Factory:         factory,
		IdentityHandler: mux,
	}
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	path := factory.PathFor(PortIdentity)
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
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

	req, _ := http.NewRequest(http.MethodPost, "http://unix/v1/tokens/oidc", nil)
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	// empty body → identity proxy would 400; our stub accepts anyway
	if resp2.StatusCode != 200 {
		t.Fatalf("token status=%d", resp2.StatusCode)
	}
}

// The global listeners cannot tell guests apart: identity there refuses tokens
// unless the lab flag trusts X-ASP-Sandbox-ID.
func TestServiceGlobalIdentityIsUnbound(t *testing.T) {
	for _, trust := range []bool{false, true} {
		cp, minted := mintRecorder(t)
		factory := UnixFactory{Dir: t.TempDir()}
		svc := &Service{
			Factory: factory,
			IdentityHandler: (&identity.Proxy{
				ControlPlaneURL:    cp.URL,
				HTTP:               cp.Client(),
				TrustSandboxHeader: trust,
			}).Handler(),
		}
		if err := svc.Start(); err != nil {
			t.Fatal(err)
		}
		status, _ := requestToken(t, factory.PathFor(PortIdentity), "sb-b")
		_ = svc.Close()
		want, wantMinted := http.StatusForbidden, "[]"
		if trust {
			want, wantMinted = http.StatusOK, "[sb-b]"
		}
		if status != want || fmt.Sprint(minted()) != wantMinted {
			t.Errorf("trust header %v: status %d, minted %v; want %d, %s", trust, status, minted(), want, wantMinted)
		}
	}
}

func TestUnixFactoryPath(t *testing.T) {
	f := UnixFactory{Dir: "/tmp/asp"}
	if got := f.PathFor(26501); got != filepath.Join("/tmp/asp", "host-vsock-26501.sock") {
		t.Fatalf("got %s", got)
	}
}
