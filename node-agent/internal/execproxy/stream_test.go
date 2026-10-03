package execproxy

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/poddaemon"
)

type tcpDialer struct{ addr string }

func (d tcpDialer) Dial(ctx context.Context) (net.Conn, error) {
	var nd net.Dialer
	return nd.DialContext(ctx, "tcp", d.addr)
}

func TestExecStreamFlushesBeforeGuestFinishes(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	guest := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/exec" || r.URL.Query().Get("stream") != "1" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "{\"type\":\"stdout\",\"data\":\"live\\n\"}\n")
		if fl != nil {
			fl.Flush()
		}
		close(started)
		<-release
		_, _ = io.WriteString(w, "{\"type\":\"exit\",\"exit_code\":2}\n")
	})}
	go func() { _ = guest.Serve(ln) }()
	defer guest.Close()

	s := &Server{Pod: poddaemon.NewClientFromDialer(tcpDialer{addr: ln.Addr().String()})}
	proxy := httptest.NewServer(s.Handler())
	defer proxy.Close()

	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/internal/exec?stream=1", bytes.NewReader([]byte(`{"sandbox_id":"sb","cmd":["echo","live"]}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)
	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "live") {
		t.Fatalf("line=%q", line)
	}
	close(release)
	rest, _ := io.ReadAll(rd)
	if !strings.Contains(string(rest), "exit_code") {
		t.Fatalf("rest=%s", rest)
	}
}

func TestExecStreamFallsBackToBufferedJSON(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	guest := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stream") == "1" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/v1/exec" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stdout":"burst\n","stderr":"","exit_code":0}`))
	})}
	go func() { _ = guest.Serve(ln) }()
	defer guest.Close()

	s := &Server{Pod: poddaemon.NewClientFromDialer(tcpDialer{addr: ln.Addr().String()})}
	proxy := httptest.NewServer(s.Handler())
	defer proxy.Close()
	resp, err := http.Post(proxy.URL+"/v1/internal/exec?stream=1", "application/json", strings.NewReader(`{"cmd":["echo","burst"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "burst") || !strings.Contains(resp.Header.Get("Content-Type"), "ndjson") {
		t.Fatalf("ct=%s body=%s", resp.Header.Get("Content-Type"), body)
	}
}
