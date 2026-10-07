package main

import (
	"encoding/json"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// listServer answers sandbox list and says how many times it was asked.
func listServer(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	n := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		n++
		_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []any{}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &n
}

func clearAuthEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"ASP_REQUIRE_TOKEN", "ASP_IDP_REQUIRED", "ASP_ID_TOKEN", "ASP_API_KEY", "ASP_CONTROL_PLANE_URL", "ASP_CP_URL"} {
		t.Setenv(k, "")
	}
}

// The control plane is found by the name the node-agent and the control plane use. The
// old name, in the environment and as a flag, still works and says it is deprecated.
func TestControlPlaneURLHasOneNameAndTheOldOnesWarn(t *testing.T) {
	srv, hits := listServer(t)
	for _, c := range []struct {
		name     string
		env      map[string]string
		args     []string
		warnWant string
	}{
		{"the new flag", nil, []string{"--control-plane-url", srv.URL}, ""},
		{"the new variable", map[string]string{"ASP_CONTROL_PLANE_URL": srv.URL}, nil, ""},
		{"the old flag", nil, []string{"--cp-url", srv.URL}, "--cp-url is deprecated: use --control-plane-url"},
		{"the old variable", map[string]string{"ASP_CP_URL": srv.URL}, nil, "ASP_CP_URL is deprecated: use ASP_CONTROL_PLANE_URL"},
		{"the flag beats both variables", map[string]string{"ASP_CONTROL_PLANE_URL": "http://127.0.0.1:1", "ASP_CP_URL": "http://127.0.0.1:2"},
			[]string{"--control-plane-url", srv.URL}, ""},
	} {
		clearAuthEnv(t)
		for k, v := range c.env {
			t.Setenv(k, v)
		}
		before := *hits
		var stdout, stderr strings.Builder
		code := run(append([]string{"sandbox", "list", "--tenant", "default"}, c.args...), &stdout, &stderr)
		if code != 0 || *hits != before+1 {
			t.Errorf("%s: exit %d, served %d times, stderr %q", c.name, code, *hits-before, stderr.String())
			continue
		}
		if c.warnWant == "" && strings.Contains(stderr.String(), "deprecated") {
			t.Errorf("%s: a warning for a name that is not deprecated: %q", c.name, stderr.String())
		}
		if c.warnWant != "" && !strings.Contains(stderr.String(), "asp: warning: "+c.warnWant) {
			t.Errorf("%s: no warning %q in %q", c.name, c.warnWant, stderr.String())
		}
	}
}

// A session knows its control plane: --force acts there unless the URL is given.
func TestEitherNameOfTheFlagCountsAsGiven(t *testing.T) {
	for _, name := range []string{"control-plane-url", "cp-url"} {
		var g globalFlags
		fs := newTestFlagSet()
		addGlobalFlags(fs, &g)
		if cpURLWasSet(fs) {
			t.Fatal("set before parsing")
		}
		if err := fs.Parse([]string{"--" + name, "http://x"}); err != nil {
			t.Fatal(err)
		}
		if !cpURLWasSet(fs) || g.cpURL != "http://x" {
			t.Errorf("--%s: set=%v url=%q", name, cpURLWasSet(fs), g.cpURL)
		}
	}
}

// Every variable the CLI reads has the ASP_ prefix, apart from the ones that are not
// ours.
func TestNoVariableIsReadWithoutThePrefix(t *testing.T) {
	external := map[string]bool{}
	root := filepath.Join("..", "..")
	found := map[string][]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			var fn string
			switch c := call.Fun.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := c.X.(*ast.Ident); ok {
					fn = pkg.Name + "." + c.Sel.Name
				}
			case *ast.Ident:
				fn = c.Name
			}
			switch fn {
			case "os.Getenv", "os.LookupEnv", "os.Setenv", "envcfg.Getenv", "envcfg.Truthy", "envcfg.Get", "envOr":
			default:
				return true
			}
			arg := call.Args[0]
			if fn == "envcfg.Get" && len(call.Args) > 1 {
				arg = call.Args[1]
			}
			lit, ok := arg.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			name, _ := strconv.Unquote(lit.Value)
			found[name] = append(found[name], filepath.ToSlash(path))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) < 5 {
		t.Fatalf("only %d variables found: is the scan reading the sources?", len(found))
	}
	var bad []string
	for name, files := range found {
		if !strings.HasPrefix(name, "ASP_") && !external[name] {
			bad = append(bad, name+" in "+strings.Join(files, ", "))
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("variables read without the ASP_ prefix:\n  %s", strings.Join(bad, "\n  "))
	}
}

func newTestFlagSet() *flag.FlagSet { return flag.NewFlagSet("t", flag.ContinueOnError) }
