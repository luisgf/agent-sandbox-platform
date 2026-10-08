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

// Only HTTP(S) and DNS are redirected; every other packet a guest sends must
// be dropped, or the allowlist is bypassed on any other port.
func TestDryRunDeniesEverythingElse(t *testing.T) {
	root := findRepoRoot(t)
	script := filepath.Join(root, "scripts", "nftables-egress-redirect.sh")
	out, err := DryRun(Config{
		ScriptPath:  script,
		GuestSubnet: "10.66.0.0/16",
		ProxyPort:   8888,
		DNSSinkPort: 5353,
		DNSAction:   "redirect",
		Table:       "asp_egress",
		Mode:        ModeSoft,
	})
	if err != nil {
		t.Fatal(err)
	}
	checks := []string{
		// guest → other hosts, other ports and other guests
		`iifname "asp-*" drop comment "asp_guest_forward_drop"`,
		`ip saddr 10.66.0.0/16 drop comment "asp_guest_forward_drop"`,
		// guest → host: only proxy and DNS sink
		`iifname "asp-*" tcp dport 8888 accept`,
		`iifname "asp-*" udp dport 5353 accept`,
		`iifname "asp-*" drop comment "asp_guest_to_host_drop"`,
		// source must be the /30 of the guest's own TAP
		`iifname "asp-*" fib saddr . iif oif missing drop`,
		`iifname "asp-*" ip saddr != 10.66.0.0/16 drop`,
		// local-net sessions keep their own tunnel
		`iifname "asp-*" oifname "wg-asp-*" accept`,
		"table ip6 asp_egress",
	}
	for _, want := range checks {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in ruleset:\n%s", want, out)
		}
	}
	// The local-net accept must come before the catch-all drop.
	if strings.Index(out, `oifname "wg-asp-*" accept`) > strings.Index(out, `iifname "asp-*" drop comment "asp_guest_forward_drop"`) {
		t.Fatal("local-net accept is after the guest forward drop")
	}
}

// The proxy and the DNS sink are bound to every address, so the table decides who reaches
// them: the guests (their TAPs) and the node itself (loopback), nobody else, over IPv4 and
// IPv6. The drops have to come after the guest accepts, or the guests would be cut off too.
func TestDryRunProxyAndSinkAreForGuestsOnly(t *testing.T) {
	root := findRepoRoot(t)
	script := filepath.Join(root, "scripts", "nftables-egress-redirect.sh")
	for _, action := range []string{"redirect", "drop"} {
		out, err := DryRun(Config{
			ScriptPath:  script,
			GuestSubnet: "10.66.0.0/16",
			ProxyPort:   8888,
			DNSSinkPort: 5353,
			DNSAction:   action,
			Table:       "asp_egress",
			Mode:        ModeSoft,
		})
		if err != nil {
			t.Fatal(err)
		}
		ip4, ip6, found := strings.Cut(out, "table ip6 ")
		if !found {
			t.Fatalf("no ip6 table:\n%s", out)
		}
		for family, rules := range map[string]string{"ip": ip4, "ip6": ip6} {
			for _, want := range []string{
				`iifname "lo" accept comment "asp_loopback"`,
				`tcp dport 8888 drop comment "asp_proxy_guests_only"`,
				`tcp dport 5353 drop comment "asp_dns_sink_guests_only"`,
				`udp dport 5353 drop comment "asp_dns_sink_guests_only"`,
			} {
				if !strings.Contains(rules, want) {
					t.Errorf("dns action %s, %s table: missing %q:\n%s", action, family, want, out)
				}
			}
			// Loopback first; then the guest rules; the guard only for what is left.
			loopback := strings.Index(rules, `iifname "lo" accept`)
			guestDrop := strings.Index(rules, `iifname "asp-*" drop comment "asp_guest_to_host_drop"`)
			guard := strings.Index(rules, `tcp dport 8888 drop comment "asp_proxy_guests_only"`)
			if !(loopback >= 0 && loopback < guestDrop && guestDrop < guard) {
				t.Errorf("dns action %s, %s table: the order has to be loopback, guests, then the guard (%d, %d, %d):\n%s", action, family, loopback, guestDrop, guard, out)
			}
		}
		if accept := strings.Index(ip4, `iifname "asp-*" tcp dport 8888 accept`); accept < 0 || accept > strings.Index(ip4, `tcp dport 8888 drop comment "asp_proxy_guests_only"`) {
			t.Errorf("dns action %s: the guests' accept of the proxy port has to come before the guard:\n%s", action, out)
		}
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

// With no script named, the node-agent applies the copy built into its binary,
// so a host needs no file installed for enforcement to work; it renders the same
// ruleset as the script in the repository.
func TestEmbeddedScriptIsUsedWithoutAPath(t *testing.T) {
	t.Setenv("ASP_NFT_SCRIPT", "")
	cfg := Config{GuestSubnet: "10.66.0.0/16", ProxyPort: 8888, DNSSinkPort: 5353, Table: "asp_egress", Mode: ModeEnforce}
	embedded, err := DryRun(cfg)
	if err != nil {
		t.Fatalf("embedded dry-run: %v\n%s", err, embedded)
	}
	if !strings.Contains(embedded, "table ip asp_egress") || !strings.Contains(embedded, "redirect to :8888") {
		t.Fatalf("embedded ruleset:\n%s", embedded)
	}
	cfg.ScriptPath = filepath.Join(findRepoRoot(t), "scripts", "nftables-egress-redirect.sh")
	onDisk, err := DryRun(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk != embedded {
		t.Fatalf("the script in scripts/ and the embedded one render different rules:\n--- scripts/\n%s\n--- embedded\n%s", onDisk, embedded)
	}
}

// A script the operator names and that is not there is an error, not a quiet
// fall back to another script. In soft mode it is the same warning as any other
// reason the rules could not be applied, and the caller is told they are not.
func TestMissingOperatorScript(t *testing.T) {
	t.Setenv("ASP_NFT_SCRIPT", "")
	if _, err := ApplyChecked(Config{ScriptPath: "/nonexistent/nft.sh", Mode: ModeEnforce}); err == nil {
		t.Fatal("enforce with a missing script returned no error")
	}
	applied, err := ApplyChecked(Config{ScriptPath: "/nonexistent/nft.sh", Mode: ModeSoft})
	if err != nil || applied {
		t.Fatalf("soft with a missing script: applied=%v err=%v", applied, err)
	}
	t.Setenv("ASP_NFT_SCRIPT", "/nonexistent/other.sh")
	if _, err := DryRun(Config{}); err == nil {
		t.Fatal("ASP_NFT_SCRIPT pointing nowhere was ignored")
	}
}

// Soft mode lets a node without root or nft go on, but says the rules are not in
// place, so the node does not report that it enforces egress.
func TestApplyCheckedSaysWhenSoftFailDidNotApply(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: the rules would be applied")
	}
	t.Setenv("ASP_NFT_SCRIPT", "")
	applied, err := ApplyChecked(Config{Mode: ModeSoft, ProxyPort: 8888, Table: "asp_unit_test"})
	if err != nil || applied {
		t.Fatalf("soft without root: applied=%v err=%v", applied, err)
	}
	if applied, err := ApplyChecked(Config{Mode: ModeEnforce, ProxyPort: 8888, Table: "asp_unit_test"}); err == nil || applied {
		t.Fatalf("enforce without root: applied=%v err=%v", applied, err)
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
