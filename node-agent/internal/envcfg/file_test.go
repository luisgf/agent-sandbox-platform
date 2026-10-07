package envcfg

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

var known = map[string]string{
	"listen_addr":          "ASP_LISTEN_ADDR",
	"database_url":         "ASP_DATABASE_URL",
	"egress_proxy_listen":  "ASP_EGRESS_PROXY_LISTEN",
	"reconcile":            "ASP_RECONCILE",
	"workspace_roots":      "ASP_WORKSPACE_ROOTS",
	"max_sandboxes":        "ASP_MAX_SANDBOXES",
	"sched_cpu_overcommit": "ASP_SCHED_CPU_OVERCOMMIT",
}

func TestFileFlattensNestedKeysListsAndScalars(t *testing.T) {
	dir := t.TempDir()
	base := write(t, dir, "agent.yaml", `
listen_addr: 127.0.0.1:9100
egress:
  proxy_listen: ":8888"
reconcile: true
max-sandboxes: 12
sched_cpu_overcommit: 2.5
workspace_roots:
  - /srv/ws
  - /home/ws
`, 0o600)
	f, err := LoadFile(base, true)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"listen_addr": "127.0.0.1:9100", "egress_proxy_listen": ":8888", "reconcile": "true",
		"max_sandboxes": "12", "sched_cpu_overcommit": "2.5", "workspace_roots": "/srv/ws,/home/ws",
	}
	if !reflect.DeepEqual(f.Values, want) {
		t.Fatalf("values:\n got %v\nwant %v", f.Values, want)
	}
	if err := f.Check(known); err != nil {
		t.Fatal(err)
	}
}

// Drop-ins merge in name order and the last file that sets a key wins; a null takes a key away.
func TestDropInsMergeLastWins(t *testing.T) {
	dir := t.TempDir()
	base := write(t, dir, "agent.yaml", "listen_addr: base\nreconcile: false\nmax_sandboxes: 4\n", 0o600)
	write(t, dir, "agent.yaml.d/20-b.yaml", "listen_addr: b\negress:\n  proxy_listen: b\n", 0o600)
	write(t, dir, "agent.yaml.d/10-a.yaml", "listen_addr: a\nreconcile: true\n", 0o600)
	write(t, dir, "agent.yaml.d/30-c.yml", "max_sandboxes: ~\n", 0o600)
	write(t, dir, "agent.yaml.d/README.txt", "listen_addr: ignored\n", 0o600)
	write(t, dir, "agent.yaml.d/.hidden.yaml", "listen_addr: ignored\n", 0o600)
	f, err := LoadFile(base, true)
	if err != nil {
		t.Fatal(err)
	}
	if f.Values["listen_addr"] != "b" || f.Values["reconcile"] != "true" || f.Values["egress_proxy_listen"] != "b" {
		t.Fatalf("merged: %v", f.Values)
	}
	if _, has := f.Values["max_sandboxes"]; has {
		t.Fatalf("a null did not remove max_sandboxes: %v", f.Values)
	}
	if f.From["listen_addr"] != filepath.Join(dir, "agent.yaml.d", "20-b.yaml") || f.From["reconcile"] != filepath.Join(dir, "agent.yaml.d", "10-a.yaml") {
		t.Fatalf("sources: %v", f.From)
	}
	var names []string
	for _, p := range f.Paths {
		names = append(names, filepath.Base(p))
	}
	if strings.Join(names, " ") != "agent.yaml 10-a.yaml 20-b.yaml 30-c.yml" {
		t.Fatalf("files read: %v", names)
	}
}

func TestMissingFiles(t *testing.T) {
	dir := t.TempDir()
	f, err := LoadFile(filepath.Join(dir, "none.yaml"), false)
	if err != nil || len(f.Values) != 0 || len(f.Paths) != 0 {
		t.Fatalf("a missing optional file: %+v %v", f, err)
	}
	if _, err := LoadFile(filepath.Join(dir, "none.yaml"), true); err == nil {
		t.Fatal("a missing file the operator named must be an error")
	}
	// Only drop-ins, no base file.
	write(t, dir, "x.yaml.d/10.yaml", "listen_addr: d\n", 0o600)
	f, err = LoadFile(filepath.Join(dir, "x.yaml"), false)
	if err != nil || f.Values["listen_addr"] != "d" {
		t.Fatalf("drop-ins without a base: %+v %v", f, err)
	}
	if f, err := LoadFile("", false); err != nil || len(f.Values) != 0 {
		t.Fatalf("no path: %+v %v", f, err)
	}
}

func TestBadFiles(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"a list at the top":        "- a\n- b\n",
		"not yaml":                 "listen_addr: [unclosed\n",
		"a scalar at the top":      "just words\n",
		"a bad key":                "listen addr: x\n",
		"a key with a dot":         "listen.addr: x\n",
		"a list of maps":           "workspace_roots:\n  - a: b\n",
		"set twice, nested + flat": "egress_proxy_listen: a\negress:\n  proxy_listen: b\n",
	} {
		p := write(t, dir, strings.ReplaceAll(name, " ", "_")+".yaml", body, 0o600)
		if _, err := LoadFile(p, true); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.Contains(err.Error(), filepath.Base(p)) {
			t.Errorf("%s: the error does not name the file: %v", name, err)
		}
	}
	// Too big.
	big := write(t, dir, "big.yaml", "x: "+strings.Repeat("a", maxFileBytes), 0o600)
	if _, err := LoadFile(big, true); err == nil || !strings.Contains(err.Error(), "too much") {
		t.Errorf("a file over the limit: %v", err)
	}
	// A directory where the file should be.
	if _, err := LoadFile(dir, true); err == nil {
		t.Error("a directory as the config file was accepted")
	}
}

func TestUnknownKeysAreErrorsWithASuggestion(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "agent.yaml", "listen_adr: x\nasp_reconcile: true\ncompletely_other: 1\negress:\n  proxy_listn: y\n", 0o600)
	f, err := LoadFile(p, true)
	if err != nil {
		t.Fatal(err)
	}
	err = f.Check(known)
	if err == nil {
		t.Fatal("unknown keys accepted")
	}
	for _, want := range []string{"listen_adr", "did you mean listen_addr", "asp_reconcile", "did you mean reconcile", "completely_other", "proxy_listn", "did you mean egress_proxy_listen", p} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "completely_other\" is not a setting (did you mean") {
		t.Errorf("a key nothing like any setting got a suggestion:\n%v", err)
	}
}

func TestLayerAndLookup(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "agent.yaml", "listen_addr: from-file\nreconcile: true\ndatabase_url: \"\"\n", 0o600)
	f, _ := LoadFile(p, true)
	env := map[string]string{"ASP_LISTEN_ADDR": "from-env"}
	look := Layer(func(n string) (string, bool) { v, ok := env[n]; return v, ok }, f.Lookup(known))
	if v, _ := look("ASP_LISTEN_ADDR"); v != "from-env" {
		t.Errorf("the environment must win: %q", v)
	}
	if v, ok := look("ASP_RECONCILE"); !ok || v != "true" {
		t.Errorf("a key only the file has: %q %v", v, ok)
	}
	if _, ok := look("ASP_DATABASE_URL"); ok {
		t.Error("an empty value counts as not set, as in the environment")
	}
	if _, ok := look("ASP_NOTHING"); ok {
		t.Error("an unknown variable has a value")
	}
}

func TestApplyToEnvKeepsWhatTheEnvironmentHas(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "server.yaml", "listen_addr: from-file\nreconcile: true\n", 0o600)
	f, _ := LoadFile(p, true)
	t.Setenv("ASP_LISTEN_ADDR", "from-env")
	t.Setenv("ASP_RECONCILE", "")
	os.Unsetenv("ASP_RECONCILE")
	set, err := f.ApplyToEnv(known)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Unsetenv("ASP_RECONCILE") })
	if os.Getenv("ASP_LISTEN_ADDR") != "from-env" {
		t.Error("the file overrode the environment")
	}
	if os.Getenv("ASP_RECONCILE") != "true" || set["ASP_RECONCILE"] != p || len(set) != 1 {
		t.Errorf("applied %v, ASP_RECONCILE=%q", set, os.Getenv("ASP_RECONCILE"))
	}
	// An unknown key stops it before anything is set.
	bad := write(t, dir, "bad.yaml", "reconcile: false\nnonsense: 1\n", 0o600)
	fb, _ := LoadFile(bad, true)
	t.Setenv("ASP_MAX_SANDBOXES", "")
	os.Unsetenv("ASP_MAX_SANDBOXES")
	if _, err := fb.ApplyToEnv(known); err == nil {
		t.Error("an unknown key was applied")
	}
}

func TestLooseFilesAreReported(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "agent.yaml", "listen_addr: x\n", 0o644)
	q := write(t, dir, "agent.yaml.d/10.yaml", "reconcile: true\n", 0o600)
	f, _ := LoadFile(p, true)
	if !f.Loose[p] || f.Loose[q] {
		t.Errorf("loose files: %v", f.Loose)
	}
}

func TestSuggest(t *testing.T) {
	names := []string{"listen_addr", "database_url", "egress_proxy_listen"}
	for key, want := range map[string]string{
		"listen_adr": "listen_addr", "databse_url": "database_url", "asp_listen_addr": "listen_addr",
		"zzzzzzzzzz": "", "l": "",
	} {
		if got := Suggest(key, names); got != want {
			t.Errorf("Suggest(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestKeyOf(t *testing.T) {
	for in, want := range map[string]string{"ASP_LISTEN_ADDR": "listen_addr", "ASP_DB_STATEMENT_TIMEOUT": "db_statement_timeout", "FOO": "foo"} {
		if got := KeyOf(in); got != want {
			t.Errorf("KeyOf(%q) = %q, want %q", in, got, want)
		}
	}
}
