package sshagent

import (
	"net"
	"path/filepath"
	"testing"
	"time"
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

func TestBridgeBytePumpEcho(t *testing.T) {
	dir := t.TempDir()
	upstreamPath := filepath.Join(dir, "upstream.sock")
	listenPath := filepath.Join(dir, "bridge.sock")

	uln, err := net.Listen("unix", upstreamPath)
	if err != nil {
		t.Fatal(err)
	}
	defer uln.Close()
	go func() {
		conn, err := uln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		ServeFakeAgentConn(conn)
	}()

	b := &Bridge{ListenPath: listenPath, HostSock: upstreamPath}
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	deadline := time.Now().Add(2 * time.Second)
	var client net.Conn
	for time.Now().Before(deadline) {
		client, err = net.Dial("unix", listenPath)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if client == nil {
		t.Fatal("dial bridge:", err)
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
