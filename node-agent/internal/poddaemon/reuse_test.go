package poddaemon

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// fakeUnixPodDaemon serves buffered exec on a unix socket and counts connections.
func fakeUnixPodDaemon(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	dir, err := os.MkdirTemp("", "pd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "pod.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var conns atomic.Int32
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"stdout":"ok","stderr":"","exit_code":0}`)
		}),
		ConnState: func(_ net.Conn, s http.ConnState) {
			if s == http.StateNew {
				conns.Add(1)
			}
		},
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return path, &conns
}

func TestRegistryReusesTheClientAndItsConnection(t *testing.T) {
	path, conns := fakeUnixPodDaemon(t)
	r := NewRegistry(nil)
	r.Register("sb1", Endpoint{Mode: ModeUnix, UnixPath: path})

	c1, err := r.ClientFor("sb1")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := r.ClientFor("sb1")
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Fatal("two calls for one sandbox must share its client")
	}
	for i := 0; i < 3; i++ {
		if _, err := c1.Exec(context.Background(), ExecRequest{Cmd: []string{"true"}}); err != nil {
			t.Fatal(err)
		}
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("3 execs opened %d connections, want 1", n)
	}

	// Unregister forgets the client; a new registration gets a fresh one.
	r.Unregister("sb1")
	if _, err := r.ClientFor("sb1"); err == nil {
		t.Fatal("an unregistered sandbox must not get a client")
	}
	r.Register("sb1", Endpoint{Mode: ModeUnix, UnixPath: path})
	c3, err := r.ClientFor("sb1")
	if err != nil {
		t.Fatal(err)
	}
	if c3 == c1 {
		t.Fatal("a re-registered sandbox must get a new client")
	}
}

func TestRegistryChangingTheEndpointReplacesTheClient(t *testing.T) {
	path, _ := fakeUnixPodDaemon(t)
	r := NewRegistry(nil)
	r.Register("sb1", Endpoint{Mode: ModeUnix, UnixPath: path})
	c1, _ := r.ClientFor("sb1")
	r.Register("sb1", Endpoint{Mode: ModeUnix, UnixPath: path}) // same endpoint: kept
	if c, _ := r.ClientFor("sb1"); c != c1 {
		t.Fatal("re-registering the same endpoint must keep the client")
	}
	r.Register("sb1", Endpoint{Mode: ModeUnix, UnixPath: path + ".other"})
	if c, _ := r.ClientFor("sb1"); c == c1 {
		t.Fatal("a new endpoint must get a new client")
	}
}
