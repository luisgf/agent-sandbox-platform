package sshagent

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/sshagent/agenttest"
)

func TestFakeAgentIdentities(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	go ServeFakeAgentConn(c2)
	count, err := RequestIdentities(c1)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("count=%d", count)
	}
}

// Only identity listing, signing and the "query" extension reach the host
// agent; requests that would change or lock it get FAILURE locally.
func TestBridgeForwardsOnlyIdentitiesAndSign(t *testing.T) {
	dir := t.TempDir()
	up := agenttest.Start(t, filepath.Join(dir, "upstream.sock"))
	listenPath := filepath.Join(dir, "bridge.sock")
	b := &Bridge{ListenPath: listenPath, HostSock: up.Path}
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	client := dialEventually(t, listenPath)
	defer client.Close()

	for _, tc := range []struct {
		name string
		msg  []byte
		want byte
	}{
		{"request_identities", []byte{agenttest.RequestIdentities}, agenttest.IdentitiesAnswer},
		{"sign_request", agenttest.SignRequestMsg(), agenttest.SignResponse},
		{"add_identity", []byte{agenttest.AddIdentity, 0, 0, 0, 7, 's', 's', 'h', '-', 'r', 's', 'a'}, agenttest.Failure},
		{"add_id_constrained", []byte{agenttest.AddIDConstrained, 0, 0, 0, 0}, agenttest.Failure},
		{"remove_identity", []byte{agenttest.RemoveIdentity, 0, 0, 0, 0}, agenttest.Failure},
		{"remove_all_identities", []byte{agenttest.RemoveAllIdentities}, agenttest.Failure},
		{"lock", []byte{agenttest.Lock, 0, 0, 0, 2, 'p', 'w'}, agenttest.Failure},
		{"unlock", []byte{agenttest.Unlock, 0, 0, 0, 2, 'p', 'w'}, agenttest.Failure},
		{"extension session-bind", agenttest.ExtensionMsg("session-bind@openssh.com"), agenttest.Failure},
		{"malformed extension", []byte{agenttest.Extension, 0, 0, 1, 0}, agenttest.Failure},
		{"extension query", agenttest.ExtensionMsg("query"), agenttest.Success},
		{"unknown type", []byte{200}, agenttest.Failure},
	} {
		got, err := agenttest.Request(client, tc.msg)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: reply type %d, want %d", tc.name, got, tc.want)
		}
	}
	want := []byte{agenttest.RequestIdentities, agenttest.SignRequest, agenttest.Extension}
	if seen := up.Seen(); string(seen) != string(want) {
		t.Fatalf("upstream received types %v, want %v", seen, want)
	}
}

func dialEventually(t *testing.T, path string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, err := net.Dial("unix", path)
		if err == nil {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatal("dial bridge:", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBridgeWithoutHostSockUsesFake(t *testing.T) {
	dir := t.TempDir()
	listenPath := filepath.Join(dir, "bridge.sock")
	b := &Bridge{ListenPath: listenPath, HostSock: ""}
	t.Setenv("SSH_AUTH_SOCK", "")
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	client, err := net.Dial("unix", listenPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	count, err := RequestIdentities(client)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("count=%d", count)
	}
}
