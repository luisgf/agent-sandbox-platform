package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"--version"}, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "asp-server ") {
		t.Fatalf("exit %d, out %q, err %q", code, out.String(), errb.String())
	}
}

func TestBadArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--profile", "production"},
		{"--listen", "8443"},
		{"extra"},
		{"--no-such-flag"},
	} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), args, &out, &errb); code != 2 {
			t.Errorf("%v: exit %d, want 2 (%s)", args, code, errb.String())
		}
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

// The settings file gives the flags the command line left alone; the command line wins; a key that
// is no flag stops the start.
func TestSettingsFile(t *testing.T) {
	dir := t.TempDir()
	conf := writeConf(t, dir, "standalone.yaml", "listen: 0.0.0.0:9443\ntls_san: [asp.example, 198.51.100.9]\nno_agent: true\nprofile: lab\n")
	writeConf(t, dir, "standalone.yaml.d/10-site.yaml", "node_id: from-drop-in\ntls_san: [other.example]\n")

	fs, opts, sans := newFlags(nil)
	if err := fs.Parse([]string{"--config", conf, "--profile", "default"}); err != nil {
		t.Fatal(err)
	}
	if err := applyConfigFile(fs, conf); err != nil {
		t.Fatal(err)
	}
	if opts.Listen != "0.0.0.0:9443" || !opts.NoAgent || opts.NodeID != "from-drop-in" {
		t.Errorf("file values: %+v", opts)
	}
	if opts.Profile != "default" {
		t.Errorf("the command line lost to the file: profile %q", opts.Profile)
	}
	if strings.Join(*sans, ",") != "other.example" {
		t.Errorf("tls_san: %v (a drop-in replaces the list)", *sans)
	}

	bad := writeConf(t, dir, "bad.yaml", "listn: x\n")
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"--config", bad}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "did you mean listen") {
		t.Errorf("a key that is no setting: exit %d, %s", code, errb.String())
	}
	if code := run(context.Background(), []string{"--config", filepath.Join(dir, "none.yaml")}, &out, &errb); code != 2 {
		t.Errorf("a named file that is missing: exit %d", code)
	}
}
