package localnet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxCommandsStillUseIPAndDoNotRouteDefault(t *testing.T) {
	d := ClientDevice{
		Iface:      "wg-asp-ln-1",
		KeyPath:    "/tmp/example.local-net.key",
		NodePublic: "ERERERERERERERERERERERERERERERERERERERERERE=",
		Endpoint:   "203.0.113.10:51024",
		Address:    "10.188.17.98/30",
	}
	var lines []string
	for _, c := range d.linuxCommands() {
		if cmdHijacks(c) {
			t.Fatalf("linux command hijacks: %s", c.line())
		}
		lines = append(lines, c.line())
	}
	text := strings.Join(lines, "\n")
	for _, want := range []string{
		"ip link add dev wg-asp-ln-1 type wireguard",
		"ip address add 10.188.17.98/30 dev wg-asp-ln-1",
		"wg set wg-asp-ln-1 private-key /tmp/example.local-net.key",
		"allowed-ips 0.0.0.0/0,::/0",
		"endpoint 203.0.113.10:51024",
		"ip link set wg-asp-ln-1 up",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q\n%s", want, text)
		}
	}
	if strings.Contains(text, " route ") || strings.Contains(text, "8888") {
		t.Fatalf("linux recipe routes or proxies:\n%s", text)
	}
}

func TestDarwinRecipeHasNoDefaultRouteAndNoIP(t *testing.T) {
	d := ClientDevice{
		Iface:      "wg-asp-ln-1",
		KeyPath:    "/tmp/s.json.local-net.key",
		NodePublic: "ERERERERERERERERERERERERERERERERERERERERERE=",
		Endpoint:   "203.0.113.10:51024",
		Address:    "10.188.17.98/30",
	}
	for _, c := range d.darwinArgv() {
		if c.name == "ip" {
			t.Fatalf("darwin argv uses ip: %s", c.line())
		}
		if cmdHijacks(c) {
			t.Fatalf("darwin argv hijacks: %s", c.line())
		}
	}
	body, err := renderDarwinUp(d, darwinTun(d.Iface))
	if err != nil {
		t.Fatal(err)
	}
	if scriptHijacks(body) {
		t.Fatalf("script hijacks:\n%s", body)
	}
	for _, want := range []string{"wireguard-go", "ifconfig", "pfctl", "private-key", "10.200.0.0", "10.188.0.0"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q\n%s", want, body)
		}
	}
	for _, bad := range []string{"\nip ", " ip ", "route add default", "route -n add default", "-net 0.0.0.0", "0.0.0.0/1", "wg-quick"} {
		if strings.Contains(body, bad) {
			t.Fatalf("bad %q\n%s", bad, body)
		}
	}
	if strings.Contains(body, d.NodePublic) && strings.Contains(body, "PrivateKey =") {
		t.Fatal("conf-style private key")
	}
	down := renderDarwinDown(darwinTun(d.Iface))
	if scriptHijacks(down) {
		t.Fatalf("down hijacks:\n%s", down)
	}
	if strings.Contains(down, "add default") || strings.Contains(down, " ip ") {
		t.Fatalf("down bad:\n%s", down)
	}
}

func TestDarwinBringUpPrintsOneSudoCommand(t *testing.T) {
	t.Setenv("ASP_LOCAL_NET_OS", "darwin")
	t.Setenv("ASP_LOCAL_NET_APPLY", "0")
	dir := t.TempDir()
	key := filepath.Join(dir, "s.json.local-net.key")
	if err := os.WriteFile(key, []byte("not-the-script\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := ClientDevice{
		Iface:      "wg-asp-ln-1",
		KeyPath:    key,
		NodePublic: "ERERERERERERERERERERERERERERERERERERERERERE=",
		Endpoint:   "203.0.113.10:51024",
		Address:    "10.188.17.98/30",
	}
	var stderr strings.Builder
	applied, err := BringUp(&stderr, d)
	if err != nil || applied {
		t.Fatalf("applied=%v err=%v stderr=%s", applied, err, stderr.String())
	}
	out := stderr.String()
	if strings.Count(out, "sudo ") != 1 {
		t.Fatalf("want one sudo command\n%s", out)
	}
	if strings.Contains(out, " ip ") || strings.Contains(out, "\nip ") {
		t.Fatalf("told the operator to run ip:\n%s", out)
	}
	up := filepath.Join(dir, "s.json.local-net.darwin-up.sh")
	fi, err := os.Stat(up)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("script mode %o", fi.Mode().Perm())
	}
	body, err := os.ReadFile(up)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "not-the-script") {
		t.Fatal("private key embedded in script")
	}
	if scriptHijacks(string(body)) {
		t.Fatalf("written script hijacks:\n%s", body)
	}
	down := filepath.Join(dir, "s.json.local-net.darwin-down.sh")
	if _, err := os.Stat(down); err != nil {
		t.Fatal(err)
	}
}

func TestScriptHijackRefusesDefaultAndAllowsUplinkRead(t *testing.T) {
	if !scriptHijacks("route -n add default -interface utun80\n") {
		t.Fatal("expected default add to be refused")
	}
	if !scriptHijacks("route -n add -net 0.0.0.0/1 -interface utun80\n") {
		t.Fatal("expected /1 split to be refused")
	}
	if scriptHijacks("UPLINK=$(route -n get default | awk '/interface:/{print $2}')\n") {
		t.Fatal("reading the default route must be allowed")
	}
	if !cmdHijacks(cmd{"route", []string{"-n", "add", "default", "-interface", "utun80"}}) {
		t.Fatal("argv default")
	}
	if cmdHijacks(cmd{"route", []string{"-n", "add", "-net", "10.200.0.0", "-netmask", "255.255.0.0", "-interface", "utun80"}}) {
		t.Fatal("guest return route is not a default route")
	}
}

func TestDarwinTunEnvRetargetsExisting(t *testing.T) {
	t.Setenv("ASP_LOCAL_NET_UTUN", "utun144")
	if got := darwinTun("wg-asp-other"); got != "utun144" {
		t.Fatalf("got %s", got)
	}
	t.Setenv("ASP_LOCAL_NET_UTUN", "not-a-utun")
	if got := darwinTun("wg-asp-ln-1"); got == "utun144" || !strings.HasPrefix(got, "utun") {
		t.Fatalf("got %s", got)
	}
}
