package main

import (
	"flag"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/settings"
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

// uncommented turns the example lines (#key: value, no space after the #) of a settings file
// into settings, and leaves the prose comments as they are.
func uncommented(src string) string {
	re := regexp.MustCompile(`(?m)^#([a-z][a-z0-9_]*:)`)
	return re.ReplaceAllString(src, "$1")
}

// The files the package installs and the lab's are accepted by the node-agent, examples
// included: a key it does not know (a setting that was renamed, or never existed) stops it
// at its first start.
func TestPackagedSettingsFilesAreAcceptedByTheNodeAgent(t *testing.T) {
	for _, path := range []string{"packaging/etc/agent.yaml", "scripts/systemd/lab/agent.yaml"} {
		src := readRepoFile(t, path)
		for name, text := range map[string]string{"as installed": src, "with its examples on": uncommented(src)} {
			conf := writeConf(t, t.TempDir(), "agent.yaml", text)
			if _, _, _, err := loadWith(t, nil, "--config", conf); err != nil {
				t.Errorf("%s (%s): %v", path, name, err)
			}
		}
	}
}

// What the package's file says is what the unit used to say with Environment= lines: the
// settings every node needs.
func TestPackagedAgentYAMLHasTheSettingsEveryNodeNeeds(t *testing.T) {
	conf := writeConf(t, t.TempDir(), "agent.yaml", readRepoFile(t, "packaging/etc/agent.yaml"))
	cfg, _, _, err := loadWith(t, nil, "--config", conf)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MTLS || !cfg.Reconcile || !cfg.TapAuto || !cfg.HostVsock || !cfg.EgressEnforce {
		t.Errorf("mtls, reconcile, tap_auto, host_vsock and egress_enforce are on: %+v", cfg)
	}
	for name, got := range map[string][2]string{
		"ch_socket_dir":       {cfg.CHSocketDir, "/run/asp"},
		"disk_dir":            {cfg.DiskDir, "/var/lib/asp/disks"},
		"local_net_key_dir":   {cfg.LocalNetKeyDir, "/var/lib/asp/local-net"},
		"cert_dir":            {cfg.CertDir, "/var/lib/asp/node-certs"},
		"egress_proxy_listen": {cfg.EgressProxyListen, ":8888"},
		"egress_dns_sink":     {cfg.EgressDNSSink, ":5353"},
	} {
		if got[0] != got[1] {
			t.Errorf("%s = %q, want %q", name, got[0], got[1])
		}
	}
}

// The units start the node-agent with its file, and set no variable that is a setting: an
// environment variable beats the file, so one in the unit could not be changed there.
func TestNodeAgentUnitsReadTheFileAndSetNoSettings(t *testing.T) {
	settingEnv := map[string]bool{}
	for _, st := range declared(t) {
		if st.Env != "" {
			settingEnv[st.Env] = true
		}
	}
	read := map[string]bool{}
	for _, name := range variablesReadInSource(t) {
		read[name] = true
	}
	envLine := regexp.MustCompile(`^Environment=(ASP_[A-Z0-9_]+)=`)
	for _, path := range []string{"packaging/systemd/asp-node-agent.service", "scripts/systemd/asp-node-agent.service", "scripts/systemd/asp-node-agent-lab.service"} {
		src := readRepoFile(t, path)
		if !regexp.MustCompile(`(?m)^ExecStart=\S*node-agent --config /etc/asp/agent\.yaml$`).MatchString(src) {
			t.Errorf("%s: ExecStart does not run the node-agent with --config /etc/asp/agent.yaml", path)
		}
		if !regexp.MustCompile(`(?m)^ExecStopPost=-\S*node-agent --config /etc/asp/agent\.yaml --reap-only$`).MatchString(src) {
			t.Errorf("%s: ExecStopPost does not clean up with the same file", path)
		}
		for _, line := range unitLines(src) {
			m := envLine.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			if settingEnv[m[1]] {
				t.Errorf("%s sets %s, a setting: it belongs in the YAML file (the environment would beat it)", path, m[1])
			}
			if !read[m[1]] {
				t.Errorf("%s sets %s, which the node-agent does not read", path, m[1])
			}
		}
	}
}

// What the installer writes into agent.yaml.d is accepted by the node-agent.
func TestInstallerAgentDropInsNameRealSettings(t *testing.T) {
	script := readRepoFile(t, "scripts/install.sh")
	part := script[strings.Index(script, "if has_role agent; then"):]
	body := strings.Builder{}
	for _, m := range regexp.MustCompile(`(?m)^([a-z][a-z0-9_]*): `).FindAllStringSubmatch(part, -1) {
		body.WriteString(m[1] + ": x\n")
	}
	if body.Len() == 0 {
		t.Fatal("no setting found in the installer's agent section: is it read right?")
	}
	// Only the keys are checked: the values here are placeholders.
	var cfg config
	fs := flag.NewFlagSet("node-agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	none := func(string) (string, bool) { return "", false }
	set := settings.New(fs, none)
	declareSettings(set, &cfg)
	conf := writeConf(t, t.TempDir(), "10-install.yaml", body.String())
	if _, err := useConfigFile(set, []string{"--config", conf}, none); err != nil {
		t.Errorf("the installer writes a key the node-agent does not have: %v", err)
	}
}
