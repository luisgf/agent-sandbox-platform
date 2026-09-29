package nftredirect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDryRunRendersHTTPAndDNS(t *testing.T) {
	root := findRepoRoot(t)
	script := filepath.Join(root, "scripts", "nftables-egress-redirect.sh")
	out, err := DryRun(Config{
		ScriptPath:  script,
		GuestSubnet: "10.66.0.0/16",
		ProxyIP:     "10.66.0.1",
		ProxyPort:   8888,
		DNSSinkPort: 5353,
		HTTPPorts:   "80,443,8080",
		DNSAction:   "redirect",
		Table:       "asp_egress",
		Mode:        ModeSoft,
	})
	if err != nil {
		t.Fatal(err)
	}
	checks := []string{
		"table ip asp_egress",
		"10.66.0.0/16",
		"redirect to :8888",
		"tcp dport { 80,443,8080 }",
		"udp dport 53 redirect to :5353",
		"tcp dport 53 redirect to :5353",
	}
	for _, want := range checks {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in ruleset:\n%s", want, out)
		}
	}
}

func TestDryRunDNSDrop(t *testing.T) {
	root := findRepoRoot(t)
	script := filepath.Join(root, "scripts", "nftables-egress-redirect.sh")
	out, err := DryRun(Config{
		ScriptPath: script,
		DNSAction:  "drop",
		Table:      "asp_egress",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "udp dport 53 drop") || !strings.Contains(out, "tcp dport 53 drop") {
		t.Fatalf("expected DNS drop rules:\n%s", out)
	}
}

func TestApplySoftFailWithoutRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; SoftFail path not exercised")
	}
	root := findRepoRoot(t)
	script := filepath.Join(root, "scripts", "nftables-egress-redirect.sh")
	err := Apply(Config{
		ScriptPath: script,
		Mode:       ModeSoft,
		ProxyPort:  8888,
	})
	if err != nil {
		t.Fatalf("SoftFail should return nil, got %v", err)
	}
}

func TestApplyEnforceWithoutRootFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; Enforce failure path not exercised")
	}
	root := findRepoRoot(t)
	script := filepath.Join(root, "scripts", "nftables-egress-redirect.sh")
	err := Apply(Config{
		ScriptPath: script,
		Mode:       ModeEnforce,
		ProxyPort:  8888,
	})
	if err == nil {
		t.Fatal("Enforce without root should return error")
	}
}

func TestScriptDryRunCLI(t *testing.T) {
	root := findRepoRoot(t)
	script := filepath.Join(root, "scripts", "nftables-egress-redirect.sh")
	// Direct CLI sanity (no Go wrapper).
	cmdOut, err := DryRun(Config{ScriptPath: script, Mode: ModeEnforce, Table: "asp_egress"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmdOut, "asp_egress") {
		t.Fatalf("table missing:\n%s", cmdOut)
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := wd
	for i := 0; i < 8; i++ {
		if st, err := os.Stat(filepath.Join(dir, "scripts", "nftables-egress-redirect.sh")); err == nil && !st.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("repo root with scripts/nftables-egress-redirect.sh not found from", wd)
	return ""
}
