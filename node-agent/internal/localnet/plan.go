// Package localnet decides per-sandbox egress for the on-demand full tunnel
// (ADR-0010). When local_net is on, the public proxy path is not installed.
// Disconnect is a blackhole, never a silent fallback.
package localnet

import (
	"hash/fnv"
	"strconv"
	"strings"
)

// Kind is the egress plan for one sandbox.
type Kind string

const (
	// guestLAN is the pool every sandbox's /30 comes from
	// (tap.DefaultGuestSubnet). Replies from this session's tunnel go to this
	// session's TAP through the session table, whatever /30 the guest has;
	// the main table is not used.
	guestLAN = "10.200.0.0/16"
	// KindPublic is ADR-0002: node proxy, DNS sink, nft asp_egress.
	KindPublic Kind = "public"
	// KindBlackhole sinks 0.0.0.0/0 (and ::/0) for this session only.
	KindBlackhole Kind = "blackhole"
	// KindTunnel sends that default through the local-agent tunnel.
	KindTunnel Kind = "tunnel"
)

// IfacePrefix starts every session WireGuard device name (Plan.Iface).
const IfacePrefix = "wg-asp-"

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

// Decide is DecideWith on the allocation hashed from the short id: what nodes
// used before allocation. Tests use it, and Clear uses it for state an older
// node left; live sessions use their Allocator's allocation.
func Decide(sandboxID, ownerSub string, localNet bool, state string) Plan {
	return DecideWith(sandboxID, ownerSub, localNet, state, HashAllocation(sandboxID))
}

// DecideWith returns the egress plan. local_net false (or state off) keeps the
// public path. Any other state with the flag on refuses the public proxy and
// uses alloc's routing table, UDP port and tunnel addresses.
func DecideWith(sandboxID, ownerSub string, localNet bool, state string, alloc Allocation) Plan {
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
	p := Plan{
		SandboxID:      sandboxID,
		OwnerSub:       ownerSub,
		Iface:          IfacePrefix + short,
		Table:          "aspln-" + short,
		Tap:            "asp-" + short,
		TableID:        alloc.TableID,
		ListenPort:     alloc.ListenPort,
		NodeCIDR:       alloc.NodeCIDR(),
		ClientCIDR:     alloc.ClientCIDR(),
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

// ListenPort is the UDP port hashed from the short id (HashAllocation).
func ListenPort(id string) int {
	return 47000 + int(fnv32a("udp"+ShortID(id))%8000)
}

// TableID is the policy-routing table hashed from the short id
// (HashAllocation); outside the main table (32766) and local (255).
func TableID(id string) int {
	return 10000 + int(fnv32a(ShortID(id))%20000)
}

// tunnelSlot is the /30 index hashed from the short id (HashAllocation).
func tunnelSlot(id string) int {
	return int(fnv32a("tun"+ShortID(id)) % slotCount)
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
			Cmd{Name: "ip", Args: []string{"route", "replace", guestLAN, "dev", p.Tap, "table", table}},
			Cmd{Name: "ip", Args: []string{"rule", "add", "iif", p.Tap, "lookup", table, "priority", table}},
			Cmd{Name: "ip", Args: []string{"rule", "add", "iif", p.Iface, "to", guestLAN, "lookup", table, "priority", returnPriority(p.TableID)}},
		)
		return cmds
	default:
		table := strconv.Itoa(p.TableID)
		return []Cmd{
			{Name: "ip", Args: []string{"rule", "del", "iif", p.Iface, "to", guestLAN, "lookup", table, "priority", returnPriority(p.TableID)}},
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
		{Name: "ip", Args: []string{"rule", "del", "iif", p.Iface, "to", guestLAN, "lookup", table, "priority", returnPriority(p.TableID)}},
		{Name: "ip", Args: []string{"route", "flush", "table", table}},
	}
}

// returnPriority is the session table id. That priority is before the main
// table (32766). A rule after main never runs: main already routes
// the guest's /30, so the session table must be consulted first.
func returnPriority(tableID int) string {
	return strconv.Itoa(tableID)
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
