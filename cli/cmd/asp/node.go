package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"text/tabwriter"
	"time"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/client"
)

func nodeCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "node subcommand required (list|cordon|uncordon)")
		return 2
	}
	switch args[0] {
	case "list", "ls":
		return cmdNodeList(args[1:], stdout, stderr)
	case "cordon":
		return cmdNodeCordon(args[1:], stdout, stderr, true)
	case "uncordon":
		return cmdNodeCordon(args[1:], stdout, stderr, false)
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
	fmt.Fprintln(tw, "NODE\tSTATE\tSCHEDULABLE\tCPU (cores)\tMEMORY (MiB)\tSANDBOXES\tLAST SEEN")
	for _, n := range nodes {
		sched := "yes"
		if !n.Schedulable {
			sched = "no: " + n.UnschedulableReason
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			n.ID, n.State, sched,
			usedOf(float64(n.Allocated.CPUMillis)/1000, float64(n.Allocatable.CPUMillis)/1000, "%.1f"),
			usedOf(float64(n.Allocated.MemoryMiB), float64(n.Allocatable.MemoryMiB), "%.0f"),
			usedOf(float64(n.Allocated.Sandboxes), float64(n.Allocatable.Sandboxes), "%.0f"),
			sinceText(n.LastSeenAt, now))
	}
	_ = tw.Flush()
}

func usedOf(used, offered float64, format string) string {
	if offered <= 0 {
		return fmt.Sprintf(format+"/-", used)
	}
	return fmt.Sprintf(format+"/"+format, used, offered)
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
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "usage: asp %s <node-id>\n", name)
		return 2
	}
	c, code := mustClient(g, stderr)
	if c == nil {
		return code
	}
	n, err := c.SetNodeCordoned(context.Background(), fs.Arg(0), cordon)
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
