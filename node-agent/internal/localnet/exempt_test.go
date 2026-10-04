package localnet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExemptSpecCoversHTTPAndDNSWithoutRedirect(t *testing.T) {
	tap := "asp-abcdef01"
	cmds := ExemptSpec(tap)
	if len(cmds) != 3 {
		t.Fatalf("got %d rules", len(cmds))
	}
	joined := strings.Join(joinCmds(cmds), "\n")
	for _, want := range []string{
		"nft insert rule ip asp_egress prerouting iifname " + tap + " tcp dport { 80, 443 } return",
		"nft insert rule ip asp_egress prerouting iifname " + tap + " udp dport 53 return",
		"nft insert rule ip asp_egress prerouting iifname " + tap + " tcp dport 53 return",
		"comment asp_ln_exempt_" + tap + "_http",
		"comment asp_ln_exempt_" + tap + "_udp53",
		"comment asp_ln_exempt_" + tap + "_tcp53",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "redirect") || strings.Contains(joined, "8888") || strings.Contains(joined, "5353") {
		t.Fatalf("exemption must not redirect:\n%s", joined)
	}
	if ExemptSpec("") != nil {
		t.Fatal("empty tap")
	}
}

func TestClearArgvRemovesRulesAndTable(t *testing.T) {
	p := Decide("abcdef012345", "owner-a", true, "withdrawn")
	got := strings.Join(joinCmds(ClearArgv(p)), "\n")
	for _, want := range []string{
		"ip link delete dev wg-asp-abcdef01",
		"ip rule del iif asp-abcdef01 lookup 13853 priority 13853",
		"ip rule del iif wg-asp-abcdef01 to 10.200.0.0/16 lookup 13853 priority 13853",
		"ip route flush table 13853",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "8888") || strings.Contains(got, "blackhole") {
		t.Fatalf("clear must not blackhole or proxy:\n%s", got)
	}
}

func TestHostExemptInstallAndClear(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv.log")
	nftState := filepath.Join(dir, "nft.rules")
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	ipScript := "#!/bin/sh\n" +
		"n=$0; n=${n##*/}\n" +
		"printf '%s' \"$n\" >> \"$ASP_MOCK_LOG\"\n" +
		"for a in \"$@\"; do printf ' %s' \"$a\" >> \"$ASP_MOCK_LOG\"; done\n" +
		"printf '\\n' >> \"$ASP_MOCK_LOG\"\n" +
		"exit 0\n"
	nftScript := "#!/bin/sh\n" +
		"PATH=\"/bin:/usr/bin:$PATH\"\n" +
		"export PATH\n" +
		"printf '%s' nft >> \"$ASP_MOCK_LOG\"\n" +
		"for a in \"$@\"; do printf ' %s' \"$a\" >> \"$ASP_MOCK_LOG\"; done\n" +
		"printf '\\n' >> \"$ASP_MOCK_LOG\"\n" +
		"if [ \"$1\" = \"list\" ]; then\n" +
		"  if [ -f \"$ASP_MOCK_NFT\" ]; then cat \"$ASP_MOCK_NFT\"; fi\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = \"-a\" ]; then\n" +
		"  if [ -f \"$ASP_MOCK_NFT\" ]; then\n" +
		"    i=1\n" +
		"    while IFS= read -r line; do\n" +
		"      printf '%s # handle %s\\n' \"$line\" \"$i\"\n" +
		"      i=$((i+1))\n" +
		"    done < \"$ASP_MOCK_NFT\"\n" +
		"  fi\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = \"insert\" ]; then\n" +
		"  printf '%s\\n' \"$*\" >> \"$ASP_MOCK_NFT\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = \"delete\" ]; then\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 0\n"
	for _, name := range []string{"ip", "wg"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(ipScript), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "nft"), []byte(nftScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASP_MOCK_LOG", logPath)
	t.Setenv("ASP_MOCK_NFT", nftState)
	t.Setenv("PATH", bin)

	h := NewHost(filepath.Join(dir, "keys"))
	id := "abcdef012345"
	up := Decide(id, "owner-a", true, "up")
	up.PeerPublic = "cHVibGljAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	if err := h.Apply(up); err != nil {
		t.Fatal(err)
	}
	log := string(mustRead(t, logPath))
	for _, want := range []string{
		"nft insert rule ip asp_egress prerouting iifname asp-abcdef01 tcp dport { 80, 443 } return comment asp_ln_exempt_asp-abcdef01_http",
		"nft insert rule ip asp_egress prerouting iifname asp-abcdef01 udp dport 53 return comment asp_ln_exempt_asp-abcdef01_udp53",
		"nft insert rule ip asp_egress prerouting iifname asp-abcdef01 tcp dport 53 return comment asp_ln_exempt_asp-abcdef01_tcp53",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("missing %q\n%s", want, log)
		}
	}
	if strings.Contains(log, "redirect") || strings.Contains(log, "8888") {
		t.Fatalf("redirect in log:\n%s", log)
	}

	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.Apply(up); err != nil {
		t.Fatal(err)
	}
	log = string(mustRead(t, logPath))
	if strings.Count(log, "nft insert rule") != 0 {
		t.Fatalf("second apply reinserted exemptions:\n%s", log)
	}

	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	down := Decide(id, "owner-a", true, "withdrawn")
	if err := h.Apply(down); err != nil {
		t.Fatal(err)
	}
	log = string(mustRead(t, logPath))
	if strings.Contains(log, "nft delete rule") {
		t.Fatalf("blackhole cleared the exemption (silent proxy fallback):\n%s", log)
	}
	if strings.Count(log, "nft insert rule") != 0 {
		t.Fatalf("blackhole reinserted:\n%s", log)
	}

	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.Clear(id); err != nil {
		t.Fatal(err)
	}
	log = string(mustRead(t, logPath))
	for _, handle := range []string{"handle 1", "handle 2", "handle 3"} {
		if !strings.Contains(log, "nft delete rule ip asp_egress prerouting "+handle) {
			t.Fatalf("missing delete %s\n%s", handle, log)
		}
	}
}

func TestHostPublicDoesNotExempt(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv.log")
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s' \"$0\" >> \"$ASP_MOCK_LOG\"\nfor a in \"$@\"; do printf ' %s' \"$a\" >> \"$ASP_MOCK_LOG\"; done\nprintf '\\n' >> \"$ASP_MOCK_LOG\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "nft"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASP_MOCK_LOG", logPath)
	t.Setenv("PATH", bin)
	h := NewHost(filepath.Join(dir, "keys"))
	pub := Decide("abcdef012345", "owner-a", false, "off")
	if err := h.Apply(pub); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		b, _ := os.ReadFile(logPath)
		t.Fatalf("public plan touched nft:\n%s", b)
	}
}
