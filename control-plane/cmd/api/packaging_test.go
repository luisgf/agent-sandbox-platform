package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// variablesInSource lists every "ASP_..." string literal in the control plane's code:
// the variables it reads are named there (some through a constant).
func variablesInSource(t *testing.T) map[string]bool {
	t.Helper()
	re := regexp.MustCompile(`"(ASP_[A-Z0-9_]+)"`)
	seen := map[string]bool{}
	err := filepath.WalkDir(filepath.Join("..", ".."), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == testSupportDir {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
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
	return seen
}

// What the packaged unit and the example env file set must be variables the control
// plane reads: one that was renamed, or never existed, does nothing, and nothing says so.
func TestPackagedControlPlaneFilesNameRealVariables(t *testing.T) {
	known := variablesInSource(t)
	if len(known) < 40 {
		t.Fatalf("only %d variables found in the source: is the scan reading it?", len(known))
	}
	name := regexp.MustCompile(`^\s*#?\s*(ASP_[A-Z0-9_]+)=`)
	envLine := regexp.MustCompile(`^\s*Environment=(ASP_[A-Z0-9_]+)=`)
	for _, f := range []struct{ path, kind string }{
		{"packaging/etc/control-plane.env", "env"},
		{"packaging/systemd/asp-control-plane.service", "unit"},
	} {
		b, err := os.ReadFile(filepath.Join("..", "..", "..", f.path))
		if err != nil {
			t.Skipf("%s is not here (a checkout of the control plane alone): %v", f.path, err)
		}
		n := 0
		for _, line := range strings.Split(string(b), "\n") {
			re := name
			if f.kind == "unit" {
				re = envLine
			}
			if m := re.FindStringSubmatch(line); m != nil {
				n++
				if !known[m[1]] {
					t.Errorf("%s sets %s, which the control plane does not read", f.path, m[1])
				}
			}
		}
		if n == 0 {
			t.Errorf("%s: no variable found: is the file read right?", f.path)
		}
	}
}
