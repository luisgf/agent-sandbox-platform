package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/client"
	"github.com/luisgf/agent-sandbox-platform/cli/internal/session"
)

// localGuestCP is a control plane whose "guest" is this machine: exec runs the
// requested argv here, so the tools' shell scripts run for real. Paths in the
// tests point into a temp dir.
func localGuestCP(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		node := "node-a"
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: r.PathValue("id"), State: "running", NodeID: &node, TenantID: "default", ImageRef: "debian:bookworm-slim", CPUMillis: 1000, MemoryMiB: 512})
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		var req client.ExecRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Cmd) == 0 {
			http.Error(w, `{"error":"bad exec"}`, http.StatusBadRequest)
			return
		}
		cmd := exec.CommandContext(r.Context(), req.Cmd[0], req.Cmd[1:]...)
		cmd.Dir = req.Cwd
		cmd.Stdin = strings.NewReader(req.Stdin)
		if len(req.Env) > 0 {
			cmd.Env = os.Environ()
			for k, v := range req.Env {
				cmd.Env = append(cmd.Env, k+"="+v)
			}
		}
		var so, se bytes.Buffer
		cmd.Stdout, cmd.Stderr = &so, &se
		code := 0
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				fmt.Fprintf(&se, "%v", err)
				code = 127
			} else {
				code = ee.ExitCode()
			}
		}
		if r.URL.Query().Get("stream") != "1" {
			_ = json.NewEncoder(w).Encode(client.ExecResult{Stdout: so.String(), Stderr: se.String(), ExitCode: code})
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		_ = enc.Encode(map[string]any{"type": "stdout", "data": so.String()})
		_ = enc.Encode(map[string]any{"type": "stderr", "data": se.String()})
		_ = enc.Encode(map[string]any{"type": "exit", "exit_code": code})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// mcpClient drives "asp mcp" over pipes.
type mcpClient struct {
	t     *testing.T
	in    *io.PipeWriter
	lines chan map[string]any
	done  chan int
	id    int
}

func startMCP(t *testing.T, args ...string) *mcpClient {
	t.Helper()
	t.Setenv("ASP_API_KEY", "")
	t.Setenv("ASP_ID_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &mcpClient{t: t, in: inW, lines: make(chan map[string]any, 16), done: make(chan int, 1)}
	go func() {
		c.done <- cmdMCP(args, inR, outW, io.Discard)
		_ = outW.Close()
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Errorf("stdout carried a non-MCP line %q", sc.Text())
				continue
			}
			c.lines <- m
		}
		close(c.lines)
	}()
	t.Cleanup(func() { _ = inW.Close() })
	return c
}

func (c *mcpClient) request(method string, params any) map[string]any {
	c.t.Helper()
	c.id++
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		c.t.Fatal(err)
	}
	for {
		select {
		case m, ok := <-c.lines:
			if !ok {
				c.t.Fatal("asp mcp closed stdout")
			}
			if m["id"] == float64(c.id) {
				return m
			}
		case <-time.After(10 * time.Second):
			c.t.Fatalf("no answer to %s", method)
		}
	}
}

// call runs a tool and returns its text and isError.
func (c *mcpClient) call(name string, args map[string]any) (string, bool) {
	c.t.Helper()
	m := c.request("tools/call", map[string]any{"name": name, "arguments": args})
	res, ok := m["result"].(map[string]any)
	if !ok {
		c.t.Fatalf("%s: %v", name, m)
	}
	return res["content"].([]any)[0].(map[string]any)["text"].(string), res["isError"] == true
}

func writeSession(t *testing.T, cpURL string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.json")
	if err := session.Save(p, session.State{Name: "mcp", SandboxID: "sb-1", CPURL: cpURL, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMCPToolsAgainstAGuest(t *testing.T) {
	srv := localGuestCP(t)
	c := startMCP(t, "--session-file", writeSession(t, srv.URL))
	dir := t.TempDir()

	ini := c.request("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1"}})
	if res := ini["result"].(map[string]any); res["serverInfo"].(map[string]any)["name"] != "asp" || !strings.Contains(res["instructions"].(string), "microVM") {
		t.Fatalf("initialize %v", ini)
	}
	tools := c.request("tools/list", nil)["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, tl := range tools {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "asp_exec,asp_read,asp_write,asp_edit,asp_list,asp_grep,asp_session_info" {
		t.Fatalf("tools %v", names)
	}

	// Content the shell would mangle: quotes, $, a leading # line, no final newline.
	file := filepath.Join(dir, "src", "app.sh")
	content := "#!/bin/sh\n# say 'hi' to \"$USER\"\necho \"total: $((1 + 2))\" | tr a-z A-Z"
	if text, isErr := c.call("asp_write", map[string]any{"path": file, "content": content}); isErr || !strings.Contains(text, fmt.Sprintf("wrote %d bytes", len(content))) {
		t.Fatalf("asp_write: %q", text)
	}
	if got, _ := os.ReadFile(file); string(got) != content {
		t.Fatalf("file holds %q, want %q", got, content)
	}
	text, isErr := c.call("asp_exec", map[string]any{"command": "sh " + file + " && echo oops >&2; exit 3"})
	if isErr || !strings.Contains(text, "exit code: 3") || !strings.Contains(text, "TOTAL: 3") || !strings.Contains(text, "stderr:\noops") {
		t.Fatalf("asp_exec: %q", text)
	}
	if text, _ := c.call("asp_read", map[string]any{"path": file, "offset": 2, "limit": 1}); !strings.Contains(text, "lines 2-2") || !strings.Contains(text, "     2\t# say 'hi'") || strings.Contains(text, "echo") {
		t.Fatalf("asp_read: %q", text)
	}
	if text, isErr := c.call("asp_edit", map[string]any{"path": file, "old_string": "1 + 2", "new_string": "40 + 2"}); isErr || !strings.Contains(text, "replaced 1") {
		t.Fatalf("asp_edit: %q", text)
	}
	if got, _ := os.ReadFile(file); string(got) != strings.Replace(content, "1 + 2", "40 + 2", 1) {
		t.Fatalf("edited file %q", got)
	}
	if text, isErr := c.call("asp_edit", map[string]any{"path": file, "old_string": "nope", "new_string": "x"}); !isErr || !strings.Contains(text, "not found") {
		t.Fatalf("asp_edit of a missing string: %q", text)
	}
	if text, _ := c.call("asp_grep", map[string]any{"pattern": "TOTAL", "path": dir, "ignore_case": true}); !strings.Contains(text, "app.sh:3:") {
		t.Fatalf("asp_grep: %q", text)
	}
	if text, _ := c.call("asp_grep", map[string]any{"pattern": "absent-string", "path": dir}); text != "no matches\n" {
		t.Fatalf("asp_grep without matches: %q", text)
	}
	if text, isErr := c.call("asp_session_info", nil); isErr || !strings.Contains(text, "state: running") || !strings.Contains(text, "node: node-a") {
		t.Fatalf("asp_session_info: %q", text)
	}
	if text, isErr := c.call("asp_read", map[string]any{"path": filepath.Join(dir, "missing")}); !isErr || !strings.Contains(text, "no such file") {
		t.Fatalf("asp_read of a missing file: %q", text)
	}

	_ = c.in.Close()
	select {
	case code := <-c.done:
		if code != 0 {
			t.Fatalf("asp mcp exit %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("asp mcp did not exit after EOF")
	}
}

func TestMCPListNeedsGNUFind(t *testing.T) {
	if err := exec.Command("find", "/", "-maxdepth", "0", "-printf", "").Run(); err != nil {
		t.Skip("find has no -printf here (the guest is Debian)")
	}
	srv := localGuestCP(t)
	c := startMCP(t, "--session-file", writeSession(t, srv.URL))
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	text, isErr := c.call("asp_list", map[string]any{"path": dir, "depth": 3})
	if isErr || !strings.Contains(text, "a/f.txt") || strings.Contains(text, ".git") {
		t.Fatalf("asp_list: %q", text)
	}
}

func TestMCPExecTimeoutStopsTheCommand(t *testing.T) {
	srv := localGuestCP(t)
	c := startMCP(t, "--session-file", writeSession(t, srv.URL))
	start := time.Now()
	text, isErr := c.call("asp_exec", map[string]any{"command": "sleep 30", "timeout_seconds": 1})
	if !isErr || !strings.Contains(text, "timed out after 1s") {
		t.Fatalf("asp_exec with a timeout: %q", text)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("timeout took %v", time.Since(start))
	}
}

func TestMCPWithoutASessionFails(t *testing.T) {
	t.Setenv("ASP_IDP_REQUIRED", "")
	var stderr bytes.Buffer
	code := cmdMCP([]string{"--session-file", filepath.Join(t.TempDir(), "none.json")}, strings.NewReader(""), io.Discard, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "--start") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}

func TestCapWriterKeepsHeadAndTail(t *testing.T) {
	w := &capWriter{limit: 10}
	for _, s := range []string{"01234", "56789", "abcdefghij"} {
		_, _ = w.Write([]byte(s))
	}
	if got := w.String(); !strings.HasPrefix(got, "01234") || !strings.HasSuffix(got, "fghij") || !strings.Contains(got, "[10 bytes omitted]") || !w.Cut() {
		t.Fatalf("capWriter %q", got)
	}
	small := &capWriter{limit: 64}
	_, _ = small.Write([]byte("short"))
	if small.String() != "short" || small.Cut() {
		t.Fatalf("small capWriter %q", small.String())
	}
}
