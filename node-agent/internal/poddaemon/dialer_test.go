package poddaemon

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDialer returns a fixed conn (or error) for unit tests without /dev/vsock.
type fakeDialer struct {
	dial func(ctx context.Context) (net.Conn, error)
}

func (f *fakeDialer) Dial(ctx context.Context) (net.Conn, error) {
	return f.dial(ctx)
}

func TestClientExecViaFakeDialer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/exec" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"stdout":"hi\n","stderr":"","exit_code":0}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	d := &fakeDialer{dial: func(ctx context.Context) (net.Conn, error) {
		var nd net.Dialer
		return nd.DialContext(ctx, "tcp", strings.TrimPrefix(srv.URL, "http://"))
	}}
	c := NewClientFromDialer(d)
	out, err := c.Exec(context.Background(), ExecRequest{Cmd: []string{"echo", "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 0 || !strings.Contains(out.Stdout, "hi") {
		t.Fatalf("out=%+v", out)
	}
}

func TestHybridVsockDialerCONNECT(t *testing.T) {
	ln, err := net.Listen("unix", t.TempDir()+"/vsock.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var gotCmd string
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		line, err := br.ReadString('\n')
		if err != nil {
			t.Errorf("read cmd: %v", err)
			return
		}
		gotCmd = strings.TrimSpace(line)
		_, _ = fmt.Fprintf(conn, "OK 1073741824\n")
		// Echo one byte so dialer user can still talk on the stream.
		buf := make([]byte, 4)
		n, _ := br.Read(buf)
		_, _ = conn.Write(buf[:n])
	}()

	d := &HybridVsockDialer{SocketPath: ln.Addr().String(), Port: 26500}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := d.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	n, err := conn.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read echo: %v", err)
	}
	wg.Wait()
	if gotCmd != "CONNECT 26500" {
		t.Fatalf("cmd=%q", gotCmd)
	}
	if string(buf[:n]) != "ping" {
		t.Fatalf("echo=%q", buf[:n])
	}
}

func TestHybridVsockDialerBadACK(t *testing.T) {
	ln, err := net.Listen("unix", t.TempDir()+"/bad.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		_, _ = br.ReadString('\n')
		_, _ = fmt.Fprintf(conn, "ERR no listener\n")
	}()
	d := &HybridVsockDialer{SocketPath: ln.Addr().String(), Port: 26500}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = d.Dial(ctx)
	if err == nil {
		t.Fatal("expected error on bad ACK")
	}
}

func TestParseOKPort(t *testing.T) {
	p, err := ParseOKPort("OK 42\n")
	if err != nil || p != 42 {
		t.Fatalf("p=%d err=%v", p, err)
	}
	if _, err := ParseOKPort("NOPE"); err == nil {
		t.Fatal("expected error")
	}
}

func TestDialerFromEndpoint(t *testing.T) {
	d, err := DialerFromEndpoint(Endpoint{Mode: ModeUnix, UnixPath: "/tmp/x.sock"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.(*UnixDialer); !ok {
		t.Fatalf("got %T", d)
	}
	d, err = DialerFromEndpoint(Endpoint{Mode: ModeHybrid, VsockPath: "/run/asp/vsock-a.sock", Port: 26500})
	if err != nil {
		t.Fatal(err)
	}
	if h, ok := d.(*HybridVsockDialer); !ok || h.Port != 26500 {
		t.Fatalf("got %T %+v", d, d)
	}
	d, err = DialerFromEndpoint(Endpoint{Mode: ModeAFVsock, CID: 5, Port: 26500})
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := d.(*AFVsockDialer); !ok || a.CID != 5 {
		t.Fatalf("got %T %+v", d, d)
	}
}
