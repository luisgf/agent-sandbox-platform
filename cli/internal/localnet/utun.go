package localnet

import (
	"fmt"
	"github.com/luisgf/agent-sandbox-platform/cli/internal/envcfg"
	"hash/fnv"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// TargetGOOS is the client OS the recipe is built for.
// ASP_LOCAL_NET_OS=darwin writes the macOS utun script even when asp itself
// is running on Linux (the API handshake stays on the host that has a token).
// A darwin recipe is never applied on a non-darwin kernel.
func TargetGOOS() string {
	switch strings.TrimSpace(os.Getenv("ASP_LOCAL_NET_OS")) {
	case "darwin", "linux":
		return strings.TrimSpace(os.Getenv("ASP_LOCAL_NET_OS"))
	default:
		return runtime.GOOS
	}
}

func targetGOOS() string { return TargetGOOS() }

var utunName = regexp.MustCompile(`^utun[0-9]+$`)

// darwinTun maps the Linux device name (wg-asp-<id>) to a utun wireguard-go accepts.
// Numbers start at 80 so we do not take the Mac's existing utun0-utun8.
func darwinTun(linuxIface string) string {
	linuxIface = strings.TrimSpace(linuxIface)
	// ASP_LOCAL_NET_UTUN retargets an existing utun (the Mac already has one up).
	if v := strings.TrimSpace(os.Getenv("ASP_LOCAL_NET_UTUN")); utunName.MatchString(v) {
		return v
	}
	if utunName.MatchString(linuxIface) {
		return linuxIface
	}
	if linuxIface == "" {
		linuxIface = "wg-asp-sandbox"
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(linuxIface))
	n := 80 + int(h.Sum32()%80)
	return fmt.Sprintf("utun%d", n)
}

func (d ClientDevice) darwinArgv() []cmd {
	if strings.TrimSpace(d.Iface) == "" {
		return nil
	}
	tun := darwinTun(d.Iface)
	var cmds []cmd
	cmds = append(cmds, cmd{name: "wireguard-go", args: []string{tun}})
	if d.KeyPath != "" && d.NodePublic != "" {
		args := []string{"set", tun, "private-key", d.KeyPath, "peer", d.NodePublic, "allowed-ips", "0.0.0.0/0,::/0"}
		if d.Endpoint != "" {
			args = append(args, "endpoint", d.Endpoint, "persistent-keepalive", "25")
		}
		cmds = append(cmds, cmd{name: "wg", args: args})
	}
	if ip, mask, err := parseV4CIDR(d.Address); err == nil && ip != "" {
		cmds = append(cmds, cmd{name: "ifconfig", args: []string{tun, "inet", ip, ip, "netmask", mask, "up"}})
	}
	// Return path for guest sources and the tunnel prefix. Not a default route.
	cmds = append(cmds,
		cmd{name: "route", args: []string{"-n", "add", "-net", "10.200.0.0", "-netmask", "255.255.0.0", "-interface", tun}},
		cmd{name: "route", args: []string{"-n", "add", "-net", "10.188.0.0", "-netmask", "255.255.0.0", "-interface", tun}},
	)
	return cmds
}

func parseV4CIDR(cidr string) (string, string, error) {
	cidr = strings.TrimSpace(cidr)
	if cidr == "" {
		return "", "", fmt.Errorf("empty address")
	}
	if !strings.Contains(cidr, "/") {
		ip := net.ParseIP(cidr).To4()
		if ip == nil {
			return "", "", fmt.Errorf("tunnel address must be ipv4")
		}
		return ip.String(), "255.255.255.252", nil
	}
	ipa, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", "", err
	}
	v4 := ipa.To4()
	if v4 == nil {
		return "", "", fmt.Errorf("tunnel address must be ipv4")
	}
	mask := net.IP(n.Mask).To4()
	if mask == nil {
		return "", "", fmt.Errorf("tunnel mask must be ipv4")
	}
	return v4.String(), mask.String(), nil
}

func canApplyDarwin() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	switch strings.TrimSpace(os.Getenv("ASP_LOCAL_NET_APPLY")) {
	case "0", "false", "no":
		return false
	}
	if _, err := exec.LookPath("wg"); err != nil {
		return false
	}
	if _, err := exec.LookPath("wireguard-go"); err != nil {
		return false
	}
	if envcfg.Truthy("ASP_LOCAL_NET_APPLY") {
		return true
	}
	if os.Geteuid() == 0 {
		return true
	}
	// sudo -n exits immediately when a password is required. Never prompt.
	cmd := exec.Command("sudo", "-n", "true")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}

func bringUpDarwin(stderr io.Writer, d ClientDevice) (bool, error) {
	if d.hijacks() {
		return false, fmt.Errorf("refusing local-net client recipe that changes the host default route")
	}
	if msg := d.NotPublished(); msg != "" {
		fmt.Fprintf(stderr, "asp: local-net device not applied: %s. Re-run local-net up.\n", msg)
		return false, nil
	}
	upPath, err := writeDarwinScripts(d)
	if err != nil {
		return false, err
	}
	body, err := os.ReadFile(upPath)
	if err != nil {
		return false, err
	}
	if scriptHijacks(string(body)) {
		return false, fmt.Errorf("refusing local-net client recipe that changes the host default route")
	}
	if !canApplyDarwin() {
		// One ready-to-run command. Do not print an ip(8) recipe.
		fmt.Fprintf(stderr, "sudo %s\n", upPath)
		return false, nil
	}
	var runErr error
	if os.Geteuid() == 0 {
		runErr = run(cmd{name: upPath}, false)
	} else {
		runErr = run(cmd{name: "sudo", args: []string{"-n", upPath}}, false)
	}
	if runErr != nil {
		return false, runErr
	}
	fmt.Fprintf(stderr, "asp: local-net darwin applied %s (no host default route)\n", darwinTun(d.Iface))
	return true, nil
}

func tearDownDarwin(stderr io.Writer, iface, sessionPath string) error {
	tun := darwinTun(iface)
	path := darwinDownScriptPath(sessionPath)
	if path == "" || !fileExists(path) {
		var err error
		path, err = writeDarwinDownOnly(tun, sessionPath)
		if err != nil {
			return err
		}
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if scriptHijacks(string(body)) {
		return fmt.Errorf("refusing local-net client recipe that changes the host default route")
	}
	if !canApplyDarwin() {
		if stderr != nil {
			fmt.Fprintf(stderr, "sudo %s\n", path)
		}
		return nil
	}
	if os.Geteuid() == 0 {
		return run(cmd{name: path}, false)
	}
	return run(cmd{name: "sudo", args: []string{"-n", path}}, false)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func darwinUpScriptPath(sessionOrKey string) string {
	sessionOrKey = strings.TrimSpace(sessionOrKey)
	if sessionOrKey == "" {
		return ""
	}
	base := strings.TrimSuffix(sessionOrKey, ".local-net.key")
	return base + ".local-net.darwin-up.sh"
}

func darwinDownScriptPath(sessionOrKey string) string {
	sessionOrKey = strings.TrimSpace(sessionOrKey)
	if sessionOrKey == "" {
		return ""
	}
	base := strings.TrimSuffix(sessionOrKey, ".local-net.key")
	base = strings.TrimSuffix(base, ".local-net.darwin-up.sh")
	return base + ".local-net.darwin-down.sh"
}

func writeDarwinScripts(d ClientDevice) (string, error) {
	up := darwinUpScriptPath(d.KeyPath)
	if up == "" {
		return "", fmt.Errorf("darwin local-net script: key path is empty")
	}
	tun := darwinTun(d.Iface)
	upBody, err := renderDarwinUp(d, tun)
	if err != nil {
		return "", err
	}
	if scriptHijacks(upBody) {
		return "", fmt.Errorf("refusing local-net client recipe that changes the host default route")
	}
	if err := writeScript(up, upBody); err != nil {
		return "", err
	}
	downBody := renderDarwinDown(tun)
	if scriptHijacks(downBody) {
		return "", fmt.Errorf("refusing local-net client recipe that changes the host default route")
	}
	if err := writeScript(darwinDownScriptPath(d.KeyPath), downBody); err != nil {
		return "", err
	}
	return up, nil
}

func writeDarwinDownOnly(tun, sessionPath string) (string, error) {
	path := darwinDownScriptPath(sessionPath)
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, ".cache", "asp", "local-net-"+tun+"-down.sh")
	}
	body := renderDarwinDown(tun)
	if scriptHijacks(body) {
		return "", fmt.Errorf("refusing local-net client recipe that changes the host default route")
	}
	if err := writeScript(path, body); err != nil {
		return "", err
	}
	return path, nil
}

func writeScript(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func shQuote(s string) (string, error) {
	if strings.ContainsAny(s, "'\n\r") {
		return "", fmt.Errorf("refusing shell metacharacter in local-net value")
	}
	return "'" + s + "'", nil
}

const shellDollar = "$"

func shellSub(expr string) string { return shellDollar + "(" + expr + ")" }

func renderDarwinUp(d ClientDevice, tun string) (string, error) {
	if !utunName.MatchString(tun) {
		return "", fmt.Errorf("darwin iface %q is not a utun", tun)
	}
	peer, err := shQuote(d.NodePublic)
	if err != nil {
		return "", err
	}
	endpoint := ""
	if strings.TrimSpace(d.Endpoint) != "" {
		endpoint, err = shQuote(d.Endpoint)
		if err != nil {
			return "", err
		}
	}
	ip, mask, err := parseV4CIDR(d.Address)
	if err != nil {
		return "", err
	}
	ipQ, err := shQuote(ip)
	if err != nil {
		return "", err
	}
	maskQ, err := shQuote(mask)
	if err != nil {
		return "", err
	}
	tunQ, err := shQuote(tun)
	if err != nil {
		return "", err
	}
	wgSet := "\"" + shellDollar + "WG\" set \"" + shellDollar + "IFACE\" private-key \"" + shellDollar + "KEY\" peer " + peer + " allowed-ips \"0.0.0.0/0,::/0\""
	if endpoint != "" {
		wgSet += " endpoint " + endpoint + " persistent-keepalive 25"
	}
	var b strings.Builder
	line := func(s string) { b.WriteString(s); b.WriteByte('\n') }
	line("#!/bin/sh")
	line("# asp local-net up. Userspace WireGuard on a utun.")
	line("# AllowedIPs is cryptokey routing only. This script does not add a default route.")
	line("# If the tunnel drops, the node blackholes that session. There is no fallback.")
	line("set -eu")
	line("if [ \"" + shellSub("id -u") + "\" -ne 0 ]; then")
	line("  echo \"asp: re-run this script with sudo\" >&2")
	line("  exit 1")
	line("fi")
	line("PATH=\"/opt/homebrew/bin:/usr/local/bin:" + shellDollar + "{PATH}\"")
	line("export PATH")
	line("WG=" + shellSub("command -v wg") + " || { echo \"asp: wg not on PATH\" >&2; exit 1; }")
	line("GO=" + shellSub("command -v wireguard-go") + " || { echo \"asp: wireguard-go not on PATH (brew install wireguard-go)\" >&2; exit 1; }")
	line("IFACE=" + tunQ)
	line("cd \"" + shellSub("dirname \""+shellDollar+"0\"") + "\" || exit 1")
	line("KEY=\"" + shellSub("pwd") + "/" + shellSub("basename \""+shellDollar+"0\" .local-net.darwin-up.sh") + ".local-net.key\"")
	line("if [ ! -f \"$KEY\" ]; then")
	line("  echo \"asp: missing key file next to this script\" >&2")
	line("  exit 1")
	line("fi")
	line("if ! ifconfig \"$IFACE\" >/dev/null 2>&1; then")
	line("  \"$GO\" \"$IFACE\"")
	line("  i=0")
	line("  while ! ifconfig \"$IFACE\" >/dev/null 2>&1; do")
	line("    i=" + shellSub("(i+1)"))
	line("    if [ \"$i\" -gt 50 ]; then")
	line("      echo \"asp: $IFACE did not appear\" >&2")
	line("      exit 1")
	line("    fi")
	line("    sleep 0.1")
	line("  done")
	line("fi")
	line(wgSet)
	line("ifconfig \"$IFACE\" inet " + ipQ + " " + ipQ + " netmask " + maskQ + " up")
	line("# Guest return path (10.200.0.0/16) and the tunnel prefix (10.188.0.0/16).")
	line("# These are not the Mac main-table default route.")
	line("route -n delete -net 10.200.0.0 -netmask 255.255.0.0 -interface \"$IFACE\" >/dev/null 2>&1 || true")
	line("route -n add -net 10.200.0.0 -netmask 255.255.0.0 -interface \"$IFACE\"")
	line("route -n delete -net 10.188.0.0 -netmask 255.255.0.0 -interface \"$IFACE\" >/dev/null 2>&1 || true")
	line("route -n add -net 10.188.0.0 -netmask 255.255.0.0 -interface \"$IFACE\"")
	line("sysctl -w net.inet.ip.forwarding=1 >/dev/null")
	line("UPLINK=" + shellSub("route -n get default | awk '/interface:/{print "+shellDollar+"2; exit}'"))
	line("if [ -z \"$UPLINK\" ] || [ \"$UPLINK\" = \"$IFACE\" ]; then")
	line("  echo \"asp: refusing to NAT; default uplink is missing or is the tunnel\" >&2")
	line("  exit 1")
	line("fi")
	line("pfctl -E >/dev/null 2>&1 || true")
	line("# /etc/pf.conf only names nat-anchor/anchor \"com.apple/*\".")
	line("# pfctl -a com.asp/$IFACE loads rules the main ruleset never evaluates.")
	line("# com.apple/asp-$IFACE is one level under that wildcard, so nat and pass run.")
	line("# Do not reload /etc/pf.conf: that flushes anchors other services inserted.")
	line("if ! grep -F -q 'nat-anchor \"com.apple/*\"' /etc/pf.conf || ! grep -F -q 'anchor \"com.apple/*\"' /etc/pf.conf; then")
	line("  echo \"asp: /etc/pf.conf does not evaluate com.apple/*; refusing to NAT\" >&2")
	line("  exit 1")
	line("fi")
	line("ANCHOR=\"com.apple/asp-" + shellDollar + "{IFACE}\"")
	line("pfctl -a \"$ANCHOR\" -f - <<ENDPF")
	line("nat pass on $UPLINK inet from 10.200.0.0/16 to any -> ($UPLINK)")
	line("nat pass on $UPLINK inet from 10.188.0.0/16 to any -> ($UPLINK)")
	line("pass in quick on $IFACE inet from 10.200.0.0/16 to any keep state")
	line("pass in quick on $IFACE inet from 10.188.0.0/16 to any keep state")
	line("pass out quick on $IFACE inet to 10.200.0.0/16 keep state")
	line("pass out quick on $IFACE inet to 10.188.0.0/16 keep state")
	line("ENDPF")
	line("echo \"asp: local-net darwin up $IFACE uplink $UPLINK (no default route change)\"")
	return b.String(), nil
}

func renderDarwinDown(tun string) string {
	var b strings.Builder
	line := func(s string) { b.WriteString(s); b.WriteByte('\n') }
	line("#!/bin/sh")
	line("# asp local-net down. Removes only this utun, its return routes, and its pf anchor.")
	line("# Does not change the Mac main-table default route and does not add a fallback.")
	line("set -eu")
	line("if [ \"" + shellSub("id -u") + "\" -ne 0 ]; then")
	line("  echo \"asp: re-run this script with sudo\" >&2")
	line("  exit 1")
	line("fi")
	line("PATH=\"/opt/homebrew/bin:/usr/local/bin:" + shellDollar + "{PATH}\"")
	line("export PATH")
	line("IFACE='" + tun + "'")
	line("if command -v pkill >/dev/null 2>&1; then")
	line("  pkill -f \"wireguard-go $IFACE\" >/dev/null 2>&1 || true")
	line("fi")
	line("if ifconfig \"$IFACE\" >/dev/null 2>&1; then")
	line("  ifconfig \"$IFACE\" down >/dev/null 2>&1 || true")
	line("  ifconfig \"$IFACE\" destroy >/dev/null 2>&1 || true")
	line("fi")
	line("route -n delete -net 10.200.0.0 -netmask 255.255.0.0 -interface \"$IFACE\" >/dev/null 2>&1 || true")
	line("route -n delete -net 10.188.0.0 -netmask 255.255.0.0 -interface \"$IFACE\" >/dev/null 2>&1 || true")
	line("pfctl -a \"com.apple/asp-$IFACE\" -F all >/dev/null 2>&1 || true")
	line("pfctl -a \"com.asp/$IFACE\" -F all >/dev/null 2>&1 || true")
	line("echo \"asp: local-net darwin down $IFACE\"")
	return b.String()
}

// scriptHijacks reports a shell recipe that would move the host default route
// or send traffic at the node proxy. Reading the current default (to learn the
// uplink for NAT) is allowed. A /1 split that shadows the default is not.
func scriptHijacks(body string) bool {
	if strings.Contains(body, "8888") || strings.Contains(body, "asp_egress") {
		return true
	}
	for _, line := range strings.Split(body, "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		if strings.Contains(s, "0.0.0.0/1") || strings.Contains(s, "128.0.0.0/1") {
			return true
		}
		if !strings.Contains(s, "route") {
			continue
		}
		if strings.Contains(s, "get default") && !strings.Contains(s, " add ") && !strings.Contains(s, " change ") {
			continue
		}
		mutates := strings.Contains(s, " add ") || strings.Contains(s, " change ")
		if !mutates {
			continue
		}
		if strings.Contains(s, "default") || strings.Contains(s, "0.0.0.0/0") || strings.Contains(s, " 0.0.0.0 ") || strings.Contains(s, "::/0") {
			return true
		}
	}
	return false
}
