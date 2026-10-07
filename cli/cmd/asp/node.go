package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/client"
)

func nodeCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "node subcommand required (list|cordon|uncordon|doctor|enroll-token|fence)")
		return 2
	}
	switch args[0] {
	case "list", "ls":
		return cmdNodeList(args[1:], stdout, stderr)
	case "cordon":
		return cmdNodeCordon(args[1:], stdout, stderr, true)
	case "uncordon":
		return cmdNodeCordon(args[1:], stdout, stderr, false)
	case "doctor":
		return cmdNodeDoctor(args[1:], stdout, stderr)
	case "enroll-token":
		return cmdNodeEnrollToken(args[1:], stdout, stderr)
	case "fence":
		return cmdNodeFence(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		printRootUsage(stderr)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown node subcommand %q\n", args[0])
		return 2
	}
}

func cmdNodeList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("node list", flag.ContinueOnError)
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
	nodes, err := c.ListNodes(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "node list: %v\n", err)
		return 1
	}
	if g.jsonOut {
		return writeJSON(stdout, map[string]any{"nodes": nodes})
	}
	writeNodeTable(stdout, nodes, time.Now())
	return 0
}

// writeNodeTable prints used/offered per node; "-" means not enforced.
func writeNodeTable(w io.Writer, nodes []client.Node, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tSTATE\tSCHEDULABLE\tEGRESS\tCPU (cores)\tMEMORY (MiB)\tSANDBOXES\tSTOPPED (DISKS)\tDISK FREE\tLAST SEEN\tCERT EXPIRES")
	for _, n := range nodes {
		sched := "yes"
		if !n.Schedulable {
			sched = "no: " + n.UnschedulableReason
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			n.ID, n.State, sched, egressText(n.EgressEnforced),
			usedOf(float64(n.Allocated.CPUMillis)/1000, float64(n.Allocatable.CPUMillis)/1000, "%.1f"),
			usedOf(float64(n.Allocated.MemoryMiB), float64(n.Allocatable.MemoryMiB), "%.0f"),
			usedOf(float64(n.Allocated.Sandboxes), float64(n.Allocatable.Sandboxes), "%.0f"),
			n.StoppedSandboxes,
			diskText(n.DiskFreeMiB),
			sinceText(n.LastSeenAt, now),
			expiryText(n.CertNotAfter, now))
	}
	_ = tw.Flush()
}

// egressText says whether a node forces its guests through the egress proxy.
// "off" means a tenant's egress policy does not bind the guests on that node.
func egressText(enforced bool) string {
	if enforced {
		return "enforced"
	}
	return "off"
}

// diskText is the free disk space of a node in GiB, or "-" when it did not report it.
func diskText(mib *int64) string {
	if mib == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f GiB", float64(*mib)/1024)
}

func usedOf(used, offered float64, format string) string {
	if offered <= 0 {
		return fmt.Sprintf(format+"/-", used)
	}
	return fmt.Sprintf(format+"/"+format, used, offered)
}

// expiryText says when the node certificate expires: node agents renew it a
// third of its lifetime ahead, so a short time left means renewal is failing.
func expiryText(t *time.Time, now time.Time) string {
	switch {
	case t == nil:
		return "-"
	case !t.After(now):
		return "EXPIRED"
	}
	days := int(t.Sub(now).Hours() / 24)
	if days < 30 {
		return fmt.Sprintf("in %dd (renewal failing?)", days)
	}
	return fmt.Sprintf("in %dd", days)
}

func sinceText(t *time.Time, now time.Time) string {
	if t == nil {
		return "never"
	}
	return now.Sub(*t).Round(time.Second).String() + " ago"
}

func cmdNodeCordon(args []string, stdout, stderr io.Writer, cordon bool) int {
	name := "node uncordon"
	if cordon {
		name = "node cordon"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var g globalFlags
	addGlobalFlags(fs, &g)
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		fmt.Fprintf(stderr, "usage: asp %s <node-id>\n", name)
		return 2
	}
	c, code := mustClient(g, stderr)
	if c == nil {
		return code
	}
	n, err := c.SetNodeCordoned(context.Background(), pos[0], cordon)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}
	if g.jsonOut {
		return writeJSON(stdout, n)
	}
	if cordon {
		fmt.Fprintf(stdout, "node %s cordoned: no new sandboxes; %d running stay\n", n.ID, n.Allocated.Sandboxes)
	} else {
		fmt.Fprintf(stdout, "node %s uncordoned: schedulable=%v\n", n.ID, n.Schedulable)
	}
	return 0
}

// cmdNodeEnrollToken prints a single-use enroll token for a new server, or
// for re-keying an enrolled node (--node-id pins it).
func cmdNodeEnrollToken(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("node enroll-token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var g globalFlags
	addGlobalFlags(fs, &g)
	nodeID := fs.String("node-id", "", "pin the token to this node id (needed to re-key a node that is enrolled)")
	ttl := fs.Duration("ttl", time.Hour, "how long the token stays valid (at most 168h)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: asp node enroll-token [--node-id ID] [--ttl 1h] [--json]")
		return 2
	}
	c, code := mustClient(g, stderr)
	if c == nil {
		return code
	}
	tok, err := c.CreateEnrollToken(context.Background(), strings.TrimSpace(*nodeID), *ttl)
	if err != nil {
		fmt.Fprintf(stderr, "node enroll-token: %v\n", err)
		return 1
	}
	if g.jsonOut {
		return writeJSON(stdout, tok)
	}
	fmt.Fprintln(stdout, tok.Token)
	scope := "any new node id"
	idFlag := "--node-id=<id>"
	if tok.NodeID != "" {
		scope = "node " + tok.NodeID + " only"
		idFlag = "--node-id=" + tok.NodeID
	}
	fmt.Fprintf(stderr, "single use, for %s, valid until %s\nOn the server: node-agent --enroll --enroll-token=<token> %s ...  (or ASP_NODE_ENROLL_TOKEN)\n",
		scope, tok.ExpiresAt.Local().Format(time.RFC3339), idFlag)
	return 0
}

// explainCreateError turns placement refusals into something actionable.
func explainCreateError(err error) string {
	var he *client.HTTPError
	if errors.As(err, &he) {
		switch he.StatusCode {
		case http.StatusServiceUnavailable:
			return "no capacity: " + he.Message + " (retry later, or ask an admin to add or uncordon nodes: asp node list)"
		case http.StatusConflict:
			return "node pin rejected: " + he.Message + " (omit --node-id to let the scheduler pick, or see asp node list)"
		}
	}
	return err.Error()
}

func nodeOf(sb client.Sandbox) string {
	if sb.NodeID == nil || *sb.NodeID == "" {
		return "-"
	}
	return *sb.NodeID
}

// cmdNodeFence sets or clears how the control plane powers a node off when it
// declares it lost. Only an admin can, and the credential is never printed or
// returned: a reference (--token-env, --token-file) is resolved by the control
// plane when it fences, so the secret need not be stored at all.
func cmdNodeFence(args []string, stdout, stderr io.Writer) int {
	usage := func() int {
		fmt.Fprintln(stderr, `usage:
  asp node fence set <node-id> --endpoint URL [--token-env NAME | --token-file /abs/path | --token-stdin]
  asp node fence clear <node-id>

--token-env and --token-file name a variable or a file of the control plane's
host, which it reads when it fences. --token-stdin reads the credential from
standard input and stores it.`)
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "set":
		return cmdNodeFenceSet(args[1:], stdout, stderr)
	case "clear":
		return cmdNodeFenceClear(args[1:], stdout, stderr)
	}
	return usage()
}

func cmdNodeFenceSet(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("node fence set", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var g globalFlags
	addGlobalFlags(fs, &g)
	endpoint := fs.String("endpoint", "", "where the control plane asks for the power-off: webhook URL, Redfish base URL or IPMI host")
	tokenEnv := fs.String("token-env", "", "credential: the control plane's environment variable NAME")
	tokenFile := fs.String("token-file", "", "credential: an absolute path on the control plane's host")
	tokenStdin := fs.Bool("token-stdin", false, "read the credential from standard input")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 || strings.TrimSpace(*endpoint) == "" {
		fmt.Fprintln(stderr, "usage: asp node fence set <node-id> --endpoint URL [--token-env NAME | --token-file /abs/path | --token-stdin]")
		return 2
	}
	n := 0
	for _, set := range []bool{*tokenEnv != "", *tokenFile != "", *tokenStdin} {
		if set {
			n++
		}
	}
	if n > 1 {
		fmt.Fprintln(stderr, "node fence set: give at most one of --token-env, --token-file and --token-stdin")
		return 2
	}
	token := ""
	switch {
	case *tokenEnv != "":
		token = "env:" + strings.TrimSpace(*tokenEnv)
	case *tokenFile != "":
		token = "file:" + strings.TrimSpace(*tokenFile)
	case *tokenStdin:
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 8192))
		if err != nil {
			fmt.Fprintf(stderr, "node fence set: reading the credential: %v\n", err)
			return 1
		}
		token = strings.TrimSpace(string(b))
		if token == "" {
			fmt.Fprintln(stderr, "node fence set: empty credential on standard input")
			return 2
		}
	}
	c, code := mustClient(g, stderr)
	if c == nil {
		return code
	}
	if err := c.SetNodeFence(context.Background(), pos[0], strings.TrimSpace(*endpoint), token); err != nil {
		fmt.Fprintf(stderr, "node fence set: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "node %s: fence target set\n", pos[0])
	return 0
}

func cmdNodeFenceClear(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("node fence clear", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var g globalFlags
	addGlobalFlags(fs, &g)
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: asp node fence clear <node-id>")
		return 2
	}
	c, code := mustClient(g, stderr)
	if c == nil {
		return code
	}
	if err := c.ClearNodeFence(context.Background(), pos[0]); err != nil {
		fmt.Fprintf(stderr, "node fence clear: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "node %s: fence target cleared\n", pos[0])
	return 0
}

// cmdNodeDoctor is asp node doctor <id>: the node-agent of the node runs its
// self-checks and the control plane brings the report. It is the node judging itself
// with the settings it runs with. Exit 1 when a check failed.
func cmdNodeDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("node doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var g globalFlags
	addGlobalFlags(fs, &g)
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "usage: asp node doctor <node-id> [--json]")
		return 2
	}
	c, code := mustClient(g, stderr)
	if c == nil {
		return code
	}
	// The node runs a dozen probes: more than the default wait.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rep, err := c.NodeDoctor(ctx, rest[0])
	if err != nil {
		fmt.Fprintf(stderr, "node doctor: %v\n", err)
		return 1
	}
	if g.jsonOut {
		if c := writeJSON(stdout, rep); c != 0 {
			return c
		}
	} else {
		fmt.Fprint(stdout, doctorText(rep))
	}
	if rep.Failed() {
		return 1
	}
	return 0
}

// doctorText renders a report for a terminal: one line per check, the fix under it.
func doctorText(r client.DoctorReport) string {
	var b strings.Builder
	title := "node doctor"
	if r.NodeID != "" {
		title += " (" + r.NodeID + ")"
	}
	fmt.Fprintln(&b, title)
	width := 0
	counts := map[string]int{}
	for _, c := range r.Results {
		width = max(width, len(c.Name))
		counts[c.Status]++
	}
	for _, c := range r.Results {
		fmt.Fprintf(&b, "  %-4s  %-*s  %s\n", c.Status, width, c.Name, c.Detail)
		if c.Fix != "" && c.Status != "ok" {
			fmt.Fprintf(&b, "        %-*s  fix: %s\n", width, "", c.Fix)
		}
	}
	fmt.Fprintf(&b, "%d ok, %d warn, %d fail, %d skipped\n", counts["ok"], counts["warn"], counts["fail"], counts["skip"])
	return b.String()
}
