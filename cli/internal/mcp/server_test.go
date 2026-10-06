package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// session runs a Server on pipes and reads its messages one line at a time.
type session struct {
	t     *testing.T
	in    *io.PipeWriter
	lines chan map[string]any
	done  chan error
}

func start(t *testing.T, s *Server) *session {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ss := &session{t: t, in: inW, lines: make(chan map[string]any, 64), done: make(chan error, 1)}
	go func() {
		ss.done <- s.Serve(context.Background(), inR, outW)
		_ = outW.Close()
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Errorf("server wrote invalid JSON %q: %v", sc.Text(), err)
				continue
			}
			ss.lines <- m
		}
		close(ss.lines)
	}()
	t.Cleanup(func() { _ = inW.Close() })
	return ss
}

func (ss *session) send(raw string) {
	ss.t.Helper()
	if _, err := io.WriteString(ss.in, raw+"\n"); err != nil {
		ss.t.Fatal(err)
	}
}

func (ss *session) next() map[string]any {
	ss.t.Helper()
	select {
	case m, ok := <-ss.lines:
		if !ok {
			ss.t.Fatal("server closed its output")
		}
		return m
	case <-time.After(5 * time.Second):
		ss.t.Fatal("no message from the server")
	}
	return nil
}

func (ss *session) quiet(d time.Duration) {
	ss.t.Helper()
	select {
	case m := <-ss.lines:
		ss.t.Fatalf("unexpected message %v", m)
	case <-time.After(d):
	}
}

func echoServer() *Server {
	return &Server{
		Name: "test", Version: "1.0", Instructions: "use echo",
		Tools: []Tool{
			{
				Name:        "echo",
				Description: "echoes text",
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
				Call: func(_ context.Context, args json.RawMessage, progress func(string)) Result {
					var a struct{ Text string }
					_ = json.Unmarshal(args, &a)
					progress("echoing")
					if a.Text == "fail" {
						return Errorf("asked to fail")
					}
					return Result{Text: a.Text}
				},
			},
			{
				Name:        "block",
				Description: "waits until cancelled",
				InputSchema: map[string]any{"type": "object"},
				Call: func(ctx context.Context, _ json.RawMessage, _ func(string)) Result {
					<-ctx.Done()
					return Result{Text: "cancelled"}
				},
			},
		},
	}
}

func TestInitializeNegotiatesTheProtocolVersion(t *testing.T) {
	ss := start(t, echoServer())
	ss.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`)
	res := ss.next()["result"].(map[string]any)
	if res["protocolVersion"] != "2025-03-26" || res["instructions"] != "use echo" {
		t.Fatalf("initialize result %v", res)
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatalf("no tools capability: %v", res)
	}
	ss.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	ss.send(`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if v := ss.next()["result"].(map[string]any)["protocolVersion"]; v != ProtocolVersions[0] {
		t.Fatalf("unknown version answered with %v, want %s", v, ProtocolVersions[0])
	}
}

func TestToolsListAndCall(t *testing.T) {
	ss := start(t, echoServer())
	ss.send(`{"jsonrpc":"2.0","id":"a","method":"tools/list"}`)
	tools := ss.next()["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["name"] != "echo" || tools[0].(map[string]any)["inputSchema"] == nil {
		t.Fatalf("tools/list %v", tools)
	}
	ss.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`)
	m := ss.next()
	res := m["result"].(map[string]any)
	if m["id"] != float64(3) || res["isError"] != false || res["content"].([]any)[0].(map[string]any)["text"] != "hi" {
		t.Fatalf("tools/call %v", m)
	}
	ss.send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":{"text":"fail"}}}`)
	if res := ss.next()["result"].(map[string]any); res["isError"] != true {
		t.Fatalf("failed call %v", res)
	}
}

func TestProtocolErrors(t *testing.T) {
	ss := start(t, echoServer())
	for _, tc := range []struct {
		raw  string
		code float64
	}{
		{`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope"}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":6,"method":"resources/list"}`, codeMethodNotFound},
		{`{not json`, codeParse},
	} {
		ss.send(tc.raw)
		e, _ := ss.next()["error"].(map[string]any)
		if e == nil || e["code"] != tc.code {
			t.Fatalf("%s: error %v, want code %v", tc.raw, e, tc.code)
		}
	}
	// Notifications get no answer, ping gets an empty result.
	ss.send(`{"jsonrpc":"2.0","method":"notifications/whatever"}`)
	ss.send(`{"jsonrpc":"2.0","id":7,"method":"ping"}`)
	if m := ss.next(); m["id"] != float64(7) || m["result"] == nil {
		t.Fatalf("ping %v", m)
	}
}

func TestCancelledCallGetsNoResponse(t *testing.T) {
	ss := start(t, echoServer())
	ss.send(`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"block"}}`)
	ss.send(`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"echo","arguments":{"text":"other"}}}`)
	if m := ss.next(); m["id"] != float64(11) {
		t.Fatalf("a running call blocked another one: %v", m)
	}
	ss.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":10,"reason":"user"}}`)
	ss.quiet(300 * time.Millisecond)
}

func TestProgressNotifications(t *testing.T) {
	ss := start(t, echoServer())
	ss.send(`{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"echo","arguments":{"text":"x"},"_meta":{"progressToken":"tok-1"}}}`)
	p := ss.next()
	params, _ := p["params"].(map[string]any)
	if p["method"] != "notifications/progress" || params["progressToken"] != "tok-1" || params["progress"] != float64(1) || params["message"] != "echoing" {
		t.Fatalf("progress %v", p)
	}
	if m := ss.next(); m["id"] != float64(12) {
		t.Fatalf("result after progress %v", m)
	}
}

func TestEOFStopsTheServer(t *testing.T) {
	ss := start(t, echoServer())
	ss.send(`{"jsonrpc":"2.0","id":13,"method":"tools/call","params":{"name":"block"}}`)
	_ = ss.in.Close()
	select {
	case err := <-ss.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after EOF with a call running")
	}
	if !strings.Contains(strings.Join(ProtocolVersions, ","), "2025-06-18") {
		t.Fatal("2025-06-18 not offered")
	}
}
