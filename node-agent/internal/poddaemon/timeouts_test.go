package poddaemon

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

type tcpTestDialer struct{ addr string }

func (d tcpTestDialer) Dial(ctx context.Context) (net.Conn, error) {
	var nd net.Dialer
	return nd.DialContext(ctx, "tcp", d.addr)
}

func fakePodDaemon(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return NewClientFromDialer(tcpTestDialer{addr: ln.Addr().String()})
}

// A stream lasts as long as the command: the buffered limit does not cut it.
func TestStreamIsNotCutByTheBufferedTimeout(t *testing.T) {
	c := fakePodDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fl := w.(http.Flusher)
		for i := 0; i < 6; i++ {
			fmt.Fprintf(w, "{\"type\":\"stdout\",\"data\":\"tick%d\\n\"}\n", i)
			fl.Flush()
			time.Sleep(100 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "{\"type\":\"exit\",\"exit_code\":0}\n")
	})
	c.BufferedTimeout = 200 * time.Millisecond
	resp, err := c.OpenExecStream(context.Background(), ExecRequest{Cmd: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("stream cut: %v (got %q)", err, body)
	}
	if !strings.Contains(string(body), "tick5") || !strings.Contains(string(body), `"exit"`) {
		t.Fatalf("incomplete stream: %q", body)
	}
}

func TestBufferedCallsKeepTheirTimeout(t *testing.T) {
	c := fakePodDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(time.Second)
		_, _ = io.WriteString(w, `{"stdout":"","stderr":"","exit_code":0}`)
	})
	c.BufferedTimeout = 100 * time.Millisecond
	started := time.Now()
	if _, err := c.Exec(context.Background(), ExecRequest{Cmd: []string{"x"}}); err == nil {
		t.Fatal("a buffered exec slower than BufferedTimeout must fail")
	}
	if err := c.WriteStdin(context.Background(), StdinMessage{ExecID: "e1", Data: "x"}); err == nil {
		t.Fatal("a stdin post slower than BufferedTimeout must fail")
	}
	if took := time.Since(started); took > 900*time.Millisecond {
		t.Fatalf("buffered calls took %s: the limit did not apply", took)
	}
}
