package poddaemon

import (
	"context"
	"net"
	"testing"
)

func TestRegistryDialerForFallback(t *testing.T) {
	fb := &UnixDialer{Path: "/tmp/fallback.sock"}
	reg := NewRegistry(fb)
	d, err := reg.DialerFor("unknown-sb")
	if err != nil {
		t.Fatal(err)
	}
	if d != fb {
		t.Fatalf("want fallback")
	}
}

func TestRegistryRegisterHybrid(t *testing.T) {
	reg := NewRegistry(nil)
	reg.Register("sb-1", Endpoint{Mode: ModeHybrid, VsockPath: "/run/asp/vsock-sb-1.sock", CID: 3, Port: 26500})
	if reg.Len() != 1 {
		t.Fatalf("len=%d", reg.Len())
	}
	d, err := reg.DialerFor("sb-1")
	if err != nil {
		t.Fatal(err)
	}
	h, ok := d.(*HybridVsockDialer)
	if !ok || h.SocketPath != "/run/asp/vsock-sb-1.sock" {
		t.Fatalf("got %+v", d)
	}
	reg.Unregister("sb-1")
	if reg.Len() != 0 {
		t.Fatal("unregister failed")
	}
	_, err = reg.DialerFor("sb-1")
	if err == nil {
		t.Fatal("expected error without fallback")
	}
}

func TestRegistryClientForUnix(t *testing.T) {
	ln, err := net.Listen("unix", t.TempDir()+"/pd.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, _ := ln.Accept()
		if conn != nil {
			_ = conn.Close()
		}
	}()
	reg := NewRegistry(nil)
	reg.Register("sb", Endpoint{Mode: ModeUnix, UnixPath: ln.Addr().String()})
	c, err := reg.ClientFor("sb")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Healthz will fail (server closes), but Dialer path is exercised.
	_ = c.Healthz(ctx)
}
