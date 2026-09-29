package main

import (
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// FakeAgent answers REQUEST_IDENTITIES with empty list (same wire as sshagent.FakeAgent).
func serveFakeAgent(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 4)
	for {
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(buf)
		if n == 0 || n > 1<<20 {
			return
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(conn, payload); err != nil {
			return
		}
		var resp []byte
		if payload[0] == 11 { // REQUEST_IDENTITIES
			resp = []byte{12, 0, 0, 0, 0}
		} else {
			resp = []byte{5}
		}
		out := make([]byte, 4+len(resp))
		binary.BigEndian.PutUint32(out, uint32(len(resp)))
		copy(out[4:], resp)
		if _, err := conn.Write(out); err != nil {
			return
		}
	}
}

func TestUnixUpstreamContract(t *testing.T) {
	dir := t.TempDir()
	upstreamPath := filepath.Join(dir, "upstream.sock")
	listenPath := filepath.Join(dir, "listen.sock")

	upLn, err := net.Listen("unix", upstreamPath)
	if err != nil {
		t.Fatal(err)
	}
	defer upLn.Close()
	go func() {
		for {
			c, err := upLn.Accept()
			if err != nil {
				return
			}
			go serveFakeAgent(c)
		}
	}()

	// Run proxy in-process via dialUpstream+listen loop would be heavy; test pump path:
	clientA, clientB := net.Pipe()
	defer clientA.Close()
	defer clientB.Close()

	up, err := dialUpstream("unix:"+upstreamPath, 2, 26501)
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	go pump(clientB, up)

	// REQUEST_IDENTITIES
	req := []byte{0, 0, 0, 1, 11}
	if _, err := clientA.Write(req); err != nil {
		t.Fatal(err)
	}
	_ = clientA.SetReadDeadline(time.Now().Add(2 * time.Second))
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(clientA, hdr); err != nil {
		t.Fatal(err)
	}
	n := binary.BigEndian.Uint32(hdr)
	body := make([]byte, n)
	if _, err := io.ReadFull(clientA, body); err != nil {
		t.Fatal(err)
	}
	if body[0] != 12 {
		t.Fatalf("want identities answer, got type %d", body[0])
	}

	// Also ensure listen path dir works for integration-ish smoke of flag defaults.
	_ = os.Remove(listenPath)
	ln, err := net.Listen("unix", listenPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	_ = os.Remove(listenPath)
}

func TestDescribeUpstream(t *testing.T) {
	if got := describeUpstream("", 2, 26501); got != "vsock://2:26501" {
		t.Fatalf("got %s", got)
	}
	if got := describeUpstream("unix:/tmp/x.sock", 2, 26501); got != "unix:/tmp/x.sock" {
		t.Fatalf("got %s", got)
	}
}
