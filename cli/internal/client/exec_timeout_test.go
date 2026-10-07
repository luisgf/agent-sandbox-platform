package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A buffered exec is not cut at the 60 s of the client's JSON calls: it waits for
// the command. The client's own timeout still applies to everything else.
func TestExecIsNotCutAtTheJSONClientTimeout(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(ExecResult{Stdout: "done\n"})
	})
	mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(Sandbox{ID: "sb"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(srv.URL, "")
	c.HTTPClient.Timeout = 100 * time.Millisecond // stands for the 60 s

	if _, err := c.GetSandbox(context.Background(), "sb"); err == nil {
		t.Fatal("a plain call outlived the client timeout")
	}
	res, err := c.Exec(context.Background(), "sb", ExecRequest{Cmd: []string{"true"}})
	if err != nil || res.Stdout != "done\n" {
		t.Fatalf("Exec: %+v %v", res, err)
	}
	if c.HTTPClient.Timeout != 100*time.Millisecond {
		t.Fatal("Exec changed the shared client's timeout")
	}
}

// The wait is the time the request gives the command plus a margin, not forever.
func TestExecWaitsForTheTimeoutItAskedFor(t *testing.T) {
	oldMargin := execMargin
	execMargin = 200 * time.Millisecond
	defer func() { execMargin = oldMargin }()
	var mu sync.Mutex
	var sent ExecRequest
	hang := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		var req ExecRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		sent = req
		mu.Unlock()
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer close(hang)
	c := New(srv.URL, "")

	started := time.Now()
	_, err := c.Exec(context.Background(), "sb", ExecRequest{Cmd: []string{"sleep", "9"}, TimeoutSeconds: 1})
	waited := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("err=%v", err)
	}
	if waited < 1100*time.Millisecond || waited > 4*time.Second {
		t.Fatalf("waited %v: want the 1 s asked for plus the margin", waited)
	}
	mu.Lock()
	defer mu.Unlock()
	if sent.TimeoutSeconds != 1 {
		t.Fatalf("the request carried timeout_seconds=%d", sent.TimeoutSeconds)
	}
}
