package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestExecStreamForwardsStdinAfterReady(t *testing.T) {
	var mu sync.Mutex
	var gotExec ExecRequest
	var gotStdin []map[string]any
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stream") != "1" {
			t.Errorf("stream query=%s", r.URL.RawQuery)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotExec); err != nil {
			t.Errorf("decode exec: %v", err)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "{\"type\":\"ready\",\"exec_id\":\"ex1\"}\n")
		if fl != nil {
			fl.Flush()
		}
		<-release
		_, _ = io.WriteString(w, "{\"type\":\"stdout\",\"data\":\"hello-stdin\\n\"}\n")
		_, _ = io.WriteString(w, "{\"type\":\"exit\",\"exit_code\":0}\n")
		if fl != nil {
			fl.Flush()
		}
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec/stdin", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("stdin json: %v", err)
		}
		mu.Lock()
		gotStdin = append(gotStdin, body)
		n := len(gotStdin)
		mu.Unlock()
		if n == 1 {
			if body["data"] != "ping\n" {
				t.Errorf("first stdin=%v", body)
			}
		}
		if n >= 2 {
			if body["close"] != true {
				t.Errorf("close=%v", body)
			}
			close(release)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(srv.URL, "")
	var out strings.Builder
	code, err := c.ExecStreamIO(context.Background(), "sb", ExecRequest{
		Cmd: []string{"cat"}, PTY: true, Rows: 24, Cols: 80,
	}, &out, io.Discard, strings.NewReader("ping\n"))
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(out.String(), "hello-stdin") {
		t.Fatalf("stdout=%q", out.String())
	}
	if !gotExec.PTY || gotExec.Rows != 24 || gotExec.Cols != 80 || gotExec.Cmd[0] != "cat" {
		t.Fatalf("exec=%+v", gotExec)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gotStdin) < 2 {
		t.Fatalf("stdin posts=%v", gotStdin)
	}
}

func TestExecStreamIOIgnoresReadyWhenNoStdin(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, "{\"type\":\"ready\",\"exec_id\":\"ex\"}\n{\"type\":\"stdout\",\"data\":\"z\"}\n{\"type\":\"exit\",\"exit_code\":7}\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(srv.URL, "")
	var out strings.Builder
	code, err := c.ExecStream(context.Background(), "sb", ExecRequest{Cmd: []string{"true"}}, &out, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 || out.String() != "z" {
		t.Fatalf("code=%d out=%q", code, out.String())
	}
}
