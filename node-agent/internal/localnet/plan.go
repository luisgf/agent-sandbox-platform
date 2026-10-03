// Package localnet decides per-sandbox egress for the on-demand full tunnel
// (ADR-0010). When local_net is on, the public proxy path is not installed.
// Disconnect is a blackhole, never a silent fallback.
package localnet

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
)

// Kind is the egress plan for one sandbox.
type Kind string

const (
	// KindPublic is ADR-0002: node proxy, DNS sink, nft asp_egress.
	KindPublic Kind = "public"
	// KindBlackhole sinks 0.0.0.0/0 (and ::/0) for this session only.
	KindBlackhole Kind = "blackhole"
	// KindTunnel sends that default through the local-agent tunnel.
	KindTunnel Kind = "tunnel"
)

// Plan is what the node should install for one sandbox.
// The default route lives in TableID, selected by ingress on Tap.
// It is never installed in the host main table.
type Plan struct {
	SandboxID  string
	OwnerSub   string
	Kind       Kind
	Iface      string
	Table      string
	Tap        string
	TableID    int
	ListenPort int
	// NodeCIDR is the address on the node WireGuard device (a /30).
	NodeCIDR string
	// ClientCIDR is the address the local agent puts on its device.
	ClientCIDR string
	// PeerPublic is the local agent's WireGuard public key when state is up.
	PeerPublic string
	// KeyPath is the node private key file (mode 0600). Not a log field.
	KeyPath string
	// UsePublicProxy is true only for KindPublic.
	UsePublicProxy bool
	// BlockNodeProxy rejects this TAP talking to :8888 / the DNS sink.
	BlockNodeProxy bool
}

// Decide returns the egress plan. local_net false (or state off) keeps the
// public path. Any other state with the flag on refuses the public proxy.
func Decide(sandboxID, ownerSub string, localNet bool, state string) Plan {
	short := ShortID(sandboxID)
	state = strings.TrimSpace(state)
	if !localNet || state == "off" {
		return Plan{
			SandboxID:      sandboxID,
			OwnerSub:       ownerSub,
			Kind:           KindPublic,
			UsePublicProxy: true,
		}
	}
	nodeCIDR, clientCIDR := Tunnel(sandboxID)
	p := Plan{
		SandboxID:      sandboxID,
		OwnerSub:       ownerSub,
		Iface:          "wg-asp-" + short,
		Table:          "aspln-" + short,
		Tap:            "asp-" + short,
		TableID:        TableID(sandboxID),
		ListenPort:     ListenPort(sandboxID),
		NodeCIDR:       nodeCIDR,
		ClientCIDR:     clientCIDR,
		UsePublicProxy: false,
		BlockNodeProxy: true,
	}
	if state == "up" {
		p.Kind = KindTunnel
		return p
	}
	// pending, withdrawn, empty, unknown: blackhole. Never KindPublic.
	p.Kind = KindBlackhole
	return p
}

// ShortID is the iface suffix. Keep in sync with the control plane and the CLI.
func ShortID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > 8 {
		id = id[:8]
	}
	if id == "" {
		id = "sandbox"
	}
	return id
}

func fnv32a(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// ListenPort is the UDP port for this sandbox's WireGuard device.
func ListenPort(id string) int {
	return 47000 + int(fnv32a("udp"+ShortID(id))%8000)
}

// TableID is a policy-routing table outside the main table (32766) and local (255).
func TableID(id string) int {
	return 10000 + int(fnv32a(ShortID(id))%20000)
}

// Tunnel returns node and client /30 addresses in 10.188.0.0/16.
func Tunnel(id string) (nodeCIDR, clientCIDR string) {
	slot := fnv32a("tun"+ShortID(id)) % 16384
	base := slot * 4
	a := (base >> 8) & 0xff
	b := base & 0xff
	nodeCIDR = fmt.Sprintf("10.188.%d.%d/30", a, b+1)
	clientCIDR = fmt.Sprintf("10.188.%d.%d/30", a, b+2)
	return nodeCIDR, clientCIDR
}

// Cmd is one argv the host applier runs. Name is looked up on PATH.
type Cmd struct {
	Name string
	Args []string
}

// Argv is the host recipe for a non-public plan. KindPublic returns nil:
// that path is the existing nft/proxy setup, untouched.
// Tunnel routes go in TableID, selected by iif of the sandbox TAP.
// There is no `ip route … default` without a table, and nothing points at :8888.
func Argv(p Plan) []Cmd {
	switch p.Kind {
	case KindPublic:
		return nil
	case KindTunnel:
		cmds := []Cmd{
			{Name: "ip", Args: []string{"link", "delete", "dev", p.Iface}},
			{Name: "ip", Args: []string{"link", "add", "dev", p.Iface, "type", "wireguard"}},
			{Name: "ip", Args: []string{"address", "add", p.NodeCIDR, "dev", p.Iface}},
		}
		if p.KeyPath != "" && p.PeerPublic != "" {
			cmds = append(cmds, Cmd{Name: "wg", Args: []string{
				"set", p.Iface,
				"listen-port", strconv.Itoa(p.ListenPort),
				"private-key", p.KeyPath,
				"peer", p.PeerPublic,
				"allowed-ips", "0.0.0.0/0,::/0",
			}})
		}
		table := strconv.Itoa(p.TableID)
		cmds = append(cmds,
			Cmd{Name: "ip", Args: []string{"link", "set", p.Iface, "up"}},
			Cmd{Name: "ip", Args: []string{"route", "replace", "default", "dev", p.Iface, "table", table}},
			Cmd{Name: "ip", Args: []string{"route", "replace", "::/0", "dev", p.Iface, "table", table}},
			Cmd{Name: "ip", Args: []string{"rule", "add", "iif", p.Tap, "lookup", table, "priority", table}},
		)
		return cmds
	default:
		table := strconv.Itoa(p.TableID)
		return []Cmd{
			{Name: "ip", Args: []string{"link", "delete", "dev", p.Iface}},
			{Name: "ip", Args: []string{"route", "replace", "blackhole", "0.0.0.0/0", "table", table}},
			{Name: "ip", Args: []string{"route", "replace", "blackhole", "::/0", "table", table}},
			{Name: "ip", Args: []string{"rule", "add", "iif", p.Tap, "lookup", table, "priority", table}},
		}
	}
}

// ClearArgv removes the device and the per-sandbox rule. It does not install
// a public-proxy route.
func ClearArgv(p Plan) []Cmd {
	table := strconv.Itoa(p.TableID)
	return []Cmd{
		{Name: "ip", Args: []string{"link", "delete", "dev", p.Iface}},
		{Name: "ip", Args: []string{"rule", "del", "iif", p.Tap, "lookup", table, "priority", table}},
		{Name: "ip", Args: []string{"route", "flush", "table", table}},
	}
}

// Commands joins Argv for logs and tests.
func Commands(p Plan) []string {
	return joinCmds(Argv(p))
}

func joinCmds(cmds []Cmd) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, c.Name+" "+strings.Join(c.Args, " "))
	}
	return out
}

// HijacksHost reports a recipe that would move the host default route or
// send the sandbox at the node proxy. Host.Apply refuses those.
func HijacksHost(cmds []Cmd) bool {
	for _, c := range cmds {
		line := c.Name + " " + strings.Join(c.Args, " ")
		if strings.Contains(line, "8888") || strings.Contains(line, "asp_egress") || strings.Contains(line, "proxy") {
			return true
		}
		if c.Name != "ip" {
			continue
		}
		if !contains(c.Args, "route") && !contains(c.Args, "rule") {
			continue
		}
		if contains(c.Args, "default") || contains(c.Args, "::/0") || contains(c.Args, "0.0.0.0/0") || contains(c.Args, "blackhole") {
			tab := flagValue(c.Args, "table")
			if tab == "" || tab == "main" || tab == "local" || tab == "0" || tab == "254" || tab == "255" || tab == "32766" {
				return true
			}
			n, err := strconv.Atoi(tab)
			if err != nil || n < 10000 || n >= 30000 {
				return true
			}
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

func flagValue(args []string, name string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

// UsesPublicProxy reports whether this plan would send the sandbox default
// through the node forward proxy. False for every local_net plan.
func (p Plan) UsesPublicProxy() bool { return p.UsePublicProxy }
