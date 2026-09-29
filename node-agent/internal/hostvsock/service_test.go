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

func TestUnixFactoryPath(t *testing.T) {
	f := UnixFactory{Dir: "/tmp/asp"}
	if got := f.PathFor(26501); got != filepath.Join("/tmp/asp", "host-vsock-26501.sock") {
		t.Fatalf("got %s", got)
	}
}
