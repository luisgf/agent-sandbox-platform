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
	"time"

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

// A stream through the proxy that outlasts the pod-daemon client's buffered
// limit reaches the caller whole (the client used to cut it at 60 s).
func TestExecStreamOutlastsTheBufferedLimit(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	guest := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fl, _ := w.(http.Flusher)
		for i := 0; i < 5; i++ {
			_, _ = io.WriteString(w, "{\"type\":\"stdout\",\"data\":\"tick\\n\"}\n")
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(100 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "{\"type\":\"exit\",\"exit_code\":0}\n")
	})}
	go func() { _ = guest.Serve(ln) }()
	defer guest.Close()

	pod := poddaemon.NewClientFromDialer(tcpDialer{addr: ln.Addr().String()})
	pod.BufferedTimeout = 150 * time.Millisecond
	proxy := httptest.NewServer((&Server{Pod: pod}).Handler())
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/internal/exec?stream=1", "application/json", strings.NewReader(`{"sandbox_id":"sb","cmd":["x"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || !strings.Contains(string(body), `"exit_code":0`) || strings.Count(string(body), "tick") != 5 {
		t.Fatalf("stream cut: err=%v body=%q", err, body)
	}
}

// The proxy sends the stream's headers as soon as the guest does, so a command
// that prints nothing for a while does not trip the caller's response-header
// timeout (the control plane waits 30 s for them).
func TestExecStreamSendsHeadersBeforeTheFirstOutput(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	guest := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(400 * time.Millisecond)
		_, _ = io.WriteString(w, "{\"type\":\"exit\",\"exit_code\":0}\n")
	})}
	go func() { _ = guest.Serve(ln) }()
	defer guest.Close()

	proxy := httptest.NewServer((&Server{Pod: poddaemon.NewClientFromDialer(tcpDialer{addr: ln.Addr().String()})}).Handler())
	defer proxy.Close()
	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 150 * time.Millisecond}}
	resp, err := client.Post(proxy.URL+"/v1/internal/exec?stream=1", "application/json", strings.NewReader(`{"sandbox_id":"sb","cmd":["sleep"]}`))
	if err != nil {
		t.Fatalf("headers did not arrive before the first output: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"exit_code":0`) {
		t.Fatalf("body=%q", body)
	}
}
