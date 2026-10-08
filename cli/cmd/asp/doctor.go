package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// cmdDoctor is asp doctor: on a node, as root, check the host and the configuration
// of the node-agent without starting it. The checks are the node-agent's own (they
// need what only it knows), so this runs `node-agent --doctor` with the settings of
// the node: the node-agent reads /etc/asp/agent.yaml (and its drop-ins) itself, as it
// does under systemd, then come the variables of --env-file (for a unit that loads an
// EnvironmentFile) and the flags given here. It is the tool
// for a node that does not start; for one that runs, `asp node doctor <id>` asks the
// running agent, which is exact about its own settings.
func cmdDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	envFile := fs.String("env-file", "", "KEY=VALUE settings for the node-agent, for a unit that loads an EnvironmentFile (the node-agent reads /etc/asp/agent.yaml itself)")
	bin := fs.String("node-agent", "", "the node-agent binary (default: node-agent on PATH, then /usr/local/bin/node-agent)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: asp doctor [--env-file FILE] [--node-agent PATH] [--json] [-- node-agent flags...]")
		fmt.Fprintln(stderr, "Runs the node-agent's host checks with this node's settings: the node-agent reads /etc/asp/agent.yaml")
		fmt.Fprintln(stderr, "(and agent.yaml.d/) itself. Flags after -- go to the node-agent (--config FILE, --disk-dir, ...).")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	path, err := findNodeAgent(*bin)
	if err != nil {
		fmt.Fprintf(stderr, "doctor: %v\n", err)
		return 2
	}
	var env []string
	if *envFile != "" {
		if env, err = readEnvFile(*envFile); err != nil {
			fmt.Fprintf(stderr, "doctor: %v\n", err)
			return 2
		}
	}
	cmdArgs := append([]string{"--doctor"}, fs.Args()...)
	if *asJSON {
		cmdArgs = append(cmdArgs, "--doctor-json")
	}
	cmd := exec.Command(path, cmdArgs...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		fmt.Fprintf(stderr, "doctor: %v\n", err)
		return 1
	}
	return 0
}

// The places the node-agent is installed: the package puts it in /usr/bin as
// asp-node-agent; a build copied by hand (docs/how-to/install-node.md) is node-agent in
// /usr/local/bin.
var (
	installedNodeAgent = "/usr/local/bin/node-agent"
	packagedNodeAgent  = "/usr/bin/asp-node-agent"
)

// findNodeAgent is the node-agent to run: the one named, else on PATH, else where the
// packages put it.
func findNodeAgent(named string) (string, error) {
	if named != "" {
		return named, nil
	}
	for _, name := range []string{"asp-node-agent", "node-agent"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	for _, p := range []string{packagedNodeAgent, installedNodeAgent} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("node-agent not found: it runs the checks. Install it, or name it with --node-agent")
}

// readEnvFile reads the KEY=VALUE lines of a systemd EnvironmentFile: comments (# and
// ;) and blank lines are skipped, a value may be single or double quoted, and a
// missing file is the caller's to ignore.
func readEnvFile(path string) ([]string, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out = append(out, k+"="+v)
	}
	return out, sc.Err()
}
