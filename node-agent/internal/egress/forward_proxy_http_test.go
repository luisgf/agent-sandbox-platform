package egress

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A response bigger than the request-body limit comes back whole, with a
// Content-Length that matches what is sent: no client waiting for missing bytes.
func TestForwardProxyStreamsResponsesLargerThanTheBodyLimit(t *testing.T) {
	payload := bytes.Repeat([]byte("r"), 4096)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer upstream.Close()
	p := &ForwardProxy{Guard: testGuard, Default: allowServer(t, upstream.URL), Enforce: true, MaxBodyBytes: 1024}
	proxy := httptest.NewServer(p)
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	resp, err := client.Get(upstream.URL + "/big")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, payload) {
		t.Fatalf("status %d, %d bytes: want 200 and the whole %d-byte body", resp.StatusCode, len(got), len(payload))
	}
}

func TestForwardProxyStripsHopByHopHeaders(t *testing.T) {
	var mu sync.Mutex
	var seen http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Connection", "X-Upstream-Secret")
		w.Header().Set("X-Upstream-Secret", "1")
		w.Header().Set("Keep-Alive", "timeout=5")
		w.Header().Set("X-Normal", "yes")
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	p := &ForwardProxy{Guard: testGuard, Default: allowServer(t, upstream.URL), Enforce: true}

	req := httptest.NewRequest(http.MethodGet, upstream.URL+"/", nil)
	req.Header.Set("Connection", "keep-alive, X-Foo")
	req.Header.Set("X-Foo", "bar")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("Proxy-Authorization", "Basic eA==")
	req.Header.Set("Te", "trailers")
	req.Header.Set("X-Keep", "1")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if seen.Get("X-Keep") != "1" {
		t.Fatalf("end-to-end header lost: %v", seen)
	}
	for _, h := range []string{"X-Foo", "Upgrade", "Keep-Alive", "Proxy-Authorization", "Te"} {
		if v := seen.Get(h); v != "" {
			t.Errorf("hop-by-hop %s reached the upstream: %q", h, v)
		}
	}
	if strings.Contains(strings.ToLower(seen.Get("Connection")), "x-foo") {
		t.Errorf("guest Connection header reached the upstream: %q", seen.Get("Connection"))
	}
	if rr.Header().Get("X-Normal") != "yes" {
		t.Fatalf("end-to-end response header lost: %v", rr.Header())
	}
	for _, h := range []string{"X-Upstream-Secret", "Keep-Alive", "Connection"} {
		if v := rr.Header().Get(h); v != "" {
			t.Errorf("hop-by-hop response header %s reached the guest: %q", h, v)
		}
	}
}

func TestForwardProxyRejectsOversizedRequestBody(t *testing.T) {
	var calls int
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
	}))
	defer upstream.Close()
	p := &ForwardProxy{Guard: testGuard, Default: allowServer(t, upstream.URL), Enforce: true, MaxBodyBytes: 16}

	req := httptest.NewRequest(http.MethodPost, upstream.URL+"/", strings.NewReader(strings.Repeat("b", 100)))
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d", rr.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatal("an oversized body must be refused before dialing the upstream")
	}
}

func TestForwardProxyKeepsRequestContentLength(t *testing.T) {
	type got struct {
		length int64
		te     []string
		body   string
	}
	ch := make(chan got, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- got{r.ContentLength, r.TransferEncoding, string(b)}
	}))
	defer upstream.Close()
	p := &ForwardProxy{Guard: testGuard, Default: allowServer(t, upstream.URL), Enforce: true}

	req := httptest.NewRequest(http.MethodPost, upstream.URL+"/", strings.NewReader("hello"))
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	g := <-ch
	if g.length != 5 || len(g.te) != 0 || g.body != "hello" {
		t.Fatalf("upstream saw content-length %d, transfer-encoding %v, body %q: want 5, none, hello", g.length, g.te, g.body)
	}
}
