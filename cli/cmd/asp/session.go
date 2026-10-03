package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/client"
	"github.com/luisgf/agent-sandbox-platform/cli/internal/cmdline"
	"github.com/luisgf/agent-sandbox-platform/cli/internal/session"
	"github.com/luisgf/agent-sandbox-platform/cli/internal/wait"
)

func sessionCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "session subcommand required (start|exec|status|stop)")
		return 2
	}
	switch args[0] {
	case "start":
		return cmdSessionStart(args[1:], stdout, stderr)
	case "exec":
		return cmdSessionExec(args[1:], stdout, stderr)
	case "status":
		return cmdSessionStatus(args[1:], stdout, stderr)
	case "stop", "destroy", "rm":
		return cmdSessionStop(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprintln(stderr, `asp session — reusable sandbox for a local agent shell tool

  asp session start [--name NAME] [--workspace /path] [flags]
  asp session exec  [--name NAME] (--cmd '…' | -- argv…)
  asp session status [--name NAME] [--json]
  asp session stop  [--name NAME]

Named sessions: ~/.cache/asp/sessions/<name>.json (default name "default").
Override the directory with --session-dir / ASP_SESSION_DIR.
--session-file / ASP_SESSION_FILE still selects one explicit file and ignores --name.
Not an OpenCode plugin. See docs/ops-asp-session.md.`)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown session subcommand %q\n", args[0])
		return 2
	}
}

type sessionLoc struct {
	file string
	dir  string
	name string
}

func addSessionLocFlags(fs *flag.FlagSet, loc *sessionLoc) {
	fs.StringVar(&loc.file, "session-file", "", "explicit session JSON; overrides --name (env ASP_SESSION_FILE if this flag is omitted)")
	fs.StringVar(&loc.dir, "session-dir", session.DefaultDir(), "directory of named sessions (env ASP_SESSION_DIR)")
	fs.StringVar(&loc.name, "name", session.DefaultName, "session name (file <session-dir>/<name>.json)")
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func resolveSessionPath(fs *flag.FlagSet, loc sessionLoc) (string, error) {
	if flagWasSet(fs, "session-file") {
		p := strings.TrimSpace(loc.file)
		if p == "" {
			return "", fmt.Errorf("session path is empty")
		}
		return p, nil
	}
	if p := strings.TrimSpace(os.Getenv("ASP_SESSION_FILE")); p != "" {
		return p, nil
	}
	return session.NamedPath(loc.dir, loc.name)
}

func cleanWorkspaceFlag(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", nil
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("--workspace must be an absolute path")
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("--workspace: %w", err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("--workspace %s is not a directory", p)
	}
	return filepath.Clean(p), nil
}

// cpURLWasSet reports whether the user passed --cp-url on this command.
func cpURLWasSet(fs *flag.FlagSet) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "cp-url" {
			set = true
		}
	})
	return set
}

func cmdSessionStart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("session start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var g globalFlags
	addGlobalFlags(fs, &g)
	var loc sessionLoc
	addSessionLocFlags(fs, &loc)
	image := fs.String("image", "debian:bookworm-slim", "image_ref")
	cpu := fs.Int("cpu-millis", 1000, "cpu_millis")
	mem := fs.Int("memory-mib", 512, "memory_mib")
	node := fs.String("node-id", "", "optional node pin")
	vmm := fs.String("vmm-profile", "cloud-hypervisor", "vmm_profile")
	workspace := fs.String("workspace", "", "absolute host directory to record as the session workspace (guest mount /workspace when virtiofs exists; CH does not start virtiofsd)")
	force := fs.Bool("force", false, "destroy any sandbox recorded in the session file, then start a new one")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ws, err := cleanWorkspaceFlag(*workspace)
	if err != nil {
		fmt.Fprintf(stderr, "session start: %v\n", err)
		return 2
	}
	name, nerr := session.ValidateName(loc.name)
	if nerr != nil {
		fmt.Fprintf(stderr, "session start: %v\n", nerr)
		return 2
	}
	path, err := resolveSessionPath(fs, loc)
	if err != nil {
		fmt.Fprintf(stderr, "session start: %v\n", err)
		return 2
	}
	existing, loadErr := session.Load(path)
	if loadErr == nil {
		if !*force {
			fmt.Fprintf(stderr, "session start: active session %s in %s (asp session stop, or --force)\n", existing.SandboxID, path)
			return 1
		}
	} else if !errors.Is(loadErr, session.ErrNoSession) {
		fmt.Fprintf(stderr, "session start: %v\n", loadErr)
		return 1
	}

	c, code := mustClient(g, stderr)
	if c == nil {
		return code
	}
	ctx := context.Background()
	if loadErr == nil && *force {
		if err := destroyRecorded(ctx, c, existing, stderr); err != nil {
			fmt.Fprintf(stderr, "session start: --force destroy %s: %v\n", existing.SandboxID, err)
			return 1
		}
		if err := session.Clear(path); err != nil {
			fmt.Fprintf(stderr, "session start: clear %s: %v\n", path, err)
			return 1
		}
	}

	sb, err := c.CreateSandbox(ctx, client.CreateInput{
		TenantID:          g.tenant,
		ImageRef:          *image,
		CPUMillis:         *cpu,
		MemoryMiB:         *mem,
		VMMProfile:        *vmm,
		NodeID:            *node,
		WorkspaceHostPath: ws,
	})
	if err != nil {
		fmt.Fprintf(stderr, "session start: create: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "asp: created sandbox %s state=%s\n", sb.ID, sb.State)
	createdID := sb.ID

	if sb.State != "running" {
		waitCtx, cancel := context.WithTimeout(ctx, g.timeout)
		sb, err = wait.WaitForState(waitCtx, c.GetSandbox, createdID, "running", wait.Options{
			Timeout: g.timeout,
			Log:     stderr,
		})
		cancel()
		if err != nil {
			fmt.Fprintf(stderr, "session start: wait: %v\n", err)
			if _, derr := c.DeleteSandbox(context.Background(), createdID); derr != nil {
				fmt.Fprintf(stderr, "session start: cleanup %s: %v\n", createdID, derr)
			} else {
				fmt.Fprintf(stderr, "asp: destroyed sandbox %s after failed start\n", createdID)
			}
			return 1
		}
	}

	st := session.State{
		Name:      name,
		SandboxID: sb.ID,
		CPURL:     c.BaseURL,
		TenantID:  sb.TenantID,
		ImageRef:  sb.ImageRef,
		Workspace: ws,
		CreatedAt: time.Now().UTC(),
	}
	if st.TenantID == "" {
		st.TenantID = g.tenant
	}
	if st.ImageRef == "" {
		st.ImageRef = *image
	}
	if err := session.Save(path, st); err != nil {
		fmt.Fprintf(stderr, "session start: save %s: %v\n", path, err)
		if _, derr := c.DeleteSandbox(context.Background(), sb.ID); derr != nil {
			fmt.Fprintf(stderr, "session start: cleanup %s: %v (sandbox id was %s)\n", sb.ID, derr, sb.ID)
		}
		return 1
	}
	fmt.Fprintf(stderr, "asp: session started %s cp=%s file=%s\n", sb.ID, st.CPURL, path)
	fmt.Fprintln(stdout, sb.ID)
	return 0
}

func cmdSessionExec(args []string, stdout, stderr io.Writer) int {
	before, after := cmdline.SplitDashDash(args)
	fs := flag.NewFlagSet("session exec", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var g globalFlags
	addGlobalFlags(fs, &g)
	var loc sessionLoc
	addSessionLocFlags(fs, &loc)
	cmdFlag := fs.String("cmd", "", "command string (quoted words)")
	cwd := fs.String("cwd", "", "working directory in guest")
	buffered := fs.Bool("buffered", false, "wait for the full JSON exec body instead of streaming NDJSON")
	if err := fs.Parse(before); err != nil {
		return 2
	}
	trail := after
	if len(trail) == 0 && len(fs.Args()) > 0 {
		trail = fs.Args()
	}
	argv, err := cmdline.FromFlagAndArgs(*cmdFlag, trail)
	if err != nil {
		fmt.Fprintf(stderr, "session exec: %v\n", err)
		return 2
	}
	path, err := resolveSessionPath(fs, loc)
	if err != nil {
		fmt.Fprintf(stderr, "session exec: %v\n", err)
		return 2
	}
	st, err := session.Load(path)
	if err != nil {
		if errors.Is(err, session.ErrNoSession) {
			fmt.Fprintf(stderr, "session exec: no active session (%s). Run: asp session start\n", path)
			return 1
		}
		fmt.Fprintf(stderr, "session exec: %v\n", err)
		return 1
	}
	if !cpURLWasSet(fs) {
		g.cpURL = st.CPURL
	}
	c, code := mustClient(g, stderr)
	if c == nil {
		return code
	}
	if sb, gerr := c.GetSandbox(context.Background(), st.SandboxID); gerr == nil && sb.IdleReaped() {
		fmt.Fprintf(stderr, "session exec: %s\n", idleReapedText(st.SandboxID, path))
		return 1
	}
	req := client.ExecRequest{Cmd: argv, Cwd: *cwd}
	// --json and --buffered keep the accumulated JSON path (smokes, one blob).
	// Default prints stdout/stderr as NDJSON chunks arrive.
	if g.jsonOut || *buffered {
		res, err := c.Exec(context.Background(), st.SandboxID, req)
		if err != nil {
			if idleReapedErr(err) {
				fmt.Fprintf(stderr, "session exec: %s\n", idleReapedText(st.SandboxID, path))
				return 1
			}
			fmt.Fprintf(stderr, "session exec: sandbox %s: %v\n", st.SandboxID, err)
			return 1
		}
		if code := writeExec(stdout, stderr, res, g.jsonOut); code != 0 {
			return code
		}
		return res.ExitCode
	}
	code, err = c.ExecStream(context.Background(), st.SandboxID, req, stdout, stderr)
	if err != nil {
		if idleReapedErr(err) {
			fmt.Fprintf(stderr, "session exec: %s\n", idleReapedText(st.SandboxID, path))
			return 1
		}
		fmt.Fprintf(stderr, "session exec: sandbox %s: %v\n", st.SandboxID, err)
		return 1
	}
	return code
}

func cmdSessionStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("session status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var g globalFlags
	addGlobalFlags(fs, &g)
	var loc sessionLoc
	addSessionLocFlags(fs, &loc)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	path, err := resolveSessionPath(fs, loc)
	if err != nil {
		fmt.Fprintf(stderr, "session status: %v\n", err)
		return 2
	}
	st, err := session.Load(path)
	if err != nil {
		if errors.Is(err, session.ErrNoSession) {
			fmt.Fprintf(stderr, "asp: no active session (%s)\n", path)
			return 1
		}
		fmt.Fprintf(stderr, "session status: %v\n", err)
		return 1
	}
	if !cpURLWasSet(fs) {
		g.cpURL = st.CPURL
	}
	c, code := mustClient(g, stderr)
	if c == nil {
		return code
	}
	sb, err := c.GetSandbox(context.Background(), st.SandboxID)
	type view struct {
		Name        string          `json:"name,omitempty"`
		SessionFile string          `json:"session_file"`
		SandboxID   string          `json:"sandbox_id"`
		CPURL       string          `json:"cp_url"`
		TenantID    string          `json:"tenant_id,omitempty"`
		ImageRef    string          `json:"image_ref,omitempty"`
		Workspace   string          `json:"workspace,omitempty"`
		CreatedAt   string          `json:"created_at,omitempty"`
		Live        *client.Sandbox `json:"live,omitempty"`
		LiveError   string          `json:"live_error,omitempty"`
		IdleReaped  bool            `json:"idle_reaped,omitempty"`
	}
	out := view{
		Name:        st.Name,
		SessionFile: path,
		SandboxID:   st.SandboxID,
		CPURL:       st.CPURL,
		TenantID:    st.TenantID,
		ImageRef:    st.ImageRef,
		Workspace:   st.Workspace,
	}
	if !st.CreatedAt.IsZero() {
		out.CreatedAt = st.CreatedAt.UTC().Format(time.RFC3339)
	}
	if err != nil {
		out.LiveError = err.Error()
		if g.jsonOut {
			_ = writeJSON(stdout, out)
		} else {
			fmt.Fprintf(stdout, "id=%s name=%s file=%s cp=%s live_error=%s\n", st.SandboxID, st.Name, path, st.CPURL, err.Error())
		}
		fmt.Fprintf(stderr, "session status: get %s: %v\n", st.SandboxID, err)
		return 1
	}
	out.Live = &sb
	out.IdleReaped = sb.IdleReaped()
	if out.IdleReaped {
		fmt.Fprintf(stderr, "session status: %s\n", idleReapedText(st.SandboxID, path))
	}
	if g.jsonOut {
		code := writeJSON(stdout, out)
		if code != 0 {
			return code
		}
		if out.IdleReaped {
			return 1
		}
		return 0
	}
	if out.IdleReaped {
		fmt.Fprintf(stdout, "id=%s name=%s state=%s tenant=%s cp=%s file=%s workspace=%s stop_reason=%s idle_reaped=true\n", sb.ID, st.Name, sb.State, sb.TenantID, st.CPURL, path, st.Workspace, sb.StopReason)
		return 1
	}
	fmt.Fprintf(stdout, "id=%s name=%s state=%s tenant=%s cp=%s file=%s workspace=%s\n", sb.ID, st.Name, sb.State, sb.TenantID, st.CPURL, path, st.Workspace)
	return 0
}

func idleReapedText(id, path string) string {
	return fmt.Sprintf("sandbox %s was stopped after idle timeout (reaped). Session file kept (%s). Run: asp session start --force", id, path)
}

func idleReapedErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "idle timeout")
}

func cmdSessionStop(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("session stop", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var g globalFlags
	addGlobalFlags(fs, &g)
	var loc sessionLoc
	addSessionLocFlags(fs, &loc)
	localOnly := fs.Bool("local", false, "clear the state file only; do not call DELETE (sandbox may keep running)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	path, err := resolveSessionPath(fs, loc)
	if err != nil {
		fmt.Fprintf(stderr, "session stop: %v\n", err)
		return 2
	}
	st, err := session.Load(path)
	if err != nil {
		if errors.Is(err, session.ErrNoSession) {
			fmt.Fprintf(stderr, "session stop: no active session (%s)\n", path)
			return 1
		}
		fmt.Fprintf(stderr, "session stop: %v\n", err)
		return 1
	}
	if *localOnly {
		if err := session.Clear(path); err != nil {
			fmt.Fprintf(stderr, "session stop: %v\n", err)
			return 1
		}
		fmt.Fprintf(stderr, "asp: cleared session file %s (sandbox %s NOT destroyed)\n", path, st.SandboxID)
		fmt.Fprintln(stdout, st.SandboxID)
		return 0
	}
	if !cpURLWasSet(fs) {
		g.cpURL = st.CPURL
	}
	c, code := mustClient(g, stderr)
	if c == nil {
		return code
	}
	if err := destroyRecorded(context.Background(), c, st, stderr); err != nil {
		fmt.Fprintf(stderr, "session stop: destroy %s: %v (state file kept: %s)\n", st.SandboxID, err, path)
		return 1
	}
	if err := session.Clear(path); err != nil {
		fmt.Fprintf(stderr, "session stop: clear %s: %v\n", path, err)
		return 1
	}
	fmt.Fprintf(stderr, "asp: session stopped %s\n", st.SandboxID)
	fmt.Fprintln(stdout, st.SandboxID)
	return 0
}

// destroyRecorded DELETEs the sandbox recorded in st. 404 is returned to the caller.
func destroyRecorded(ctx context.Context, c *client.Client, st session.State, stderr io.Writer) error {
	sb, err := c.DeleteSandbox(ctx, st.SandboxID)
	if err != nil {
		var he *client.HTTPError
		if errors.As(err, &he) && he.StatusCode == 404 {
			fmt.Fprintf(stderr, "asp: sandbox %s already gone\n", st.SandboxID)
			return nil
		}
		return err
	}
	fmt.Fprintf(stderr, "asp: destroyed sandbox %s state=%s\n", sb.ID, sb.State)
	return nil
}
