package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const repoRoot = "../../../"

// variablesReadInSource lists the ASP_ variables the node-agent's code reads by name
// (os.Getenv and friends), which are not all settings.
func variablesReadInSource(t *testing.T) []string {
	t.Helper()
	re := regexp.MustCompile(`(?:Getenv|LookupEnv)\("(ASP_[A-Z0-9_]+)"\)`)
	seen := map[string]bool{}
	err := filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			seen[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for name := range seen {
		out = append(out, name)
	}
	return out
}

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(repoRoot + path)
	if err != nil {
		t.Skipf("%s is not here (a checkout of the node-agent alone): %v", path, err)
	}
	return string(b)
}

// unitLines are the lines of a unit that do something: no comments, no blank lines.
func unitLines(src string) []string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// The package installs the node-agent as asp-node-agent in /usr/bin and the lab host
// has it as node-agent in /usr/local/bin; apart from that, the units are one.
func TestPackagedUnitIsTheLabUnit(t *testing.T) {
	lab := unitLines(strings.ReplaceAll(readRepoFile(t, "scripts/systemd/asp-node-agent.service"), "/usr/local/bin/node-agent", "/usr/bin/asp-node-agent"))
	pkg := unitLines(readRepoFile(t, "packaging/systemd/asp-node-agent.service"))
	if len(lab) != len(pkg) {
		t.Fatalf("the packaged unit has %d active lines, the lab unit %d: change both", len(pkg), len(lab))
	}
	for i := range lab {
		if lab[i] != pkg[i] {
			t.Errorf("line %d differs:\n  lab:      %s\n  packaged: %s", i+1, lab[i], pkg[i])
		}
	}
}

// What the unit and the example env file set must be settings the node-agent has: a
// variable that was renamed, or never existed, does nothing, and nothing says so.
func TestPackagedNodeAgentFilesNameRealSettings(t *testing.T) {
	known := map[string]bool{}
	for _, st := range declared(t) {
		if st.Env != "" {
			known[st.Env] = true
		}
	}
	// And the ones it reads without declaring them as settings.
	for _, name := range variablesReadInSource(t) {
		known[name] = true
	}
	name := regexp.MustCompile(`^\s*#?\s*(ASP_[A-Z0-9_]+)=`)
	envLine := regexp.MustCompile(`^\s*Environment=(ASP_[A-Z0-9_]+)=`)
	for _, f := range []struct{ path, kind string }{
		{"packaging/etc/node-agent.env", "env"},
		{"packaging/systemd/asp-node-agent.service", "unit"},
	} {
		n := 0
		for _, line := range strings.Split(readRepoFile(t, f.path), "\n") {
			re := name
			if f.kind == "unit" {
				re = envLine
			}
			m := re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			n++
			if !known[m[1]] {
				t.Errorf("%s sets %s, which is not a setting of the node-agent", f.path, m[1])
			}
		}
		if n == 0 {
			t.Errorf("%s: no variable found: is the file read right?", f.path)
		}
	}
}
