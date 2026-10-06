// Package mcp is a minimal Model Context Protocol server over stdio: JSON-RPC
// 2.0, one message per line. It covers what a tools-only server needs:
// initialize, ping, tools/list and tools/call, with calls running
// concurrently, cancellable by notifications/cancelled and reporting
// notifications/progress. No resources, prompts or sampling.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ProtocolVersions are the MCP revisions this server speaks, newest first.
// A client asking for another one gets the newest.
var ProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// Tool is one callable tool.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	// Call runs the tool. progress reports a status line while it runs; it
	// does nothing when the client asked for no progress.
	Call func(ctx context.Context, args json.RawMessage, progress func(string)) Result
}

// Result is a tool's answer. IsError marks a failed call; the model reads Text
// either way.
type Result struct {
	Text    string
	IsError bool
}

// Errorf is a failed call.
func Errorf(format string, a ...any) Result {
	return Result{Text: fmt.Sprintf(format, a...), IsError: true}
}

// Server serves Tools to one client.
type Server struct {
	Name, Version string
	// Instructions tells the client how to use the tools (initialize result).
	Instructions string
	Tools        []Tool

	outMu sync.Mutex
	out   io.Writer

	mu       sync.Mutex
	inflight map[string]*call
	wg       sync.WaitGroup
}

type call struct {
	cancel    context.CancelFunc
	cancelled bool // by the client: no response is sent
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// Serve answers the messages read from in until EOF and writes to out. When it
// returns, calls still running are cancelled and waited for.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		s.wg.Wait()
	}()
	s.out = out
	s.mu.Lock()
	s.inflight = map[string]*call{}
	s.mu.Unlock()
	r := bufio.NewReader(in)
	for {
		line, err := r.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			s.handle(ctx, line)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *Server) handle(ctx context.Context, raw []byte) {
	if raw[0] == '[' {
		// Batches left the protocol in 2025-06-18; answer each one anyway.
		var batch []json.RawMessage
		if err := json.Unmarshal(raw, &batch); err != nil || len(batch) == 0 {
			s.fail(json.RawMessage("null"), codeInvalidRequest, "invalid batch")
			return
		}
		for _, m := range batch {
			s.handle(ctx, bytes.TrimSpace(m))
		}
		return
	}
	var m message
	if err := json.Unmarshal(raw, &m); err != nil {
		s.fail(json.RawMessage("null"), codeParse, "parse error: "+err.Error())
		return
	}
	request := len(m.ID) > 0 && string(m.ID) != "null"
	switch m.Method {
	case "":
		// A response: this server sends no requests.
	case "initialize":
		if request {
			s.respond(m.ID, s.initialize(m.Params))
		}
	case "ping":
		if request {
			s.respond(m.ID, struct{}{})
		}
	case "tools/list":
		if request {
			s.respond(m.ID, map[string]any{"tools": s.toolList()})
		}
	case "tools/call":
		if request {
			s.startCall(ctx, m)
		}
	case "notifications/cancelled":
		s.cancel(m.Params)
	default:
		if request {
			s.fail(m.ID, codeMethodNotFound, "method not found: "+m.Method)
		}
	}
}

func (s *Server) initialize(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	version := ProtocolVersions[0]
	for _, v := range ProtocolVersions {
		if p.ProtocolVersion == v {
			version = v
		}
	}
	res := map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
	}
	if s.Instructions != "" {
		res["instructions"] = s.Instructions
	}
	return res
}

func (s *Server) toolList() []map[string]any {
	out := make([]map[string]any, 0, len(s.Tools))
	for _, t := range s.Tools {
		out = append(out, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema})
	}
	return out
}

func (s *Server) startCall(ctx context.Context, m message) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		s.fail(m.ID, codeInvalidParams, "invalid tools/call params: "+err.Error())
		return
	}
	var tool *Tool
	for i := range s.Tools {
		if s.Tools[i].Name == p.Name {
			tool = &s.Tools[i]
		}
	}
	if tool == nil {
		s.fail(m.ID, codeInvalidParams, "unknown tool: "+p.Name)
		return
	}
	args := p.Arguments
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	cctx, cancel := context.WithCancel(ctx)
	c := &call{cancel: cancel}
	key := idKey(m.ID)
	s.mu.Lock()
	s.inflight[key] = c
	s.mu.Unlock()

	progress := func(string) {}
	if token := p.Meta.ProgressToken; len(token) > 0 && string(token) != "null" {
		var pmu sync.Mutex
		var n int
		progress = func(msg string) {
			pmu.Lock()
			defer pmu.Unlock()
			n++
			s.notify("notifications/progress", map[string]any{"progressToken": token, "progress": n, "message": msg})
		}
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		res := run(cctx, tool, args, progress)
		s.mu.Lock()
		delete(s.inflight, key)
		dropped := c.cancelled
		s.mu.Unlock()
		if dropped {
			return
		}
		s.respond(m.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": res.Text}},
			"isError": res.IsError,
		})
	}()
}

func run(ctx context.Context, tool *Tool, args json.RawMessage, progress func(string)) (res Result) {
	defer func() {
		if r := recover(); r != nil {
			res = Errorf("tool %s failed: %v", tool.Name, r)
		}
	}()
	return tool.Call(ctx, args, progress)
}

func (s *Server) cancel(params json.RawMessage) {
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(params, &p) != nil || len(p.RequestID) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.inflight[idKey(p.RequestID)]; ok {
		c.cancelled = true
		c.cancel()
	}
}

// idKey makes "7" and " 7 " the same request id.
func idKey(id json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, id) != nil {
		return string(id)
	}
	return b.String()
}

func (s *Server) respond(id json.RawMessage, result any) {
	s.send(message{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) fail(id json.RawMessage, code int, msg string) {
	s.send(message{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (s *Server) notify(method string, params any) {
	b, err := json.Marshal(params)
	if err != nil {
		return
	}
	s.send(message{JSONRPC: "2.0", Method: method, Params: b})
}

func (s *Server) send(m message) {
	b, err := json.Marshal(m)
	if err != nil {
		b, _ = json.Marshal(message{JSONRPC: "2.0", ID: m.ID, Error: &rpcError{Code: -32603, Message: "encode: " + err.Error()}})
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	_, _ = s.out.Write(append(b, '\n'))
}
