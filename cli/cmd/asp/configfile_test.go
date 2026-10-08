package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/envcfg"
)

// No test reads the files of whoever runs it: the two places are empty ones, and the
// variable that names a file is gone.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "asp-cli-test")
	if err != nil {
		panic(err)
	}
	systemConfigPath = filepath.Join(dir, "etc", "asp.yaml")
	userConfigPath = func() string { return filepath.Join(dir, "home", "asp.yaml") }
	os.Unsetenv("ASP_CONFIG")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// The names the source mentions that are not settings of the CLI's file: the variable that names
// the file itself, and the node-agent's own setting that asp doctor hands over to it.
var notSettings = map[string]bool{"ASP_CONFIG": true, "ASP_CONTROL_PLANE_CA": true}

var settingName = regexp.MustCompile(`^ASP_[A-Z0-9_]+$`)

// variablesInSource is every string in the CLI's sources that is exactly the name of a variable:
// a read, or a constant that is read somewhere.
func variablesInSource(t *testing.T) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(filepath.Join("..", ".."), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// internal/standalone names the environment of the programs asp-server starts, not the CLI's.
		if d.IsDir() && d.Name() == "standalone" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || filepath.Base(path) == "settingstable.go" {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if v, err := strconv.Unquote(lit.Value); err == nil && settingName.MatchString(v) {
					found[v] = true
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) < 10 {
		t.Fatalf("only %d variables found: is the scan reading the sources?", len(found))
	}
	return found
}

func TestTheTableIsEveryVariableTheSourceReads(t *testing.T) {
	found := variablesInSource(t)
	table := map[string]bool{}
	for _, s := range settingsTable {
		if table[s.Env] {
			t.Errorf("%s is in the table twice", s.Env)
		}
		table[s.Env] = true
		if s.Help == "" {
			t.Errorf("%s has no help text", s.Env)
		}
	}
	for name := range found {
		if !table[name] && !notSettings[name] {
			t.Errorf("the source reads %s, which is not in settingsTable (a config file could not set it and `asp config show` would not list it)", name)
		}
	}
	for name := range table {
		if !found[name] {
			t.Errorf("settingsTable lists %s, which the source does not read", name)
		}
	}
	keys := configKeys()
	legacy := 0
	for _, s := range settingsTable {
		if s.Legacy {
			legacy++
		}
	}
	if len(keys) != len(settingsTable)-legacy {
		t.Errorf("%d settings give %d keys: two names collapse into one key", len(settingsTable)-legacy, len(keys))
	}
}

func writeConf(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// isolate gives the test empty places for the config files and none of the variables, and
// puts everything back when it ends.
func isolate(t *testing.T) (system, user string) {
	t.Helper()
	dir := t.TempDir()
	system, user = filepath.Join(dir, "etc", "asp.yaml"), filepath.Join(dir, "home", ".config", "asp", "asp.yaml")
	oldSystem, oldUser := systemConfigPath, userConfigPath
	systemConfigPath, userConfigPath = system, func() string { return user }
	was := map[string]string{}
	for _, s := range settingsTable {
		if v, ok := os.LookupEnv(s.Env); ok {
			was[s.Env] = v
		}
		os.Unsetenv(s.Env)
	}
	t.Cleanup(func() {
		systemConfigPath, userConfigPath = oldSystem, oldUser
		// What the file layer set, in any call of the test, must not leak into the next one.
		for _, s := range settingsTable {
			os.Unsetenv(s.Env)
			if v, ok := was[s.Env]; ok {
				os.Setenv(s.Env, v)
			}
		}
		loaded.paths, loaded.applied = nil, nil
	})
	return system, user
}

func TestTheConfigFilesSitBelowTheEnvironment(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	conf := writeConf(t, dir, "asp.yaml", `
control_plane_url: http://cp.example:8080
tenant: acme
idp:
  token_url: https://idp.example/token
  grant_type: password
session_dir: /tmp/sessions
`)
	writeConf(t, dir, "asp.yaml.d/10-site.yaml", "tenant: widgets\nidp_username: alice\n")
	t.Setenv("ASP_CONTROL_PLANE_URL", "http://env.example:1")
	if err := loadConfigFiles(configChoice{path: conf, named: true}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"ASP_CONTROL_PLANE_URL": "http://env.example:1", // the environment wins
		"ASP_TENANT":            "widgets",              // a drop-in wins over the base file
		"ASP_IDP_TOKEN_URL":     "https://idp.example/token",
		"ASP_IDP_GRANT_TYPE":    "password",
		"ASP_IDP_USERNAME":      "alice",
		"ASP_SESSION_DIR":       "/tmp/sessions",
	} {
		if got := os.Getenv(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if _, fromFile := loaded.applied["ASP_CONTROL_PLANE_URL"]; fromFile {
		t.Error("ASP_CONTROL_PLANE_URL came from the environment, not the file")
	}
	if got := loaded.applied["ASP_TENANT"]; !strings.HasSuffix(got, "10-site.yaml") {
		t.Errorf("ASP_TENANT came from %q, want the drop-in", got)
	}
}

func TestTheUsersFileWinsOverTheSystemsAndTheirDropInsMerge(t *testing.T) {
	system, user := isolate(t)
	writeConf(t, filepath.Dir(system), "asp.yaml", "control_plane_url: http://system:8080\ntenant: from-system\nidp_issuer: https://idp.system\n")
	writeConf(t, filepath.Dir(user), "asp.yaml", "tenant: from-user\n")
	writeConf(t, filepath.Dir(user), "asp.yaml.d/50.yaml", "idp_client_id: user-drop-in\n")
	if err := loadConfigFiles(configChoice{}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"ASP_TENANT":            "from-user",
		"ASP_CONTROL_PLANE_URL": "http://system:8080",
		"ASP_IDP_ISSUER":        "https://idp.system",
		"ASP_IDP_CLIENT_ID":     "user-drop-in",
	} {
		if got := os.Getenv(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if len(loaded.paths) != 3 || loaded.paths[0] != system {
		t.Errorf("files read: %v", loaded.paths)
	}
}

func TestNoFileIsNothingToDo(t *testing.T) {
	isolate(t)
	if err := loadConfigFiles(configChoice{}); err != nil {
		t.Fatalf("no config file is not an error: %v", err)
	}
	if len(loaded.paths) != 0 || len(loaded.applied) != 0 {
		t.Errorf("read %v, set %v", loaded.paths, loaded.applied)
	}
}

func TestAKeyThatIsNoSettingStopsTheCommand(t *testing.T) {
	isolate(t)
	// cp_url is the old name of control_plane_url: it still works as a variable, but a file is new.
	conf := writeConf(t, t.TempDir(), "asp.yaml", "contrl_plane_url: x\nasp_tenant: t\ncp_url: y\n")
	var stdout, stderr strings.Builder
	code := run([]string{"--config", conf, "sandbox", "list"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit %d, want 2; stderr %q", code, stderr.String())
	}
	for _, want := range []string{"contrl_plane_url", "did you mean control_plane_url", "asp_tenant", "did you mean tenant", "cp_url", filepath.Base(conf)} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr.String())
		}
	}
	if os.Getenv("ASP_TENANT") != "" {
		t.Error("a file with an unknown key set some of its variables before failing")
	}
}

func TestANamedFileThatIsMissingIsAnError(t *testing.T) {
	isolate(t)
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	var stdout, stderr strings.Builder
	if code := run([]string{"--config=" + missing, "sandbox", "list"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "nope.yaml") {
		t.Errorf("--config: exit %d, stderr %q", code, stderr.String())
	}
	t.Setenv("ASP_CONFIG", missing)
	stderr.Reset()
	if code := run([]string{"sandbox", "list"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "nope.yaml") {
		t.Errorf("ASP_CONFIG: exit %d, stderr %q", code, stderr.String())
	}
}

func TestAnEmptyNameReadsNoFile(t *testing.T) {
	system, _ := isolate(t)
	writeConf(t, filepath.Dir(system), "asp.yaml", "not_a_setting: 1\n")
	if err := loadConfigFiles(configChoice{}); err == nil {
		t.Fatal("the system file has an unknown key and was not refused")
	}
	if err := loadConfigFiles(configChoice{path: "", named: true}); err != nil {
		t.Fatalf("--config= must read no file: %v", err)
	}
}

func TestConfigBeforeTheCommandIsOurs_AfterItIsTheCommands(t *testing.T) {
	for _, c := range []struct {
		args  []string
		rest  []string
		path  string
		named bool
		fails bool
	}{
		{[]string{"--config", "a.yaml", "sandbox", "list"}, []string{"sandbox", "list"}, "a.yaml", true, false},
		{[]string{"--config=a.yaml", "sandbox", "list"}, []string{"sandbox", "list"}, "a.yaml", true, false},
		{[]string{"-config", "a.yaml", "version"}, []string{"version"}, "a.yaml", true, false},
		{[]string{"--config=", "sandbox"}, []string{"sandbox"}, "", true, false},
		{[]string{"sandbox", "exec", "id", "tool", "--config", "x"}, []string{"sandbox", "exec", "id", "tool", "--config", "x"}, "", false, false},
		{[]string{"sandbox", "exec", "id", "--", "tool", "--config=x"}, []string{"sandbox", "exec", "id", "--", "tool", "--config=x"}, "", false, false},
		{[]string{"--config"}, nil, "", false, true},
		{nil, nil, "", false, false},
	} {
		rest, got, err := leadingConfigFlag(c.args)
		if (err != nil) != c.fails {
			t.Errorf("%v: error %v", c.args, err)
			continue
		}
		if c.fails {
			continue
		}
		if strings.Join(rest, "\x00") != strings.Join(c.rest, "\x00") || got.path != c.path || got.named != c.named {
			t.Errorf("%v: rest %v choice %+v, want %v %q %v", c.args, rest, got, c.rest, c.path, c.named)
		}
	}
}

// The file is the third place a setting comes from, and it reaches the command: the control
// plane URL and the key a command uses can all be in it.
func TestTheConfigFileReachesTheCommands(t *testing.T) {
	isolate(t)
	var auth string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []any{}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	conf := writeConf(t, t.TempDir(), "asp.yaml", "control_plane_url: "+srv.URL+"\ntenant: acme\napi_key: key-from-the-file\n")

	var stdout, stderr strings.Builder
	if code := run([]string{"--config", conf, "sandbox", "list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if auth != "Bearer key-from-the-file" {
		t.Errorf("the control plane saw %q", auth)
	}

	// A flag and the environment still win over the file.
	srv2, hits := listServer(t)
	auth = ""
	t.Setenv("ASP_API_KEY", "key-from-the-environment")
	stderr.Reset()
	if code := run([]string{"--config", conf, "sandbox", "list", "--control-plane-url", srv2.URL}, &stdout, &stderr); code != 0 || *hits != 1 {
		t.Fatalf("flag over file: exit %d, hits %d, stderr %q", code, *hits, stderr.String())
	}
}

func TestAWorldReadableFileWithACredentialWarns(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	for _, c := range []struct {
		name, body string
		mode       os.FileMode
		warns      bool
	}{
		{"open-key.yaml", "api_key: k\n", 0o644, true},
		{"private-key.yaml", "api_key: k\n", 0o600, false},
		{"open-url.yaml", "control_plane_url: http://x\n", 0o644, false},
	} {
		p := writeConf(t, dir, c.name, c.body)
		if err := os.Chmod(p, c.mode); err != nil {
			t.Fatal(err)
		}
		var warnings []string
		prev := envcfg.Warn
		envcfg.Warn = func(m string) { warnings = append(warnings, m) }
		err := loadConfigFiles(configChoice{path: p, named: true})
		envcfg.Warn = prev
		if err != nil {
			t.Fatal(err)
		}
		for name := range loaded.applied {
			os.Unsetenv(name)
		}
		if got := len(warnings) > 0; got != c.warns {
			t.Errorf("%s: warnings %q, want a warning: %v", c.name, warnings, c.warns)
		}
		if c.warns && !strings.Contains(warnings[0], "chmod 600") {
			t.Errorf("%s: warning %q does not say what to do", c.name, warnings[0])
		}
	}
}

func TestConfigShowListsSourcesAndHidesCredentials(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	conf := writeConf(t, dir, "asp.yaml", "control_plane_url: http://cp.example\napi_key: s3cret-file-key\nidp_username: alice\n")
	t.Setenv("ASP_IDP_PASSWORD", "env-password")
	t.Setenv("ASP_TENANT", "from-env")

	var stdout, stderr strings.Builder
	if code := run([]string{"config", "show", "--config", conf}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	text := stdout.String()
	for _, want := range []string{
		"# files: " + conf,
		"control_plane_url: http://cp.example  # file " + conf,
		"api_key: <redacted>  # file " + conf,
		"idp_username: alice  # file " + conf,
		"idp_password: <redacted>  # environment",
		"tenant: from-env  # environment",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("config show lacks %q:\n%s", want, text)
		}
	}
	for _, secret := range []string{"s3cret-file-key", "env-password"} {
		if strings.Contains(text, secret) {
			t.Errorf("config show prints a credential: %s", secret)
		}
	}
	if strings.Contains(text, "# session_dir") {
		t.Errorf("without --effective only what is set is listed:\n%s", text)
	}

	stdout.Reset()
	if code := run([]string{"config", "show", "--effective", "--config", conf}, &stdout, &stderr); code != 0 {
		t.Fatalf("--effective: exit %d, stderr %q", code, stderr.String())
	}
	for _, want := range []string{"# session_dir: (~/.cache/asp/sessions)", "# idp_issuer: (unset)"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("--effective lacks %q:\n%s", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "cp_url") || strings.Contains(stdout.String(), "idp_required") {
		t.Errorf("the old names are not settings:\n%s", stdout.String())
	}
}

func TestConfigShowOfTheServerAndTheAgent(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	conf := writeConf(t, dir, "server.yaml", "listen_addr: 127.0.0.1:9999\ndatabase_url: postgres://asp:pw-123@db/asp\nfence_pass: x\nsched:\n  policy: binpack\n")
	writeConf(t, dir, "server.yaml.d/10.yaml", "shutdown_timeout: 20s\n")
	var stdout, stderr strings.Builder
	if code := run([]string{"config", "show", "--component", "server", "--config", conf}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	text := stdout.String()
	for _, want := range []string{"listen_addr: 127.0.0.1:9999  # " + conf, "database_url: <redacted>", "fence_pass: <redacted>", "sched_policy: binpack", "shutdown_timeout: 20s  # " + conf + ".d/10.yaml", "asp-control-plane --print-config"} {
		if !strings.Contains(text, want) {
			t.Errorf("lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "pw-123") {
		t.Errorf("prints the database password:\n%s", text)
	}

	stdout.Reset()
	if code := run([]string{"config", "show", "--component=agent"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "no config file") {
		t.Errorf("agent without a file: exit %d, %q", code, stdout.String())
	}
	if code := run([]string{"config", "show", "--component", "nobody"}, &stdout, &stderr); code != 2 {
		t.Errorf("unknown component: exit %d", code)
	}
	if code := run([]string{"config"}, &stdout, &stderr); code != 2 {
		t.Errorf("config without a subcommand: exit %d", code)
	}
}

// A broken file must be diagnosable with the command meant to diagnose it.
func TestConfigShowRunsWhenTheFileIsBroken(t *testing.T) {
	isolate(t)
	conf := writeConf(t, t.TempDir(), "asp.yaml", "nonsense_key: 1\n")
	var stdout, stderr strings.Builder
	code := run([]string{"config", "show", "--config", conf}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "nonsense_key") {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
}
