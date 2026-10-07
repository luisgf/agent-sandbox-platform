package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// names the source mentions that are not settings of a file: the file's own path, and the
// variables only the tests use.
var notSettings = map[string]bool{"ASP_CONFIG": true}

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
			t.Errorf("the source reads %s, which is not in settingsTable (a config file could not set it and --print-config would not show it)", name)
		}
	}
	for name := range table {
		if !found[name] {
			t.Errorf("settingsTable lists %s, which the source does not read", name)
		}
	}
	keys := configKeys()
	if len(keys) != len(settingsTable) {
		t.Errorf("%d settings give %d keys: two names collapse into one key", len(settingsTable), len(keys))
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

// clearSettings unsets every variable the control plane reads for the test, and puts them back.
func clearSettings(t *testing.T) {
	t.Helper()
	for _, s := range settingsTable {
		if v, ok := os.LookupEnv(s.Env); ok {
			os.Unsetenv(s.Env)
			name, val := s.Env, v
			t.Cleanup(func() { os.Setenv(name, val) })
		}
	}
	// What the file layer sets must not leak into the next test.
	t.Cleanup(func() {
		for name := range fileSettings.applied {
			os.Unsetenv(name)
		}
		fileSettings.paths, fileSettings.applied = nil, nil
	})
}

func TestTheConfigFileSitsBelowTheEnvironment(t *testing.T) {
	clearSettings(t)
	dir := t.TempDir()
	conf := writeConf(t, dir, "server.yaml", `
listen_addr: 127.0.0.1:9999
shutdown_timeout: 12s
sched:
  policy: binpack
node_failover_after: off
sandbox_idle_timeout: 2h
`)
	writeConf(t, dir, "server.yaml.d/10-site.yaml", "shutdown_timeout: 20s\nsched_cpu_overcommit: 2\n")
	t.Setenv("ASP_LISTEN_ADDR", "127.0.0.1:1111")
	if err := loadConfigFile([]string{"--config", conf}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"ASP_LISTEN_ADDR":          "127.0.0.1:1111", // the environment wins
		"ASP_SHUTDOWN_TIMEOUT":     "20s",            // a drop-in wins over the base file
		"ASP_SCHED_POLICY":         "binpack",        // a nested key
		"ASP_SCHED_CPU_OVERCOMMIT": "2",
		"ASP_NODE_FAILOVER_AFTER":  "off",
		"ASP_SANDBOX_IDLE_TIMEOUT": "2h",
	} {
		if got := os.Getenv(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if _, fromFile := fileSettings.applied["ASP_LISTEN_ADDR"]; fromFile {
		t.Error("ASP_LISTEN_ADDR came from the environment, not the file")
	}
}

func TestAKeyThatIsNoSettingStopsTheStart(t *testing.T) {
	clearSettings(t)
	dir := t.TempDir()
	conf := writeConf(t, dir, "server.yaml", "listen_adr: x\nasp_shutdown_timeout: 1s\n")
	err := run(context.Background(), []string{"--config", conf})
	if err == nil {
		t.Fatal("a file with unknown keys started the control plane")
	}
	for _, want := range []string{"listen_adr", "did you mean listen_addr", "asp_shutdown_timeout", "did you mean shutdown_timeout", filepath.Base(conf)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if _, isConfig := err.(configError); !isConfig {
		t.Errorf("a bad config file must be a configuration error (exit 2), got %T", err)
	}
	if os.Getenv("ASP_SHUTDOWN_TIMEOUT") != "" {
		t.Error("a file with an unknown key set some of its variables before failing")
	}
}

func TestANamedConfigFileThatIsMissingIsAnError(t *testing.T) {
	clearSettings(t)
	if err := loadConfigFile([]string{"--config=" + filepath.Join(t.TempDir(), "nope.yaml")}); err == nil {
		t.Fatal("a missing named file is not an error")
	}
	t.Setenv("ASP_CONFIG", filepath.Join(t.TempDir(), "nope.yaml"))
	if err := loadConfigFile(nil); err == nil {
		t.Fatal("a missing file named by ASP_CONFIG is not an error")
	}
}

func TestPrintConfigShowsSourcesAndHidesCredentials(t *testing.T) {
	clearSettings(t)
	dir := t.TempDir()
	conf := writeConf(t, dir, "server.yaml", "listen_addr: 127.0.0.1:9999\nbootstrap_api_key: s3cret-key\ndatabase_url: postgres://asp:pw-123@db/asp\n")
	t.Setenv("ASP_FENCE_PASS", "env-secret")
	if err := loadConfigFile([]string{"--config", conf}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	printConfig(&out)
	text := out.String()
	for _, want := range []string{
		"listen_addr: 127.0.0.1:9999  # file " + conf,
		"bootstrap_api_key: <redacted>  # file " + conf,
		"database_url: <redacted>  # file " + conf,
		"fence_pass: <redacted>  # environment",
		"# shutdown_timeout: (30s)",
		"# files: " + conf,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("print-config lacks %q:\n%s", want, text)
		}
	}
	for _, secret := range []string{"s3cret-key", "pw-123", "env-secret"} {
		if strings.Contains(text, secret) {
			t.Errorf("print-config shows a credential: %s", secret)
		}
	}
}

func TestPrintConfigDoesNotStartTheServer(t *testing.T) {
	clearSettings(t)
	t.Setenv("ASP_LISTEN_ADDR", "not an address")
	if err := run(context.Background(), []string{"--print-config"}); err != nil {
		t.Fatalf("--print-config: %v", err)
	}
}

func TestConfigFlag(t *testing.T) {
	clearSettings(t)
	for args, want := range map[string]string{
		"--config /a.yaml": "/a.yaml", "--config=/b.yaml": "/b.yaml", "-config /c.yaml": "/c.yaml",
		"--idle-timeout 2h": defaultConfigFile,
	} {
		if got, _ := configFlag(strings.Fields(args)); got != want {
			t.Errorf("configFlag(%q) = %q, want %q", args, got, want)
		}
	}
}
