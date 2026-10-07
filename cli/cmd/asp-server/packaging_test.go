package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/envcfg"
	"github.com/luisgf/agent-sandbox-platform/cli/internal/standalone"
)

const repoRoot = "../../../"

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(repoRoot + path)
	if err != nil {
		t.Skipf("%s is not here (a checkout of the CLI alone): %v", path, err)
	}
	return string(b)
}

func uncommented(src string) string {
	return regexp.MustCompile(`(?m)^#([a-z][a-z0-9_]*:)`).ReplaceAllString(src, "$1")
}

// The file the package installs is accepted by asp-server, examples included: a key that is no
// flag stops it at its first start.
func TestPackagedSettingsFileIsAccepted(t *testing.T) {
	src := readRepoFile(t, "packaging/etc/standalone.yaml")
	for name, text := range map[string]string{"as installed": src, "with its examples on": uncommented(src)} {
		fs, _, _ := newFlags(nil)
		conf := writeConf(t, t.TempDir(), "standalone.yaml", text)
		if err := applyConfigFile(fs, conf); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !strings.Contains(uncommented(src), "\nlisten:") || !strings.Contains(uncommented(src), "\ntls_san:") {
		t.Error("the example no longer shows how to let other hosts join (listen, tls_san)")
	}
}

// What the installer writes into standalone.yaml.d are flags of asp-server.
func TestInstallerStandaloneDropInNamesRealFlags(t *testing.T) {
	script := readRepoFile(t, "scripts/install.sh")
	start := strings.Index(script, "if has_role standalone; then")
	end := strings.Index(script[start:], "\nfi\n")
	if start < 0 || end < 0 {
		t.Fatal("the installer's standalone section is not where it was")
	}
	section := script[start : start+end]
	keys := regexp.MustCompile(`(?m)\bbody="\$\{body\}([a-z][a-z0-9_]*):`).FindAllStringSubmatch(section, -1)
	keys = append(keys, regexp.MustCompile(`(?m)\bbody="\$\{body\}([a-z][a-z0-9_]*): `).FindAllStringSubmatch(section, -1)...)
	if len(keys) < 4 {
		t.Fatalf("found %d settings in the installer's standalone section: is it read right?", len(keys))
	}
	var body strings.Builder
	seen := map[string]bool{}
	for _, m := range keys {
		if !seen[m[1]] {
			seen[m[1]] = true
			body.WriteString(m[1] + ": x\n")
		}
	}
	fs, _, _ := newFlags(nil)
	conf := filepath.Join(t.TempDir(), "10-install.yaml")
	if err := os.WriteFile(conf, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	// Only the keys are checked here: the values are placeholders.
	if err := checkKeys(fs, conf); err != nil {
		t.Errorf("the installer writes a setting asp-server does not have: %v", err)
	}
}

// The unit starts asp-server with its file, stops its children in order, and does not run with
// the units of the programs it starts.
func TestPackagedUnit(t *testing.T) {
	unit := readRepoFile(t, "packaging/systemd/asp-server.service")
	active := func(pattern string) bool { return regexp.MustCompile(`(?m)^` + pattern + `$`).MatchString(unit) }
	for what, pattern := range map[string]string{
		"ExecStart with its settings file":                        `ExecStart=/usr/bin/asp-server --config /etc/asp/standalone\.yaml`,
		"KillMode=mixed (asp-server stops its children in order)": `KillMode=mixed`,
		"a stop long enough for both programs":                    `TimeoutStopSec=(\d+)`,
		"no clash with the services of the same programs":         `Conflicts=asp-control-plane\.service asp-node-agent\.service`,
	} {
		if !active(pattern) {
			t.Errorf("the unit lacks %s", what)
		}
	}
	if m := regexp.MustCompile(`(?m)^TimeoutStopSec=(\d+)$`).FindStringSubmatch(unit); m != nil {
		secs := 0
		for _, c := range m[1] {
			secs = secs*10 + int(c-'0')
		}
		if secs < 2*40 {
			t.Errorf("TimeoutStopSec=%d is less than the 40 s each program gets", secs)
		}
	}
	if strings.Contains(unit, "KillMode=control-group") {
		t.Error("control-group would stop the control plane and the node at once")
	}
}

// A node asp-server starts behaves like the one the node-agent package installs: every setting the
// package's agent.yaml gives and asp-server gives too has the same value. (They differ only where
// asp-server decides, from --data-dir; with the default directory those are the same too.)
func TestTheNodeItStartsIsThePackagedNode(t *testing.T) {
	file, err := envcfg.LoadFile(repoRoot+"packaging/etc/agent.yaml", true)
	if err != nil {
		t.Skipf("packaging/etc/agent.yaml is not here (a checkout of the CLI alone): %v", err)
	}
	p := standalone.Prepared{Layout: standalone.Layout{Root: standalone.DefaultDataDir}, LocalURL: "https://127.0.0.1:8443", NodeID: "box", NodeToken: "t"}
	env := map[string]string{}
	for _, kv := range standalone.NodeAgentEnv(p, standalone.ProfileDefault, nil) {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	same := func(a, b string) bool {
		if ba, ok := envcfg.ParseBool(a); ok {
			bb, ok2 := envcfg.ParseBool(b)
			return ok2 && ba == bb
		}
		return a == b
	}
	compared := 0
	for key, want := range file.Values {
		got, ok := env["ASP_"+strings.ToUpper(key)]
		if !ok {
			continue
		}
		compared++
		if !same(got, want) {
			t.Errorf("%s: the package says %q and asp-server says %q", key, want, got)
		}
	}
	if compared < 8 {
		t.Fatalf("only %d settings are in both: are the files read right?", compared)
	}
	// And what the package gives every node, asp-server gives too.
	for _, key := range []string{"mtls", "reconcile", "tap_auto", "host_vsock", "egress_enforce", "egress_proxy_listen", "egress_dns_sink", "ch_socket_dir", "disk_dir", "cert_dir", "local_net_key_dir"} {
		if _, ok := file.Values[key]; !ok {
			t.Errorf("agent.yaml no longer gives %s: asp-server's defaults need a look", key)
			continue
		}
		if _, ok := env["ASP_"+strings.ToUpper(key)]; !ok {
			t.Errorf("agent.yaml gives %s and asp-server does not", key)
		}
	}
}
