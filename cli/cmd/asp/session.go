package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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

  asp session start [flags]          create, wait until running, save ~/.cache/asp/session.json
  asp session exec (--cmd '…' | -- argv…)
  asp session status [--json]
  asp session stop                   destroy sandbox and clear the state file

One local session file (override --session-file / ASP_SESSION_FILE). Not an OpenCode plugin.
See docs/ops-asp-session.md.`)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown session subcommand %q\n", args[0])
		return 2
	}
}

func addSessionFileFlag(fs *flag.FlagSet, dest *string) {
	def := session.DefaultPath()
	fs.StringVar(dest, "session-file", def, "session state path (env ASP_SESSION_FILE)")
}

func resolveSessionPath(flagPath string) (string, error) {
	p := strings.TrimSpace(flagPath)
	if p == "" {
		p = session.DefaultPath()
	}
	if p == "" {
		return "", fmt.Errorf("cannot resolve session path (set --session-file or ASP_SESSION_FILE)")
	}
	return p, nil
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
	var sessionPath string
	addSessionFileFlag(fs, &sessionPath)
	image := fs.String("image", "debian:bookworm-slim", "image_ref")
	cpu := fs.Int("cpu-millis", 1000, "cpu_millis")
	mem := fs.Int("memory-mib", 512, "memory_mib")
	node := fs.String("node-id", "", "optional node pin")
	vmm := fs.String("vmm-profile", "cloud-hypervisor", "vmm_profile")
	force := fs.Bool("force", false, "destroy any sandbox recorded in the session file, then start a new one")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	path, err := resolveSessionPath(sessionPath)
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
		TenantID:   g.tenant,
		ImageRef:   *image,
		CPUMillis:  *cpu,
		MemoryMiB:  *mem,
		VMMProfile: *vmm,
		NodeID:     *node,
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
		SandboxID: sb.ID,
		CPURL:     c.BaseURL,
		TenantID:  sb.TenantID,
		ImageRef:  sb.ImageRef,
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
	var sessionPath string
	addSessionFileFlag(fs, &sessionPath)
	cmdFlag := fs.String("cmd", "", "command string (quoted words)")
	cwd := fs.String("cwd", "", "working directory in guest")
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
	path, err := resolveSessionPath(sessionPath)
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
	res, err := c.Exec(context.Background(), st.SandboxID, client.ExecRequest{Cmd: argv, Cwd: *cwd})
	if err != nil {
		fmt.Fprintf(stderr, "session exec: sandbox %s: %v\n", st.SandboxID, err)
		return 1
	}
	if code := writeExec(stdout, stderr, res, g.jsonOut); code != 0 {
		return code
	}
	return res.ExitCode
}

func cmdSessionStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("session status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var g globalFlags
	addGlobalFlags(fs, &g)
	var sessionPath string
	addSessionFileFlag(fs, &sessionPath)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	path, err := resolveSessionPath(sessionPath)
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
		SessionFile string          `json:"session_file"`
		SandboxID   string          `json:"sandbox_id"`
		CPURL       string          `json:"cp_url"`
		TenantID    string          `json:"tenant_id,omitempty"`
		ImageRef    string          `json:"image_ref,omitempty"`
		CreatedAt   string          `json:"created_at,omitempty"`
		Live        *client.Sandbox `json:"live,omitempty"`
		LiveError   string          `json:"live_error,omitempty"`
	}
	out := view{
		SessionFile: path,
		SandboxID:   st.SandboxID,
		CPURL:       st.CPURL,
		TenantID:    st.TenantID,
		ImageRef:    st.ImageRef,
	}
	if !st.CreatedAt.IsZero() {
		out.CreatedAt = st.CreatedAt.UTC().Format(time.RFC3339)
	}
	if err != nil {
		out.LiveError = err.Error()
		if g.jsonOut {
			_ = writeJSON(stdout, out)
		} else {
			fmt.Fprintf(stdout, "id=%s file=%s cp=%s live_error=%s\n", st.SandboxID, path, st.CPURL, err.Error())
		}
		fmt.Fprintf(stderr, "session status: get %s: %v\n", st.SandboxID, err)
		return 1
	}
	out.Live = &sb
	if g.jsonOut {
		return writeJSON(stdout, out)
	}
	fmt.Fprintf(stdout, "id=%s state=%s tenant=%s cp=%s file=%s\n", sb.ID, sb.State, sb.TenantID, st.CPURL, path)
	return 0
}

func cmdSessionStop(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("session stop", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var g globalFlags
	addGlobalFlags(fs, &g)
	var sessionPath string
	addSessionFileFlag(fs, &sessionPath)
	localOnly := fs.Bool("local", false, "clear the state file only; do not call DELETE (sandbox may keep running)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	path, err := resolveSessionPath(sessionPath)
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
