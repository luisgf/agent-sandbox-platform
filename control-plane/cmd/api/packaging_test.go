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

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", path))
	if err != nil {
		t.Skipf("%s is not here (a checkout of the control plane alone): %v", path, err)
	}
	return string(b)
}

// uncommented turns the example lines (#key: value, no space after the #) of a settings file
// into settings, and leaves the prose comments as they are.
func uncommented(src string) string {
	return regexp.MustCompile(`(?m)^#([a-z][a-z0-9_]*:)`).ReplaceAllString(src, "$1")
}

// The files the package installs and the lab's are accepted by the control plane, examples
// included: a key it does not have (a setting that was renamed, or never existed) stops it at
// its first start.
func TestPackagedSettingsFilesAreAcceptedByTheControlPlane(t *testing.T) {
	for _, path := range []string{"packaging/etc/server.yaml", "scripts/systemd/lab/server.yaml"} {
		src := readRepoFile(t, path)
		for name, text := range map[string]string{"as installed": src, "with its examples on": uncommented(src)} {
			clearSettings(t)
			conf := writeConf(t, t.TempDir(), "server.yaml", text)
			if err := loadConfigFile([]string{"--config", conf}); err != nil {
				t.Errorf("%s (%s): %v", path, name, err)
			}
		}
	}
}

// The keys of the CA, the OIDC key and the attestation key are in the directory the unit makes
// for them: the unit's StateDirectory and the file are two halves of one setting.
func TestPackagedStatePathsAreInTheUnitsStateDirectory(t *testing.T) {
	unit := readRepoFile(t, "packaging/systemd/asp-control-plane.service")
	m := regexp.MustCompile(`(?m)^StateDirectory=(\S+)$`).FindStringSubmatch(unit)
	if m == nil {
		t.Fatal("the unit has no StateDirectory")
	}
	dir := "/var/lib/" + m[1] + "/"
	clearSettings(t)
	conf := writeConf(t, t.TempDir(), "server.yaml", readRepoFile(t, "packaging/etc/server.yaml"))
	if err := loadConfigFile([]string{"--config", conf}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ASP_CA_CERT", "ASP_CA_KEY", "ASP_OIDC_KEY", "ASP_ATTEST_KEY"} {
		if got := os.Getenv(name); !strings.HasPrefix(got, dir) {
			t.Errorf("%s = %q, want a file under %s (the unit's StateDirectory)", name, got, dir)
		}
	}
}

// The units start the control plane with its file, and set no variable that is a setting: an
// environment variable beats the file, so one in the unit could not be changed there. (The lab
// unit keeps its secrets in an EnvironmentFile, outside the repository.)
func TestControlPlaneUnitsReadTheFileAndSetNoSettings(t *testing.T) {
	table := map[string]bool{}
	for _, s := range settingsTable {
		table[s.Env] = true
	}
	envLine := regexp.MustCompile(`(?m)^Environment=(ASP_[A-Z0-9_]+)=`)
	for _, c := range []struct{ path, exec string }{
		{"packaging/systemd/asp-control-plane.service", `/usr/bin/asp-control-plane`},
		{"scripts/systemd/asp-control-plane.service", `\S*/api`},
	} {
		src := readRepoFile(t, c.path)
		if !regexp.MustCompile(`(?m)^ExecStart=` + c.exec + ` --config /etc/asp/server\.yaml$`).MatchString(src) {
			t.Errorf("%s: ExecStart does not run the control plane with --config /etc/asp/server.yaml", c.path)
		}
		for _, m := range envLine.FindAllStringSubmatch(src, -1) {
			if table[m[1]] {
				t.Errorf("%s sets %s, a setting: it belongs in the YAML file (the environment would beat it)", c.path, m[1])
			}
		}
	}
}

// What the installer writes into server.yaml.d is accepted by the control plane.
func TestInstallerServerDropInsNameRealSettings(t *testing.T) {
	script := readRepoFile(t, "scripts/install.sh")
	start, end := strings.Index(script, "if has_role server; then"), strings.Index(script, "if has_role agent; then")
	if start < 0 || end < start {
		t.Fatal("the installer's server section is not where it was")
	}
	body := strings.Builder{}
	for _, m := range regexp.MustCompile(`(?m)^([a-z][a-z0-9_]*): `).FindAllStringSubmatch(script[start:end], -1) {
		body.WriteString(m[1] + ": x\n")
	}
	if body.Len() == 0 {
		t.Fatal("no setting found in the installer's server section: is it read right?")
	}
	clearSettings(t)
	conf := writeConf(t, t.TempDir(), "10-install.yaml", body.String())
	if err := loadConfigFile([]string{"--config", conf}); err != nil {
		t.Errorf("the installer writes a key the control plane does not have: %v", err)
	}
}
