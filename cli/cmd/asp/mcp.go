package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/client"
	"github.com/luisgf/agent-sandbox-platform/cli/internal/mcp"
	"github.com/luisgf/agent-sandbox-platform/cli/internal/session"
)

const (
	// mcpMaxWrite bounds asp_write and asp_edit: the control plane reads at
	// most 1 MiB of exec body and the content travels as base64.
	mcpMaxWrite = 512 << 10
	// mcpMaxFile bounds what asp_read and asp_edit read back.
	mcpMaxFile        = 4 << 20
	mcpMaxTimeout     = time.Hour
	mcpGuestWorkspace = "/workspace"
)

const mcpInstructions = `These tools act inside an isolated ASP sandbox (a Cloud Hypervisor microVM), not on the machine running this client. The guest is a minimal Debian with POSIX sh, bash, coreutils, grep, sed and awk; commands run as root. Relative paths resolve in %s. Use asp_exec to run commands (it runs /bin/sh -c, so pipes, && and redirections work), asp_read, asp_write and asp_edit for files, asp_list and asp_grep to explore, and asp_session_info to see the sandbox.`

// mcpSession serves the sandbox of one ASP session as MCP tools.
type mcpSession struct {
	g         globalFlags
	cpURLSet  bool
	st        session.State
	base      string // guest directory for relative paths and the default cwd
	maxOutput int
	timeout   time.Duration
}

// cmdMCP is "asp mcp": a Model Context Protocol server on stdio whose tools
// run inside one session's sandbox, for harnesses that speak MCP (Claude
// Code, OpenCode, Codex, Cursor…). Only MCP messages go to stdout.
func cmdMCP(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var g globalFlags
	addGlobalFlags(fs, &g)
	var loc sessionLoc
	addSessionLocFlags(fs, &loc)
	startIt := fs.Bool("start", false, "start the session (asp session start) when it does not exist or its sandbox has ended")
	workspace := fs.String("workspace", "", "with --start: absolute host directory the guest mounts on /workspace")
	image := fs.String("image", "", "with --start: image_ref")
	stopOnExit := fs.Bool("stop-on-exit", false, "stop the session when the client disconnects")
	maxOutput := fs.Int("max-output", 30000, "bytes of stdout and of stderr an asp_exec result keeps (head and tail)")
	execTimeout := fs.Duration("exec-timeout", 2*time.Minute, "default asp_exec timeout; a call can ask for up to 1h")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: asp mcp [--name NAME] [--start [--workspace DIR] [--image REF]] [--stop-on-exit]")
		return 2
	}
	sessPath, err := resolveSessionPath(fs, loc)
	if err != nil {
		fmt.Fprintf(stderr, "mcp: %v\n", err)
		return 2
	}
	if *startIt {
		if code := ensureMCPSession(fs, g, sessPath, *workspace, *image, stderr); code != 0 {
			return code
		}
	}
	st, err := session.Load(sessPath)
	if err != nil {
		if errors.Is(err, session.ErrNoSession) {
			fmt.Fprintf(stderr, "mcp: no active session (%s). Run: asp session start --name %s, or pass --start\n", sessPath, loc.name)
			return 1
		}
		fmt.Fprintf(stderr, "mcp: %v\n", err)
		return 1
	}
	m := &mcpSession{g: g, cpURLSet: cpURLWasSet(fs), st: st, base: "/", maxOutput: *maxOutput, timeout: *execTimeout}
	if st.Workspace != "" {
		m.base = mcpGuestWorkspace
	}
	srv := &mcp.Server{Name: "asp", Version: version, Instructions: fmt.Sprintf(mcpInstructions, m.base), Tools: m.tools()}
	fmt.Fprintf(stderr, "asp mcp: serving sandbox %s (session file %s) on stdio\n", st.SandboxID, sessPath)
	serveErr := srv.Serve(context.Background(), stdin, stdout)
	if *stopOnExit {
		stop := []string{"--session-file", sessPath}
		if flagWasSet(fs, "cp-url") {
			stop = append(stop, "--cp-url", g.cpURL)
		}
		cmdSessionStop(stop, stderr, stderr)
	}
	if serveErr != nil {
		fmt.Fprintf(stderr, "mcp: %v\n", serveErr)
		return 1
	}
	return 0
}

// ensureMCPSession starts the session unless its sandbox is alive.
func ensureMCPSession(fs *flag.FlagSet, g globalFlags, sessPath, workspace, image string, stderr io.Writer) int {
	start := []string{"--session-file", sessPath}
	st, err := session.Load(sessPath)
	switch {
	case err == nil:
		if !flagWasSet(fs, "cp-url") {
			g.cpURL = st.CPURL
		}
		if c, cerr := newClient(g); cerr == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			sb, gerr := c.GetSandbox(ctx, st.SandboxID)
			cancel()
			if gerr == nil && !sandboxEnded(sb.State) {
				return 0
			}
		}
		start = append(start, "--force")
	case !errors.Is(err, session.ErrNoSession):
		fmt.Fprintf(stderr, "mcp: %v\n", err)
		return 1
	}
	for _, f := range []string{"cp-url", "api-key", "id-token", "tenant", "timeout"} {
		if flagWasSet(fs, f) {
			start = append(start, "--"+f, fs.Lookup(f).Value.String())
		}
	}
	if workspace != "" {
		start = append(start, "--workspace", workspace)
	}
	if image != "" {
		start = append(start, "--image", image)
	}
	// session start prints the id on stdout, which belongs to MCP here.
	return cmdSessionStart(start, stderr, stderr)
}

func sandboxEnded(state string) bool {
	switch state {
	case "stopping", "stopped", "failed":
		return true
	}
	return false
}

func (m *mcpSession) client() (*client.Client, error) {
	// A new client per call resolves the Bearer again: a server lives for
	// hours, longer than an access token.
	g := m.g
	if !m.cpURLSet {
		g.cpURL = m.st.CPURL
	}
	return newClient(g)
}

// guestPath resolves p in the guest: relative paths start at m.base.
func (m *mcpSession) guestPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return m.base
	}
	if path.IsAbs(p) {
		return path.Clean(p)
	}
	return path.Join(m.base, p)
}

type execOut struct {
	stdout, stderr string
	code           int
	cut            bool // some output was dropped
}

// stream runs req through the streaming exec, which has no 30s cap and no
// 1 MiB body limit, keeping at most limit bytes of each stream.
func (m *mcpSession) stream(ctx context.Context, req client.ExecRequest, limit int, progress func(string)) (execOut, error) {
	c, err := m.client()
	if err != nil {
		return execOut{}, err
	}
	so, se := &capWriter{limit: limit}, &capWriter{limit: limit}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		begin := time.Now()
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				progress(fmt.Sprintf("running for %s, %d bytes of output", time.Since(begin).Round(time.Second), so.Total()+se.Total()))
			}
		}
	}()
	code, err := c.ExecStreamIO(ctx, m.st.SandboxID, req, so, se, nil)
	close(stop)
	wg.Wait()
	return execOut{stdout: so.String(), stderr: se.String(), code: code, cut: so.Cut() || se.Cut()}, err
}

// script runs a fixed /bin/sh script with positional arguments, so paths and
// patterns from the model are never parsed by a shell.
func (m *mcpSession) script(ctx context.Context, body string, limit int, args ...string) (execOut, error) {
	argv := append([]string{"/bin/sh", "-c", body, "sh"}, args...)
	return m.stream(ctx, client.ExecRequest{Cmd: argv}, limit, func(string) {})
}

func (m *mcpSession) writeFile(ctx context.Context, p string, content []byte, mkdir bool) error {
	if len(content) > mcpMaxWrite {
		return fmt.Errorf("%d bytes is over the %d byte limit of one write", len(content), mcpMaxWrite)
	}
	c, err := m.client()
	if err != nil {
		return err
	}
	body := `base64 -d > "$1"`
	if mkdir {
		body = `mkdir -p "$(dirname "$1")" && base64 -d > "$1"`
	}
	res, err := c.Exec(ctx, m.st.SandboxID, client.ExecRequest{
		Cmd:   []string{"/bin/sh", "-c", body, "sh", p},
		Stdin: base64.StdEncoding.EncodeToString(content),
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

func failExec(err error) mcp.Result {
	return mcp.Errorf("exec in the sandbox failed: %v (asp_session_info shows whether the sandbox is still running)", err)
}

func decode(args json.RawMessage, v any) error {
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("invalid arguments: %v", err)
	}
	return nil
}

func schema(required []string, props map[string]any) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func prop(typ, desc string) map[string]any { return map[string]any{"type": typ, "description": desc} }

func (m *mcpSession) tools() []mcp.Tool {
	return []mcp.Tool{
		{
			Name: "asp_exec",
			Description: "Run a shell command inside the sandbox with /bin/sh -c (pipes, && and redirections work) and return its exit code, stdout and stderr. " +
				"Default working directory: " + m.base + ". Long outputs keep their head and tail.",
			InputSchema: schema([]string{"command"}, map[string]any{
				"command":         prop("string", "shell command"),
				"cwd":             prop("string", "working directory in the guest (default "+m.base+")"),
				"env":             map[string]any{"type": "object", "description": "extra environment variables", "additionalProperties": map[string]any{"type": "string"}},
				"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 3600, "description": "stop the command after this long (default " + strconv.Itoa(int(m.timeout/time.Second)) + ")"},
			}),
			Call: m.execTool,
		},
		{
			Name:        "asp_read",
			Description: "Read a text file in the sandbox. Lines come numbered (cat -n style); long lines are cut at 2000 characters.",
			InputSchema: schema([]string{"path"}, map[string]any{
				"path":   prop("string", "file path (relative paths start at "+m.base+")"),
				"offset": map[string]any{"type": "integer", "minimum": 1, "description": "first line (default 1)"},
				"limit":  map[string]any{"type": "integer", "minimum": 1, "description": "number of lines (default 2000)"},
			}),
			Call: m.readTool,
		},
		{
			Name:        "asp_write",
			Description: "Create or overwrite a file in the sandbox with exactly this content (parent directories are created). Up to 512 KiB.",
			InputSchema: schema([]string{"path", "content"}, map[string]any{
				"path":    prop("string", "file path (relative paths start at "+m.base+")"),
				"content": prop("string", "full file content"),
			}),
			Call: m.writeTool,
		},
		{
			Name:        "asp_edit",
			Description: "Replace an exact string in a file in the sandbox. old_string must occur exactly once unless replace_all is true. Files up to 512 KiB.",
			InputSchema: schema([]string{"path", "old_string", "new_string"}, map[string]any{
				"path":        prop("string", "file path (relative paths start at "+m.base+")"),
				"old_string":  prop("string", "exact text to replace, including whitespace"),
				"new_string":  prop("string", "replacement text"),
				"replace_all": prop("boolean", "replace every occurrence"),
			}),
			Call: m.editTool,
		},
		{
			Name:        "asp_list",
			Description: "List a directory tree in the sandbox: type (f file, d directory, l link), size and path. Skips .git.",
			InputSchema: schema(nil, map[string]any{
				"path":  prop("string", "directory (default "+m.base+")"),
				"depth": map[string]any{"type": "integer", "minimum": 1, "maximum": 10, "description": "levels to descend (default 2)"},
			}),
			Call: m.listTool,
		},
		{
			Name:        "asp_grep",
			Description: "Search file contents in the sandbox with an extended regular expression (grep -rnE). Returns path:line:text.",
			InputSchema: schema([]string{"pattern"}, map[string]any{
				"pattern":     prop("string", "extended regular expression"),
				"path":        prop("string", "file or directory (default "+m.base+")"),
				"include":     prop("string", "only files whose name matches this glob, e.g. *.go"),
				"ignore_case": prop("boolean", "case-insensitive match"),
				"max_results": map[string]any{"type": "integer", "minimum": 1, "description": "lines to return (default 200)"},
			}),
			Call: m.grepTool,
		},
		{
			Name:        "asp_session_info",
			Description: "Show the sandbox behind these tools: id, state, node, image, resources and workspace.",
			InputSchema: schema(nil, map[string]any{}),
			Call:        m.infoTool,
		},
	}
}

func (m *mcpSession) execTool(ctx context.Context, raw json.RawMessage, progress func(string)) mcp.Result {
	var a struct {
		Command        string            `json:"command"`
		Cwd            string            `json:"cwd"`
		Env            map[string]string `json:"env"`
		TimeoutSeconds int               `json:"timeout_seconds"`
	}
	if err := decode(raw, &a); err != nil {
		return mcp.Errorf("%v", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return mcp.Errorf("command is required")
	}
	timeout := m.timeout
	if a.TimeoutSeconds > 0 {
		timeout = time.Duration(a.TimeoutSeconds) * time.Second
	}
	if timeout > mcpMaxTimeout {
		timeout = mcpMaxTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req := client.ExecRequest{Cmd: []string{"/bin/sh", "-c", a.Command}, Env: a.Env}
	if a.Cwd != "" || m.base != "/" {
		req.Cwd = m.guestPath(a.Cwd)
	}
	out, err := m.stream(cctx, req, m.maxOutput, progress)
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return mcp.Errorf("timed out after %s; the command was stopped.\n%s", timeout, formatExec(out, true))
	}
	if err != nil {
		return failExec(err)
	}
	return mcp.Result{Text: formatExec(out, false)}
}

func formatExec(out execOut, partial bool) string {
	var b strings.Builder
	if !partial {
		fmt.Fprintf(&b, "exit code: %d\n", out.code)
	}
	if out.stdout != "" {
		fmt.Fprintf(&b, "stdout:\n%s", out.stdout)
		if !strings.HasSuffix(out.stdout, "\n") {
			b.WriteString("\n")
		}
	}
	if out.stderr != "" {
		fmt.Fprintf(&b, "stderr:\n%s", out.stderr)
		if !strings.HasSuffix(out.stderr, "\n") {
			b.WriteString("\n")
		}
	}
	if out.stdout == "" && out.stderr == "" {
		b.WriteString("(no output)\n")
	}
	return b.String()
}

const readScript = `f=$1; s=$2; n=$3
[ -e "$f" ] || { echo "no such file: $f" >&2; exit 2; }
[ -d "$f" ] && { echo "$f is a directory (use asp_list)" >&2; exit 2; }
t=$(wc -l < "$f")
e=$((s + n - 1)); [ "$e" -gt "$t" ] && e=$t
echo "$f: lines $s-$e of $t"
awk -v s="$s" -v e="$((s + n - 1))" 'NR >= s && NR <= e { if (length($0) > 2000) $0 = substr($0, 1, 2000) "…"; printf "%6d\t%s\n", NR, $0 } NR > e { exit }' "$f"`

func (m *mcpSession) readTool(ctx context.Context, raw json.RawMessage, _ func(string)) mcp.Result {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := decode(raw, &a); err != nil {
		return mcp.Errorf("%v", err)
	}
	if a.Offset < 1 {
		a.Offset = 1
	}
	if a.Limit < 1 {
		a.Limit = 2000
	}
	out, err := m.script(ctx, readScript, mcpMaxFile, m.guestPath(a.Path), strconv.Itoa(a.Offset), strconv.Itoa(a.Limit))
	if err != nil {
		return failExec(err)
	}
	if out.code != 0 {
		return mcp.Errorf("%s", strings.TrimSpace(out.stderr))
	}
	return mcp.Result{Text: out.stdout}
}

func (m *mcpSession) writeTool(ctx context.Context, raw json.RawMessage, _ func(string)) mcp.Result {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decode(raw, &a); err != nil {
		return mcp.Errorf("%v", err)
	}
	if strings.TrimSpace(a.Path) == "" {
		return mcp.Errorf("path is required")
	}
	p := m.guestPath(a.Path)
	if err := m.writeFile(ctx, p, []byte(a.Content), true); err != nil {
		return mcp.Errorf("write %s: %v", p, err)
	}
	return mcp.Result{Text: fmt.Sprintf("wrote %d bytes to %s", len(a.Content), p)}
}

func (m *mcpSession) editTool(ctx context.Context, raw json.RawMessage, _ func(string)) mcp.Result {
	var a struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := decode(raw, &a); err != nil {
		return mcp.Errorf("%v", err)
	}
	if a.OldString == "" {
		return mcp.Errorf("old_string is required")
	}
	if a.OldString == a.NewString {
		return mcp.Errorf("old_string and new_string are the same")
	}
	p := m.guestPath(a.Path)
	out, err := m.script(ctx, `[ -f "$1" ] || { echo "no such file: $1" >&2; exit 2; }; base64 < "$1"`, mcpMaxFile, p)
	if err != nil {
		return failExec(err)
	}
	if out.code != 0 {
		return mcp.Errorf("%s", strings.TrimSpace(out.stderr))
	}
	if out.cut {
		return mcp.Errorf("%s is too large for asp_edit (limit %d bytes)", p, mcpMaxWrite)
	}
	data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(out.stdout), ""))
	if err != nil {
		return mcp.Errorf("read %s: %v", p, err)
	}
	content := string(data)
	n := strings.Count(content, a.OldString)
	switch {
	case n == 0:
		return mcp.Errorf("old_string was not found in %s", p)
	case n > 1 && !a.ReplaceAll:
		return mcp.Errorf("old_string occurs %d times in %s: include more context to make it unique, or set replace_all", n, p)
	}
	if !a.ReplaceAll {
		n = 1
	}
	if err := m.writeFile(ctx, p, []byte(strings.Replace(content, a.OldString, a.NewString, n)), false); err != nil {
		return mcp.Errorf("write %s: %v", p, err)
	}
	return mcp.Result{Text: fmt.Sprintf("replaced %d occurrence(s) in %s", n, p)}
}

const listScript = `[ -d "$1" ] || { echo "not a directory: $1" >&2; exit 2; }
find "$1" -mindepth 1 -maxdepth "$2" -name .git -prune -o -printf '%y %10s %P\n' | sort -k3 | head -n 2000`

func (m *mcpSession) listTool(ctx context.Context, raw json.RawMessage, _ func(string)) mcp.Result {
	var a struct {
		Path  string `json:"path"`
		Depth int    `json:"depth"`
	}
	if err := decode(raw, &a); err != nil {
		return mcp.Errorf("%v", err)
	}
	if a.Depth < 1 {
		a.Depth = 2
	}
	if a.Depth > 10 {
		a.Depth = 10
	}
	dir := m.guestPath(a.Path)
	out, err := m.script(ctx, listScript, mcpMaxFile, dir, strconv.Itoa(a.Depth))
	if err != nil {
		return failExec(err)
	}
	if out.code != 0 {
		return mcp.Errorf("%s", strings.TrimSpace(out.stderr))
	}
	if strings.TrimSpace(out.stdout) == "" {
		return mcp.Result{Text: dir + " is empty\n"}
	}
	return mcp.Result{Text: dir + ":\n" + out.stdout}
}

const grepScript = `p=$1; d=$2; inc=$3; ic=$4; max=$5
set -- -rnE --exclude-dir=.git
[ "$ic" = 1 ] && set -- "$@" -i
[ -n "$inc" ] && set -- "$@" --include="$inc"
tmp=$(mktemp)
grep "$@" -e "$p" -- "$d" > "$tmp"; rc=$?
head -n "$max" "$tmp"
total=$(wc -l < "$tmp"); rm -f "$tmp"
[ "$total" -gt "$max" ] && echo "… $total matching lines, first $max shown"
exit $rc`

func (m *mcpSession) grepTool(ctx context.Context, raw json.RawMessage, _ func(string)) mcp.Result {
	var a struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		Include    string `json:"include"`
		IgnoreCase bool   `json:"ignore_case"`
		MaxResults int    `json:"max_results"`
	}
	if err := decode(raw, &a); err != nil {
		return mcp.Errorf("%v", err)
	}
	if a.Pattern == "" {
		return mcp.Errorf("pattern is required")
	}
	if a.MaxResults < 1 {
		a.MaxResults = 200
	}
	ic := "0"
	if a.IgnoreCase {
		ic = "1"
	}
	out, err := m.script(ctx, grepScript, mcpMaxFile, a.Pattern, m.guestPath(a.Path), a.Include, ic, strconv.Itoa(a.MaxResults))
	if err != nil {
		return failExec(err)
	}
	switch out.code {
	case 0:
		return mcp.Result{Text: out.stdout}
	case 1:
		return mcp.Result{Text: "no matches\n"}
	}
	return mcp.Errorf("grep: %s", strings.TrimSpace(out.stderr))
}

func (m *mcpSession) infoTool(ctx context.Context, _ json.RawMessage, _ func(string)) mcp.Result {
	c, err := m.client()
	if err != nil {
		return mcp.Errorf("%v", err)
	}
	sb, err := c.GetSandbox(ctx, m.st.SandboxID)
	if err != nil {
		return mcp.Errorf("get sandbox %s: %v", m.st.SandboxID, err)
	}
	node := ""
	if sb.NodeID != nil {
		node = *sb.NodeID
	}
	var b strings.Builder
	fmt.Fprintf(&b, "sandbox: %s\nstate: %s\nnode: %s\ntenant: %s\nimage: %s\ncpu_millis: %d\nmemory_mib: %d\n",
		sb.ID, sb.State, node, sb.TenantID, sb.ImageRef, sb.CPUMillis, sb.MemoryMiB)
	if sb.WorkspaceHostPath != "" {
		fmt.Fprintf(&b, "workspace: host %s mounted on %s\n", sb.WorkspaceHostPath, mcpGuestWorkspace)
	}
	if sb.StopReason != "" {
		fmt.Fprintf(&b, "stop_reason: %s\n", sb.StopReason)
	}
	return mcp.Result{Text: b.String()}
}

// capWriter keeps the first and last limit/2 bytes written to it.
type capWriter struct {
	mu    sync.Mutex
	limit int
	head  []byte
	tail  []byte
	total int64
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	w.total += int64(n)
	half := w.limit / 2
	if room := half - len(w.head); room > 0 {
		k := min(room, len(p))
		w.head = append(w.head, p[:k]...)
		p = p[k:]
	}
	if len(p) > 0 {
		w.tail = append(w.tail, p...)
		if keep := w.limit - half; len(w.tail) > keep {
			w.tail = append([]byte(nil), w.tail[len(w.tail)-keep:]...)
		}
	}
	return n, nil
}

func (w *capWriter) Total() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total
}

func (w *capWriter) Cut() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total > int64(len(w.head)+len(w.tail))
}

func (w *capWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	kept := int64(len(w.head) + len(w.tail))
	if w.total <= kept {
		return strings.ToValidUTF8(string(w.head)+string(w.tail), "�")
	}
	return strings.ToValidUTF8(string(w.head), "�") +
		fmt.Sprintf("\n… [%d bytes omitted] …\n", w.total-kept) +
		strings.ToValidUTF8(string(w.tail), "�")
}
