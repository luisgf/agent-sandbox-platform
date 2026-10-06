package sshagent

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExpandSockTemplate(t *testing.T) {
	got := ExpandSockTemplate("/run/asp/ssh-agents/{owner_sub}.sock", "user:alice", "sb1")
	want := "/run/asp/ssh-agents/user:alice.sock"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	got = ExpandSockTemplate("/run/asp/{owner_sub}/{sandbox_id}.sock", "o1", "s1")
	if got != "/run/asp/o1/s1.sock" {
		t.Fatalf("got %q", got)
	}
	got = ExpandSockTemplate("/tmp/{id}.sock", "", "abc")
	if got != "/tmp/abc.sock" {
		t.Fatalf("got %q", got)
	}
}

func TestRegistryBindTemplateNoGlobalFallback(t *testing.T) {
	reg := NewRegistry("/run/asp/ssh-agents/{owner_sub}.sock", "/run/asp/global.sock")
	path := reg.Bind("sb-a", "user:alice")
	if path != "/run/asp/ssh-agents/user:alice.sock" {
		t.Fatalf("path=%q", path)
	}
	got, ok := reg.Get("sb-a")
	if !ok || got != path {
		t.Fatalf("get=%q ok=%v", got, ok)
	}
	// Different owner → different path (isolation).
	pathB := reg.Bind("sb-b", "user:bob")
	if pathB == path {
		t.Fatal("owners must not share the same resolved path")
	}
	if pathB != "/run/asp/ssh-agents/user:bob.sock" {
		t.Fatalf("pathB=%q", pathB)
	}
}

func TestRegistryBindWithoutTemplateUsesFallback(t *testing.T) {
	reg := NewRegistry("", "/run/asp/global.sock")
	path := reg.Bind("sb1", "user:alice")
	if path != "/run/asp/global.sock" {
		t.Fatalf("path=%q", path)
	}
}

func TestRegistryLookupScopedFakeWhenMissing(t *testing.T) {
	reg := NewRegistry("/run/agents/{owner_sub}.sock", "/should-not-use")
	if p := reg.Lookup("unbound"); p != "" {
		t.Fatalf("unbound template mode should FakeAgent, got %q", p)
	}
	if !reg.Scoped("unbound") {
		t.Fatal("template mode → Scoped")
	}
	reg.Bind("sb1", "")
	p, ok := reg.Get("sb1")
	if !ok || p != "/run/agents/.sock" {
		// empty owner_sub → placeholder replaced with empty
		if !ok {
			t.Fatal("expected bind")
		}
	}
	reg.Unset("sb1")
	if _, ok := reg.Get("sb1"); ok {
		t.Fatal("unset failed")
	}
}

func TestRegistryRoutesServeConnToDifferentUpstreams(t *testing.T) {
	dir := t.TempDir()
	upA := filepath.Join(dir, "a.sock")
	upB := filepath.Join(dir, "b.sock")
	startFakeUpstream(t, upA)
	startFakeUpstream(t, upB)

	reg := NewRegistry(filepath.Join(dir, "{owner_sub}.sock"), "")
	// Bind using owner_sub that matches filenames a / b
	if p := reg.Bind("sb-a", "a"); p != upA {
		t.Fatalf("bind a: %q", p)
	}
	if p := reg.Bind("sb-b", "b"); p != upB {
		t.Fatalf("bind b: %q", p)
	}

	// sb-a's connection reaches upA (identities ok).
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	go ServeConn(c2, reg.Lookup("sb-a"), "sb-a", nil, nil)
	n, err := RequestIdentities(c1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("count=%d", n)
	}

	// Missing path → FakeAgent, no env leak.
	t.Setenv("SSH_AUTH_SOCK", upA)
	c3, c4 := net.Pipe()
	defer c3.Close()
	defer c4.Close()
	go ServeConn(c4, "", "sb-x", nil, nil)
	n, err = RequestIdentities(c3)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("FakeAgent count=%d", n)
	}
	// ServeConn never falls back to the process's SSH_AUTH_SOCK.
	if _, err := os.Stat(upA); err != nil {
		t.Fatal(err)
	}
}

func startFakeUpstream(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go ServeFakeAgentConn(c)
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}
