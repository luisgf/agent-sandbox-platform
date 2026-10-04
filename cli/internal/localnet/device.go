package localnet

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// ClientDevice is the laptop side of one sandbox tunnel.
// AllowedIPs 0.0.0.0/0 is cryptokey routing only: nothing here installs a
// default route in the host main table.
type ClientDevice struct {
	Iface      string
	KeyPath    string
	NodePublic string
	Endpoint   string
	Address    string
}

// Endpoint joins a dial host with the node listen port when the dial has no port.
func Endpoint(dial string, port int) string {
	dial = strings.TrimSpace(dial)
	dial = strings.TrimPrefix(dial, "udp://")
	dial = strings.TrimPrefix(dial, "wg://")
	if dial == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(dial); err == nil {
		return dial
	}
	if port <= 0 {
		return dial
	}
	return net.JoinHostPort(dial, strconv.Itoa(port))
}

// Iface is the client device name for a sandbox id.
func Iface(sandboxID string) string {
	return "wg-asp-" + shortID(sandboxID)
}

func shortID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > 8 {
		id = id[:8]
	}
	if id == "" {
		id = "sandbox"
	}
	return id
}

type cmd struct {
	name string
	args []string
}

func (c cmd) line() string { return c.name + " " + strings.Join(c.args, " ") }

// Commands is the exact argv used when the device is applied.
// It does not route 0.0.0.0/0 in the main table and it does not mention :8888.
// On Darwin (or ASP_LOCAL_NET_OS=darwin) the argv is wireguard-go, wg, ifconfig
// and route. BringUp writes one sudo script; it never tells the operator to run ip(8).
func (d ClientDevice) Commands() []cmd {
	if targetGOOS() == "darwin" {
		return d.darwinArgv()
	}
	return d.linuxCommands()
}

func (d ClientDevice) linuxCommands() []cmd {
	var cmds []cmd
	if strings.TrimSpace(d.Iface) == "" {
		return nil
	}
	cmds = append(cmds, cmd{"ip", []string{"link", "delete", "dev", d.Iface}})
	cmds = append(cmds, cmd{"ip", []string{"link", "add", "dev", d.Iface, "type", "wireguard"}})
	if d.Address != "" {
		cmds = append(cmds, cmd{"ip", []string{"address", "add", d.Address, "dev", d.Iface}})
	}
	if d.KeyPath != "" && d.NodePublic != "" {
		args := []string{"set", d.Iface, "private-key", d.KeyPath, "peer", d.NodePublic, "allowed-ips", "0.0.0.0/0,::/0"}
		if d.Endpoint != "" {
			args = append(args, "endpoint", d.Endpoint, "persistent-keepalive", "25")
		}
		cmds = append(cmds, cmd{"wg", args})
	}
	cmds = append(cmds, cmd{"ip", []string{"link", "set", d.Iface, "up"}})
	return cmds
}

func (d ClientDevice) hijacks() bool {
	for _, c := range d.Commands() {
		if cmdHijacks(c) {
			return true
		}
	}
	return false
}

func cmdHijacks(c cmd) bool {
	line := c.line()
	if strings.Contains(line, "8888") || strings.Contains(line, "asp_egress") {
		return true
	}
	if c.name == "ip" && contains(c.args, "route") && (contains(c.args, "default") || contains(c.args, "0.0.0.0/0") || contains(c.args, "::/0")) {
		return true
	}
	if c.name != "route" {
		return false
	}
	joined := " " + strings.Join(c.args, " ") + " "
	if strings.Contains(joined, " get ") {
		return false
	}
	adds := strings.Contains(joined, " add ") || strings.Contains(joined, " change ")
	if !adds {
		return false
	}
	for _, bad := range []string{" default ", " 0.0.0.0/0 ", " 0.0.0.0/1 ", " 128.0.0.0/1 ", " ::/0 ", " 0.0.0.0 "} {
		if strings.Contains(joined, bad) {
			return true
		}
	}
	return false
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// CanApply reports whether wg and ip are on PATH and the process may create
// a netdev. ASP_LOCAL_NET_APPLY=1 forces the attempt (tests). =0 never applies.
func CanApply() bool {
	switch strings.TrimSpace(os.Getenv("ASP_LOCAL_NET_APPLY")) {
	case "0", "false", "no":
		return false
	}
	if targetGOOS() == "darwin" {
		return canApplyDarwin()
	}
	if _, err := exec.LookPath("wg"); err != nil {
		return false
	}
	if _, err := exec.LookPath("ip"); err != nil {
		return false
	}
	if os.Getenv("ASP_LOCAL_NET_APPLY") == "1" {
		return true
	}
	return hasNetAdmin()
}

func hasNetAdmin() bool {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "CapEff:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return false
		}
		v, err := strconv.ParseUint(fields[1], 16, 64)
		if err != nil {
			return false
		}
		const capNetAdmin = 12
		return v&(1<<capNetAdmin) != 0
	}
	return false
}

// BringUp prints the exact commands and runs them when CanApply is true.
// The private key is referenced by path, never printed.
func BringUp(stderr io.Writer, d ClientDevice) (bool, error) {
	if targetGOOS() == "darwin" {
		return bringUpDarwin(stderr, d)
	}
	if d.hijacks() {
		return false, fmt.Errorf("refusing local-net client recipe that changes the host default route")
	}
	if strings.TrimSpace(d.NodePublic) == "" {
		fmt.Fprintf(stderr, "asp: local-net device not applied: node public key is empty (node-agent has not registered it yet). Re-run local-net up.\n")
		printCmds(stderr, d.Commands())
		return false, nil
	}
	printCmds(stderr, d.Commands())
	if !CanApply() {
		fmt.Fprintf(stderr, "asp: local-net device not applied (need wireguard-tools on PATH and CAP_NET_ADMIN; or set ASP_LOCAL_NET_APPLY=1 only in a lab that has both). Config file is mode 0600.\n")
		return false, nil
	}
	for _, c := range d.Commands() {
		soft := c.name == "ip" && len(c.args) >= 2 && c.args[0] == "link" && c.args[1] == "delete"
		if err := run(c, soft); err != nil {
			return false, err
		}
	}
	if _, err := exec.LookPath("nft"); err == nil {
		for _, c := range nftCmds(d.Iface, false) {
			if err := run(c, false); err != nil {
				fmt.Fprintf(stderr, "asp: local-net nat step failed (device is up, NAT not proven): %v\n", err)
				break
			}
		}
	} else {
		fmt.Fprintf(stderr, "asp: nft not on PATH; NAT not applied. Device is up without a host default route. Packets were not proven.\n")
	}
	return true, nil
}

// TearDown deletes the client device. Missing devices are ignored.
func TearDown(stderr io.Writer, iface, sessionPath string) error {
	if targetGOOS() == "darwin" {
		return tearDownDarwin(stderr, iface, sessionPath)
	}
	iface = strings.TrimSpace(iface)
	if iface == "" {
		return nil
	}
	if _, err := exec.LookPath("ip"); err != nil {
		return nil
	}
	if os.Getenv("ASP_LOCAL_NET_APPLY") == "0" {
		return nil
	}
	if !CanApply() && os.Getenv("ASP_LOCAL_NET_APPLY") != "1" {
		// Still try delete when ip exists and we have the capability, or when
		// forced. Without capability, skip rather than failing session stop.
		if !hasNetAdmin() {
			return nil
		}
	}
	if err := run(cmd{"ip", []string{"link", "delete", "dev", iface}}, true); err != nil {
		return err
	}
	if _, err := exec.LookPath("nft"); err == nil {
		_ = run(cmd{"nft", []string{"delete", "table", "ip", nftTable(iface)}}, true)
	}
	return nil
}

func nftTable(iface string) string {
	var b strings.Builder
	b.WriteString("asp_ln_")
	for _, r := range iface {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func nftCmds(iface string, del bool) []cmd {
	table := nftTable(iface)
	if del {
		return []cmd{{"nft", []string{"delete", "table", "ip", table}}}
	}
	return []cmd{
		{"nft", []string{"add", "table", "ip", table}},
		{"nft", []string{"add", "chain", "ip", table, "postrouting", "{", "type", "nat", "hook", "postrouting", "priority", "100", ";", "}"}},
		{"nft", []string{"add", "rule", "ip", table, "postrouting", "iifname", iface, "masquerade"}},
	}
}

func printCmds(stderr io.Writer, cmds []cmd) {
	fmt.Fprintln(stderr, "asp: local-net commands (no host default route):")
	for _, c := range cmds {
		fmt.Fprintln(stderr, "  "+c.line())
	}
}

func run(c cmd, soft bool) error {
	cmd := exec.Command(c.name, c.args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	if soft && (strings.Contains(msg, "Cannot find device") || strings.Contains(msg, "does not exist") || strings.Contains(msg, "No such file")) {
		return nil
	}
	if strings.Contains(msg, "File exists") {
		return nil
	}
	if msg == "" {
		msg = err.Error()
	}
	return fmt.Errorf("%s: %s", c.line(), msg)
}
