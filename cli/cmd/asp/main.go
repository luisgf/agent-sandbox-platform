// Command asp is a demo CLI for the Agent Sandbox Platform control-plane API.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/auth"
	"github.com/luisgf/agent-sandbox-platform/cli/internal/client"
	"github.com/luisgf/agent-sandbox-platform/cli/internal/cmdline"
	"github.com/luisgf/agent-sandbox-platform/cli/internal/wait"
)

const version = "0.1.0"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printRootUsage(stderr)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Fprintf(stdout, "asp %s\n", version)
		return 0
	}
	switch args[0] {
	case "sandbox":
		return sandboxCmd(args[1:], stdout, stderr)
	case "session":
		return sessionCmd(args[1:], stdout, stderr)
	case "auth":
		return authCmd(args[1:], stdout, stderr)
	case "node":
		return nodeCmd(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		printRootUsage(stderr)
		return 2
	}
}

func printRootUsage(w io.Writer) {
	fmt.Fprintf(w, `asp — Agent Sandbox Platform demo CLI

Usage:
  asp sandbox create [flags]
  asp sandbox get <id>
  asp sandbox list [--tenant]
  asp sandbox exec <id> (--cmd '…' | -- argv…)
  asp sandbox delete <id>
  asp sandbox run (--cmd '…' | -- argv…) [flags]
  asp session start|exec|status|stop|local-net [--name] [--local-net] [flags]
  asp auth login|logout|status [flags]
  asp node list [--json]
  asp node cordon|uncordon <id>
  asp version

Global env:
  ASP_CP_URL              control-plane base URL (default http://127.0.0.1:8080)
  ASP_SESSION_DIR         named sessions dir (default ~/.cache/asp/sessions, mode 0700)
  ASP_SESSION_FILE        optional single-file override (ignores --name)
  ASP_API_KEY             Bearer API key (also --api-key) — lab without IdP
  ASP_ID_TOKEN            IdP access token (also --id-token); preferred Bearer
  ASP_IDP_REQUIRED        if 1/true, require IdP token (auto-fetch when possible)
  ASP_IDP_TOKEN_URL       OIDC token endpoint (or derive from ASP_IDP_ISSUER)
  ASP_IDP_SECRETS_FILE    KEY=VALUE secrets (default ~/.secrets/asp-keycloak-lab.txt)
  ASP_IDP_CLIENT_ID/_SECRET / ASP_IDP_USERNAME/_PASSWORD / ASP_IDP_GRANT_TYPE

Agent one-liner (lab IdP on ncc1701d — see docs/ops-asp-agent-runner.md):
  export ASP_CP_URL=http://127.0.0.1:18112 ASP_IDP_REQUIRED=1
  asp sandbox run --tenant=default --cmd 'echo hello'

Reusable shell session (OpenCode bash tool — see docs/ops-asp-session.md):
  asp session start --tenant=default
  asp session exec --cmd 'echo hello'
  asp session stop

Demo (local dry-run stack — docs/mvp-smoke.md):
  asp sandbox run --node-id=dev-node --cmd 'echo hello'
`)
}

type globalFlags struct {
	cpURL   string
	apiKey  string
	idToken string
	tenant  string
	timeout time.Duration
	jsonOut bool
}

func addGlobalFlags(fs *flag.FlagSet, g *globalFlags) {
	defURL := envOr("ASP_CP_URL", "http://127.0.0.1:8080")
	defKey := os.Getenv("ASP_API_KEY")
	defID := auth.EnvIDToken()
	fs.StringVar(&g.cpURL, "cp-url", defURL, "control-plane base URL")
	fs.StringVar(&g.apiKey, "api-key", defKey, "Bearer API key (env ASP_API_KEY)")
	fs.StringVar(&g.idToken, "id-token", defID, "IdP access token (env ASP_ID_TOKEN)")
	fs.StringVar(&g.tenant, "tenant", "tenant-demo", "tenant_id for create/list/run")
	fs.DurationVar(&g.timeout, "timeout", 60*time.Second, "wait timeout for running state")
	fs.BoolVar(&g.jsonOut, "json", false, "print raw JSON to stdout")
}

func newClient(g globalFlags) (*client.Client, error) {
	c := client.New(g.cpURL, g.apiKey)
	res, err := auth.ResolveBearer(context.Background(), auth.ResolveInput{
		ExplicitToken: g.idToken,
		APIKey:        g.apiKey,
	})
	if err != nil {
		return nil, err
	}
	switch res.Source {
	case "id-token", "cache", "fetch":
		c.SetBearer(res.Bearer)
		c.APIKey = "" // avoid dual semantics; Bearer is the IdP token
	case "api-key":
		// New() already set APIKey
	}
	return c, nil
}

func mustClient(g globalFlags, stderr io.Writer) (*client.Client, int) {
	c, err := newClient(g)
	if err != nil {
		fmt.Fprintf(stderr, "auth: %v\n", err)
		return nil, 1
	}
	return c, 0
}

func sandboxCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "sandbox subcommand required (create|get|list|exec|delete|run)")
		return 2
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "create":
		return cmdCreate(rest, stdout, stderr)
	case "get":
		return cmdGet(rest, stdout, stderr)
	case "list":
		return cmdList(rest, stdout, stderr)
	case "exec":
		return cmdExec(rest, stdout, stderr)
	case "delete", "destroy", "rm":
		return cmdDelete(rest, stdout, stderr)
	case "run":
		return cmdRun(rest, stdout, stderr)
	case "-h", "--help", "help":
		printRootUsage(stderr)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown sandbox subcommand %q\n", sub)
		return 2
	}
}

func cmdCreate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sandbox create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var g globalFlags
	addGlobalFlags(fs, &g)
	image := fs.String("image", "debian:bookworm-slim", "image_ref")
	cpu := fs.Int("cpu-millis", 1000, "cpu_millis")
	mem := fs.Int("memory-mib", 512, "memory_mib")
	node := fs.String("node-id", "", "pin to this node (default: the scheduler picks one with room)")
	vmm := fs.String("vmm-profile", "cloud-hypervisor", "vmm_profile")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c, code := mustClient(g, stderr)
	if c == nil {
		return code
	}
	ctx := context.Background()
	sb, err := c.CreateSandbox(ctx, client.CreateInput{
		TenantID:   g.tenant,
		ImageRef:   *image,
		CPUMillis:  *cpu,
		MemoryMiB:  *mem,
		VMMProfile: *vmm,
		NodeID:     *node,
	})
	if err != nil {
		fmt.Fprintf(stderr, "create: %s\n", explainCreateError(err))
		return 1
	}
	fmt.Fprintf(stderr, "asp: created sandbox %s state=%s\n", sb.ID, sb.State)
	return writeSandbox(stdout, sb, g.jsonOut)
}

func cmdGet(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sandbox get", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var g globalFlags
	addGlobalFlags(fs, &g)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	pos := fs.Args()
	if len(pos) < 1 {
		fmt.Fprintln(stderr, "usage: asp sandbox get <id>")
		return 2
	}
	c, code := mustClient(g, stderr)
	if c == nil {
		return code
	}
	sb, err := c.GetSandbox(context.Background(), pos[0])
	if err != nil {
		fmt.Fprintf(stderr, "get: %v\n", err)
		return 1
	}
	return writeSandbox(stdout, sb, g.jsonOut)
}

func cmdList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sandbox list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var g globalFlags
	addGlobalFlags(fs, &g)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c, code := mustClient(g, stderr)
	if c == nil {
		return code
	}
	list, err := c.ListSandboxes(context.Background(), g.tenant)
	if err != nil {
		fmt.Fprintf(stderr, "list: %v\n", err)
		return 1
	}
	if g.jsonOut {
		return writeJSON(stdout, map[string]any{"sandboxes": list})
	}
	for _, sb := range list {
		node := ""
		if sb.NodeID != nil {
			node = *sb.NodeID
		}
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", sb.ID, sb.State, sb.TenantID, node)
	}
	return 0
}

func cmdDelete(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sandbox delete", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var g globalFlags
	addGlobalFlags(fs, &g)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	pos := fs.Args()
	if len(pos) < 1 {
		fmt.Fprintln(stderr, "usage: asp sandbox delete <id>")
		return 2
	}
	c, code := mustClient(g, stderr)
	if c == nil {
		return code
	}
	sb, err := c.DeleteSandbox(context.Background(), pos[0])
	if err != nil {
		fmt.Fprintf(stderr, "delete: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "asp: deleted sandbox %s state=%s\n", sb.ID, sb.State)
	return writeSandbox(stdout, sb, g.jsonOut)
}

func cmdExec(args []string, stdout, stderr io.Writer) int {
	before, after := cmdline.SplitDashDash(args)
	fs := flag.NewFlagSet("sandbox exec", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var g globalFlags
	addGlobalFlags(fs, &g)
	cmdFlag := fs.String("cmd", "", "command string (quoted words)")
	cwd := fs.String("cwd", "", "working directory in guest")
	if err := fs.Parse(before); err != nil {
		return 2
	}
	pos := fs.Args()
	if len(pos) < 1 {
		fmt.Fprintln(stderr, "usage: asp sandbox exec <id> --cmd '…' | asp sandbox exec <id> -- argv…")
		return 2
	}
	id := pos[0]
	// Remaining positional after id can also be command if no --cmd/--
	trail := after
	if len(trail) == 0 && len(pos) > 1 {
		trail = pos[1:]
	}
	argv, err := cmdline.FromFlagAndArgs(*cmdFlag, trail)
	if err != nil {
		fmt.Fprintf(stderr, "exec: %v\n", err)
		return 2
	}
	c, code := mustClient(g, stderr)
	if c == nil {
		return code
	}
	res, err := c.Exec(context.Background(), id, client.ExecRequest{Cmd: argv, Cwd: *cwd})
	if err != nil {
		fmt.Fprintf(stderr, "exec: %v\n", err)
		return 1
	}
	return writeExec(stdout, stderr, res, g.jsonOut)
}

func cmdRun(args []string, stdout, stderr io.Writer) int {
	before, after := cmdline.SplitDashDash(args)
	fs := flag.NewFlagSet("sandbox run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var g globalFlags
	addGlobalFlags(fs, &g)
	cmdFlag := fs.String("cmd", "", "command string (quoted words)")
	cwd := fs.String("cwd", "", "working directory in guest")
	keep := fs.Bool("keep", false, "do not destroy sandbox on exit")
	image := fs.String("image", "debian:bookworm-slim", "image_ref")
	cpu := fs.Int("cpu-millis", 1000, "cpu_millis")
	mem := fs.Int("memory-mib", 512, "memory_mib")
	node := fs.String("node-id", "", "pin to this node (default: the scheduler picks one with room)")
	vmm := fs.String("vmm-profile", "cloud-hypervisor", "vmm_profile")
	if err := fs.Parse(before); err != nil {
		return 2
	}
	// leftover positionals without -- become command if --cmd empty
	trail := after
	if len(trail) == 0 && len(fs.Args()) > 0 {
		trail = fs.Args()
	}
	argv, err := cmdline.FromFlagAndArgs(*cmdFlag, trail)
	if err != nil {
		fmt.Fprintf(stderr, "run: %v\n", err)
		return 2
	}

	c, code := mustClient(g, stderr)
	if c == nil {
		return code
	}
	ctx := context.Background()
	sb, err := c.CreateSandbox(ctx, client.CreateInput{
		TenantID:   g.tenant,
		ImageRef:   *image,
		CPUMillis:  *cpu,
		MemoryMiB:  *mem,
		VMMProfile: *vmm,
		NodeID:     *node,
	})
	if err != nil {
		fmt.Fprintf(stderr, "run create: %s\n", explainCreateError(err))
		return 1
	}
	fmt.Fprintf(stderr, "asp: created sandbox %s state=%s\n", sb.ID, sb.State)

	destroy := !*keep
	defer func() {
		if !destroy {
			fmt.Fprintf(stderr, "asp: keeping sandbox %s (--keep)\n", sb.ID)
			return
		}
		dsb, err := c.DeleteSandbox(context.Background(), sb.ID)
		if err != nil {
			fmt.Fprintf(stderr, "asp: destroy %s: %v\n", sb.ID, err)
			return
		}
		fmt.Fprintf(stderr, "asp: destroyed sandbox %s state=%s\n", dsb.ID, dsb.State)
	}()

	if sb.State != "running" {
		waitCtx, cancel := context.WithTimeout(ctx, g.timeout)
		defer cancel()
		sb, err = wait.WaitForState(waitCtx, c.GetSandbox, sb.ID, "running", wait.Options{
			Timeout: g.timeout,
			Log:     stderr,
		})
		if err != nil {
			fmt.Fprintf(stderr, "run wait: %v\n", err)
			return 1
		}
	}

	res, err := c.Exec(ctx, sb.ID, client.ExecRequest{Cmd: argv, Cwd: *cwd})
	if err != nil {
		fmt.Fprintf(stderr, "run exec: %v\n", err)
		return 1
	}
	if code := writeExec(stdout, stderr, res, g.jsonOut); code != 0 {
		return code
	}
	return res.ExitCode
}

func writeExec(stdout, stderr io.Writer, res client.ExecResult, asJSON bool) int {
	if asJSON {
		return writeJSON(stdout, res)
	}
	if res.Stdout != "" {
		fmt.Fprint(stdout, res.Stdout)
		if !strings.HasSuffix(res.Stdout, "\n") {
			fmt.Fprintln(stdout)
		}
	}
	if res.Stderr != "" {
		fmt.Fprint(stderr, res.Stderr)
		if !strings.HasSuffix(res.Stderr, "\n") {
			fmt.Fprintln(stderr)
		}
	}
	return 0
}

func writeSandbox(w io.Writer, sb client.Sandbox, asJSON bool) int {
	if asJSON {
		return writeJSON(w, sb)
	}
	b, _ := json.MarshalIndent(sb, "", "  ")
	fmt.Fprintln(w, string(b))
	return 0
}

func writeJSON(w io.Writer, v any) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return 1
	}
	return 0
}

func authCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "auth subcommand required (login|logout|status)")
		return 2
	}
	switch args[0] {
	case "login":
		return cmdAuthLogin(args[1:], stdout, stderr)
	case "logout":
		return cmdAuthLogout(args[1:], stdout, stderr)
	case "status":
		return cmdAuthStatus(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprintln(stderr, `asp auth — IdP token for control-plane

  asp auth login [--print-env|--print-token] [--grant password|client_credentials]
  asp auth logout
  asp auth status

Env: ASP_IDP_TOKEN_URL / ASP_IDP_ISSUER, ASP_IDP_SECRETS_FILE, ASP_IDP_GRANT_TYPE,
     ASP_IDP_CLIENT_ID, ASP_IDP_CLIENT_SECRET, ASP_IDP_USERNAME, ASP_IDP_PASSWORD`)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown auth subcommand %q\n", args[0])
		return 2
	}
}

func cmdAuthLogin(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	printEnv := fs.Bool("print-env", false, "print export ASP_ID_TOKEN=… to stdout")
	printTok := fs.Bool("print-token", false, "print access_token only to stdout")
	grant := fs.String("grant", "", "password|client_credentials (default: auto)")
	secrets := fs.String("secrets", auth.DefaultSecretsPath(), "credentials KEY=VALUE file")
	cache := fs.String("cache", auth.DefaultCachePath(), "token cache path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	res, err := auth.ResolveBearer(context.Background(), auth.ResolveInput{
		SecretsPath: *secrets,
		CachePath:   *cache,
		GrantType:   *grant,
		ForceFetch:  true,
		Required:    true,
	})
	if err != nil {
		fmt.Fprintf(stderr, "auth login: %v\n", err)
		return 1
	}
	if res.Bearer == "" {
		fmt.Fprintln(stderr, "auth login: empty token")
		return 1
	}
	fmt.Fprintf(stderr, "asp: logged in (source=%s", res.Source)
	if !res.Token.ExpiresAt().IsZero() {
		fmt.Fprintf(stderr, " expires=%s", res.Token.ExpiresAt().Format(time.RFC3339))
	}
	fmt.Fprintln(stderr, ")")
	if *printEnv {
		fmt.Fprintf(stdout, "export ASP_ID_TOKEN=%s\n", shellSingleQuote(res.Bearer))
		return 0
	}
	if *printTok {
		fmt.Fprintln(stdout, res.Bearer)
		return 0
	}
	return 0
}

func cmdAuthLogout(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth logout", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cache := fs.String("cache", auth.DefaultCachePath(), "token cache path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := auth.ClearCache(*cache); err != nil {
		fmt.Fprintf(stderr, "auth logout: %v\n", err)
		return 1
	}
	fmt.Fprintln(stderr, "asp: logged out (cache cleared)")
	return 0
}

func cmdAuthStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cache := fs.String("cache", auth.DefaultCachePath(), "token cache path")
	jsonOut := fs.Bool("json", false, "JSON status")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	type st struct {
		HasEnvToken bool   `json:"has_env_token"`
		CacheValid  bool   `json:"cache_valid"`
		CachePath   string `json:"cache_path"`
		ExpiresAt   string `json:"expires_at,omitempty"`
		CanFetch    bool   `json:"can_fetch"`
		IDPRequired bool   `json:"idp_required"`
	}
	out := st{
		HasEnvToken: auth.EnvIDToken() != "",
		CachePath:   *cache,
		CanFetch:    auth.CanAutoFetch(),
		IDPRequired: auth.IDPRequired(),
	}
	if tok, err := auth.LoadCache(*cache); err == nil {
		out.CacheValid = tok.Valid(auth.DefaultSkew())
		if exp := tok.ExpiresAt(); !exp.IsZero() {
			out.ExpiresAt = exp.Format(time.RFC3339)
		}
	}
	if *jsonOut {
		return writeJSON(stdout, out)
	}
	fmt.Fprintf(stdout, "env_token=%v cache_valid=%v can_fetch=%v idp_required=%v",
		out.HasEnvToken, out.CacheValid, out.CanFetch, out.IDPRequired)
	if out.ExpiresAt != "" {
		fmt.Fprintf(stdout, " expires=%s", out.ExpiresAt)
	}
	fmt.Fprintln(stdout)
	return 0
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
