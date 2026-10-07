package main

import (
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/settings"
)

// loadWith declares the settings on a private flag set, reads the config file the way loadConfig
// does and parses args, with env as the environment.
func loadWith(t *testing.T, env map[string]string, args ...string) (config, *settings.Set, *configSource, error) {
	t.Helper()
	var cfg config
	fs := flag.NewFlagSet("node-agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	look := func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok && strings.TrimSpace(v) != ""
	}
	s := settings.New(fs, look)
	declareSettings(s, &cfg)
	src, err := useConfigFile(s, args, look)
	if err != nil {
		return cfg, s, nil, err
	}
	return cfg, s, src, s.Parse(args)
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

// Precedence is the same everywhere: a flag beats the environment, which beats the file, which
// beats the default.
func TestPrecedenceFlagEnvironmentFileDefault(t *testing.T) {
	dir := t.TempDir()
	conf := writeConf(t, dir, "agent.yaml", `
node_id: from-file
control_plane_url: https://file.example:8443
max_sandboxes: 7
reconcile: true
egress:
  proxy_listen: ":8888"
`)
	env := map[string]string{"ASP_NODE_ID": "from-env", "ASP_CONTROL_PLANE_URL": "https://env.example"}
	cfg, _, _, err := loadWith(t, env, "--config", conf, "--control-plane-url", "https://flag.example")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ControlPlaneURL != "https://flag.example" {
		t.Errorf("flag over env and file: %q", cfg.ControlPlaneURL)
	}
	if cfg.NodeID != "from-env" {
		t.Errorf("env over file: %q", cfg.NodeID)
	}
	if cfg.MaxSandboxes != 7 || !cfg.Reconcile {
		t.Errorf("file over default: max_sandboxes=%d reconcile=%v", cfg.MaxSandboxes, cfg.Reconcile)
	}
	if cfg.EgressProxyListen != ":8888" {
		t.Errorf("a nested key: %q", cfg.EgressProxyListen)
	}
	if cfg.GuestVerify != "auto" {
		t.Errorf("a default nobody set: %q", cfg.GuestVerify)
	}
}

func TestDropInsOverrideTheBaseFile(t *testing.T) {
	dir := t.TempDir()
	conf := writeConf(t, dir, "agent.yaml", "node_id: base\nmax_sandboxes: 4\n")
	writeConf(t, dir, "agent.yaml.d/10-site.yaml", "node_id: site\n")
	writeConf(t, dir, "agent.yaml.d/20-local.yaml", "max_sandboxes: 9\n")
	cfg, _, src, err := loadWith(t, nil, "--config="+conf)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NodeID != "site" || cfg.MaxSandboxes != 9 {
		t.Errorf("node_id=%q max_sandboxes=%d", cfg.NodeID, cfg.MaxSandboxes)
	}
	if len(src.file.Paths) != 3 {
		t.Errorf("files: %v", src.file.Paths)
	}
}

func TestConfigFileFromTheEnvironmentOrNamedButMissing(t *testing.T) {
	dir := t.TempDir()
	conf := writeConf(t, dir, "elsewhere.yaml", "node_id: via-env\n")
	cfg, _, _, err := loadWith(t, map[string]string{"ASP_CONFIG": conf})
	if err != nil || cfg.NodeID != "via-env" {
		t.Fatalf("ASP_CONFIG: %q %v", cfg.NodeID, err)
	}
	if _, _, _, err := loadWith(t, nil, "--config", filepath.Join(dir, "nope.yaml")); err == nil {
		t.Fatal("a config file the operator named and that does not exist must be an error")
	}
	// No --config and no default file: nothing to read, not an error.
	if _, _, _, err := loadWith(t, nil); err != nil {
		t.Fatalf("no config file: %v", err)
	}
}

func TestAKeyThatIsNoSettingIsAnErrorThatSaysWhichOne(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"a typo":                "contrl_plane_url: x\n",
		"an action":             "doctor: true\n",
		"the file's own path":   "config: /other.yaml\n",
		"a variable's name":     "asp_node_id: x\n",
		"a flag-only print":     "print_config: true\n",
		"a legacy flag spelled": "ssh_agent_bridge_dir: x\n",
	} {
		conf := writeConf(t, dir, strings.ReplaceAll(name, " ", "_")+".yaml", body)
		_, _, _, err := loadWith(t, nil, "--config", conf)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), filepath.Base(conf)) {
			t.Errorf("%s: the error does not name the file: %v", name, err)
		}
	}
	conf := writeConf(t, dir, "typo.yaml", "contrl_plane_url: x\n")
	_, _, _, err := loadWith(t, nil, "--config", conf)
	if err == nil || !strings.Contains(err.Error(), "did you mean control_plane_url") {
		t.Errorf("no suggestion: %v", err)
	}
}

func TestEverySettingAFileMayHoldIsASettingWithAVariable(t *testing.T) {
	_, s, src, err := loadWith(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(src.known) < 60 {
		t.Fatalf("only %d keys", len(src.known))
	}
	for key, env := range src.known {
		if env == "" || !strings.HasPrefix(env, "ASP_") {
			t.Errorf("%s maps to %q", key, env)
		}
	}
	for _, flagOnly := range []string{"doctor", "print_config", "version", "reap_only", "print_measurement", "config"} {
		if _, ok := src.known[flagOnly]; ok {
			t.Errorf("%s is an action and must not be a key", flagOnly)
		}
	}
	_ = s
}

func TestPrintConfigShowsSourcesAndHidesCredentials(t *testing.T) {
	dir := t.TempDir()
	conf := writeConf(t, dir, "agent.yaml", "node_id: from-file\nenroll_token: s3cret-token\nmax_sandboxes: 3\n")
	env := map[string]string{"ASP_MAX_SANDBOXES": "5", "ASP_NODE_BOOTSTRAP_TOKEN": "other-secret"}
	t.Setenv("ASP_MAX_SANDBOXES", "5")
	t.Setenv("ASP_NODE_BOOTSTRAP_TOKEN", "other-secret")
	_, s, src, err := loadWith(t, env, "--config", conf, "--guest-verify", "on")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	printConfig(&out, s, src)
	text := out.String()
	for _, want := range []string{
		"node_id: from-file  # file " + conf,
		"max_sandboxes: 5  # environment",
		"guest_verify: \"on\"  # flag",
		"guest_verify:",
		"enroll_token: <redacted>",
		"bootstrap_token: <redacted>",
		"reap_leftovers: \"on\"  # default",
		"# files: " + conf,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("print-config lacks %q:\n%s", want, text)
		}
	}
	for _, secret := range []string{"s3cret-token", "other-secret"} {
		if strings.Contains(text, secret) {
			t.Errorf("print-config shows a credential: %s", secret)
		}
	}
	if strings.Contains(text, "doctor:") || strings.Contains(text, "print_config:") {
		t.Errorf("print-config lists actions:\n%s", text)
	}
}

func TestALooseFileWithACredentialIsReported(t *testing.T) {
	dir := t.TempDir()
	conf := writeConf(t, dir, "agent.yaml", "enroll_token: tok\n")
	if err := os.Chmod(conf, 0o644); err != nil {
		t.Fatal(err)
	}
	_, s, src, err := loadWith(t, nil, "--config", conf)
	if err != nil {
		t.Fatal(err)
	}
	var warned []string
	src.warnLooseSecrets(s, func(m string) { warned = append(warned, m) })
	if len(warned) != 1 || !strings.Contains(warned[0], "enroll_token") || !strings.Contains(warned[0], "chmod 600") {
		t.Errorf("warnings: %v", warned)
	}
	if err := os.Chmod(conf, 0o600); err != nil {
		t.Fatal(err)
	}
	_, s, src, _ = loadWith(t, nil, "--config", conf)
	warned = nil
	src.warnLooseSecrets(s, func(m string) { warned = append(warned, m) })
	if len(warned) != 0 {
		t.Errorf("a private file warned: %v", warned)
	}
}

func TestConfigFlagValue(t *testing.T) {
	for args, want := range map[string]string{
		"--config /a.yaml": "/a.yaml", "--config=/b.yaml": "/b.yaml", "-config /c.yaml": "/c.yaml", "-config=/d.yaml": "/d.yaml",
		"--node-id x": "", "--node-id --config": "", "-- --config /e.yaml": "",
	} {
		if got := configFlagValue(strings.Fields(args)); got != want {
			t.Errorf("configFlagValue(%q) = %q, want %q", args, got, want)
		}
	}
}
