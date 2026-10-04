package localnet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParamsFrozenVector(t *testing.T) {
	p := Decide("abcdef012345", "owner-a", true, "up")
	if p.Iface != "wg-asp-abcdef01" || p.Tap != "asp-abcdef01" {
		t.Fatalf("names %+v", p)
	}
	if p.ListenPort != 51024 || p.TableID != 13853 {
		t.Fatalf("port=%d table=%d", p.ListenPort, p.TableID)
	}
	if p.NodeCIDR != "10.188.17.97/30" || p.ClientCIDR != "10.188.17.98/30" {
		t.Fatalf("cidr node=%s client=%s", p.NodeCIDR, p.ClientCIDR)
	}
	if HijacksHost(Argv(p)) {
		t.Fatalf("tunnel argv hijacks: %v", Commands(p))
	}
}

func TestArgvNeverHostDefaultOrProxy(t *testing.T) {
	up := Decide("abcdef012345", "o", true, "up")
	up.PeerPublic = "cHVibGljAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	up.KeyPath = "/tmp/not-a-secret-path.key"
	down := Decide("abcdef012345", "o", true, "withdrawn")
	for _, p := range []Plan{up, down} {
		cmds := Argv(p)
		if HijacksHost(cmds) {
			t.Fatalf("hijack %+v", Commands(p))
		}
		for _, c := range cmds {
			line := c.Name + " " + strings.Join(c.Args, " ")
			if strings.Contains(line, "8888") || strings.Contains(line, "asp_egress") {
				t.Fatalf("proxy in %s", line)
			}
			if c.Name == "ip" && contains(c.Args, "route") && contains(c.Args, "default") && !contains(c.Args, "table") {
				t.Fatalf("bare default route %s", line)
			}
		}
	}
	got := strings.Join(Commands(down), "\n")
	if !strings.Contains(got, "blackhole 0.0.0.0/0") || !strings.Contains(got, "link delete") {
		t.Fatalf("disconnect commands:\n%s", got)
	}
}

func TestHostApplyMockPath(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv.log")
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"n=$0; n=${n##*/}\n" +
		"printf '%s' \"$n\" >> \"$ASP_MOCK_LOG\"\n" +
		"for a in \"$@\"; do printf ' %s' \"$a\" >> \"$ASP_MOCK_LOG\"; done\n" +
		"printf '\\n' >> \"$ASP_MOCK_LOG\"\n" +
		"exit 0\n"
	for _, name := range []string{"ip", "wg"} {
		path := filepath.Join(bin, name)
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ASP_MOCK_LOG", logPath)
	t.Setenv("PATH", bin)

	h := NewHost(filepath.Join(dir, "keys"))
	id := "abcdef012345"
	up := Decide(id, "owner-a", true, "up")
	up.PeerPublic = "cHVibGljAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	if err := h.Apply(up); err != nil {
		t.Fatal(err)
	}
	pub, port, keyPath, err := h.EnsureNodeKey(id)
	if err != nil {
		t.Fatal(err)
	}
	if pub == "" || port != 51024 {
		t.Fatalf("pub=%q port=%d", pub, port)
	}
	fi, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %o", fi.Mode().Perm())
	}
	keyRaw, _ := os.ReadFile(keyPath)
	logRaw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logRaw)
	if strings.Contains(log, strings.TrimSpace(string(keyRaw))) {
		t.Fatal("private key leaked into argv")
	}
	for _, want := range []string{
		"ip link add dev wg-asp-abcdef01 type wireguard",
		"wg set wg-asp-abcdef01 listen-port 51024 private-key " + keyPath + " peer " + up.PeerPublic + " allowed-ips 0.0.0.0/0,::/0",
		"ip route replace default dev wg-asp-abcdef01 table 13853",
		"ip rule add iif asp-abcdef01 lookup 13853 priority 13853",
		"ip route replace 10.200.0.0/24 dev asp-abcdef01 table 13853",
		"ip rule add iif wg-asp-abcdef01 to 10.200.0.0/24 lookup 13853 priority 13853",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("missing %q\nlog:\n%s", want, log)
		}
	}
	if strings.Contains(log, "8888") || strings.Contains(log, "asp_egress") {
		t.Fatalf("proxy in log:\n%s", log)
	}

	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	down := Decide(id, "owner-a", true, "withdrawn")
	if err := h.Apply(down); err != nil {
		t.Fatal(err)
	}
	log = string(mustRead(t, logPath))
	if !strings.Contains(log, "ip link delete dev wg-asp-abcdef01") {
		t.Fatalf("no delete:\n%s", log)
	}
	if !strings.Contains(log, "ip route replace blackhole 0.0.0.0/0 table 13853") {
		t.Fatalf("no blackhole:\n%s", log)
	}
	if !strings.Contains(log, "ip rule del iif wg-asp-abcdef01 to 10.200.0.0/24 lookup 13853 priority 13853") {
		t.Fatalf("return rule not removed:\n%s", log)
	}
	if strings.Contains(log, "8888") || strings.Contains(log, "wg set") {
		t.Fatalf("disconnect still tunneled or proxied:\n%s", log)
	}
	cur, ok := h.Current(id)
	if !ok || cur.Kind != KindBlackhole || cur.UsePublicProxy {
		t.Fatalf("current %+v ok=%v", cur, ok)
	}

	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.Clear(id); err != nil {
		t.Fatal(err)
	}
	log = string(mustRead(t, logPath))
	if !strings.Contains(log, "ip link delete dev wg-asp-abcdef01") {
		t.Fatalf("clear:\n%s", log)
	}
	if strings.Contains(log, "8888") {
		t.Fatalf("clear proxy:\n%s", log)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("key remained: %v", err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
