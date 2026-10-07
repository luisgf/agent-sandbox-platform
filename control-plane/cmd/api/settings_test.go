package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/envcfg"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

func captureWarnings(t *testing.T) *[]string {
	t.Helper()
	var mu sync.Mutex
	var msgs []string
	old := envcfg.Warn
	envcfg.Warn = func(m string) { mu.Lock(); msgs = append(msgs, m); mu.Unlock() }
	envcfg.ResetWarnings()
	t.Cleanup(func() { envcfg.Warn = old })
	return &msgs
}

// Every variable the control plane reads has the ASP_ prefix, whatever the file
// that reads it: LISTEN_ADDR and DATABASE_URL did not, and a new one that does not
// fails here. The names that are not ours are listed.
func TestNoVariableIsReadWithoutThePrefix(t *testing.T) {
	external := map[string]bool{
		"BMC_PW": false, // a name an operator chooses for a fence secret (env:NAME), read in tests only
	}
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
			case "os.Getenv", "os.LookupEnv", "os.Setenv", "envcfg.Getenv", "envcfg.Truthy", "envcfg.Get":
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
	// Names that reach the read through a constant (Env… = "ASP_…") are not literals in the call.
	// They are checked below: every string constant that is used as a variable name.
	consts := map[string]string{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			g, ok := decl.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, spec := range g.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if !strings.HasPrefix(name.Name, "Env") && !strings.HasPrefix(name.Name, "env") {
						continue
					}
					if i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						v, _ := strconv.Unquote(lit.Value)
						consts[name.Name] = v + "\x00" + filepath.ToSlash(path)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range consts {
		name, file, _ := strings.Cut(v, "\x00")
		found[name] = append(found[name], file)
	}
	if len(found) < 20 {
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
		t.Fatalf("variables read without the ASP_ prefix (use envcfg.Get with the old name as a legacy one):\n  %s", strings.Join(bad, "\n  "))
	}
}

func TestListenAddrAndDatabaseURLKeepTheirOldNames(t *testing.T) {
	msgs := captureWarnings(t)
	for _, k := range []string{EnvListenAddr, "LISTEN_ADDR", EnvDatabaseURL, "DATABASE_URL"} {
		t.Setenv(k, "")
	}
	if listenAddr() != "" || databaseURL() != "" {
		t.Fatal("something is set")
	}
	t.Setenv("LISTEN_ADDR", "127.0.0.1:1")
	t.Setenv("DATABASE_URL", "postgres://old")
	if listenAddr() != "127.0.0.1:1" || databaseURL() != "postgres://old" {
		t.Fatalf("old names: %q %q", listenAddr(), databaseURL())
	}
	if len(*msgs) != 2 || !strings.Contains((*msgs)[0], "LISTEN_ADDR is deprecated: use ASP_LISTEN_ADDR") {
		t.Fatalf("warnings: %v", *msgs)
	}
	t.Setenv(EnvListenAddr, "127.0.0.1:2")
	t.Setenv(EnvDatabaseURL, "postgres://new")
	if listenAddr() != "127.0.0.1:2" || databaseURL() != "postgres://new" {
		t.Fatalf("new names: %q %q", listenAddr(), databaseURL())
	}
	// A control plane with only the old database variable is still a production one.
	t.Setenv(EnvDatabaseURL, "")
	if prod, why := prodMode(); !prod || !strings.Contains(why, EnvDatabaseURL) {
		t.Fatalf("prodMode: %v %q", prod, why)
	}
}

// What a tenant with no rules may reach is one setting, true or false. The old name
// said the opposite and is read only when the new one is not set.
func TestEgressDefaultIsOneSetting(t *testing.T) {
	for _, c := range []struct {
		name   string
		memory bool
		allow  string
		deny   string
		want   string // "" is a configuration error
		warns  bool
	}{
		{"memory store, nothing set: allow (development)", true, "", "", "1", false},
		{"postgres, nothing set: deny", false, "", "", "0", false},
		{"memory store, allow false: deny", true, "0", "", "0", false},
		{"postgres, allow true: allow", false, "yes", "", "1", false},
		{"the old name, true: deny", true, "", "1", "0", true},
		{"the old name, false: allow", false, "", "false", "1", true},
		{"the new name wins over the old", true, "1", "1", "1", true},
		{"not a boolean", true, "maybe", "", "", false},
		{"the old name, not a boolean", true, "", "maybe", "", false},
	} {
		msgs := captureWarnings(t)
		t.Setenv(EnvEgressDefaultAllow, c.allow)
		t.Setenv(legacyEgressDenyDefault, c.deny)
		err := resolveEgressDefault(c.memory)
		if c.want == "" {
			if err == nil {
				t.Errorf("%s: no error", c.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		// The store reads it from the environment, as it always did.
		allowed := store.EffectiveEgress("t", nil).Mode == store.EgressModeAllowAll
		if allowed != (c.want == "1") {
			t.Errorf("%s: the policy of a tenant without rules allows everything: %v, want %v", c.name, allowed, c.want == "1")
		}
		if c.warns != (len(*msgs) > 0) {
			t.Errorf("%s: warnings %v, want warning %v", c.name, *msgs, c.warns)
		}
	}
}
