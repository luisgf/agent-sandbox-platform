package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"
)

// ErrDestinationBlocked marks an address the proxy never connects to.
var ErrDestinationBlocked = errors.New("destination address not allowed")

// DialGuard decides which addresses the proxy may connect to, whatever the
// allowlist says about the name.
//
// The allowlist is the tenant's and it names hosts; a name can resolve to
// anything, including the node itself. Without a guard, a guest that reaches
// the proxy under a wildcard rule could point a name it controls at 127.0.0.1
// and use the proxy, which runs in the node's network namespace, to talk to the
// node-agent's local API, to cloud metadata at 169.254.169.254, or to anything
// on the node's LAN. The check runs on the address about to be connected, after
// name resolution, so a name that resolves differently each time gains nothing.
type DialGuard struct {
	// AllowCIDRs are private destinations the operator opens on purpose
	// (--egress-allow-cidr). They can open RFC 1918, unique-local and shared
	// (carrier-grade NAT) space; nothing else.
	AllowCIDRs []netip.Prefix
	// Never lists networks refused even when AllowCIDRs covers them: the
	// guests' own network, so one sandbox cannot reach another through the proxy.
	Never []netip.Prefix
	// AllowLoopback lets the proxy connect to loopback addresses. For tests and
	// the lab smoke binary only; no node-agent flag sets it.
	AllowLoopback bool
	// LocalAddrs returns the node's own addresses. nil: those of its interfaces.
	LocalAddrs func() []netip.Addr

	localMu sync.Mutex
	local   []netip.Addr
	localAt time.Time
}

// neverPrefixes are not destinations at all, or translate to ones we refuse.
var neverPrefixes = mustPrefixes(
	"0.0.0.0/8",     // "this network"
	"192.0.0.0/24",  // IETF protocol assignments
	"198.18.0.0/15", // benchmarking
	"240.0.0.0/4",   // reserved; includes the broadcast address
	"64:ff9b::/96",  // NAT64: translated to IPv4, private ranges included
	"fec0::/10",     // deprecated site-local
)

// sharedPrefix is carrier-grade NAT space: private in effect, not in RFC 1918.
var sharedPrefix = netip.MustParsePrefix("100.64.0.0/10")

func mustPrefixes(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(s))
	for i, p := range s {
		out[i] = netip.MustParsePrefix(p)
	}
	return out
}

// Check returns nil when the proxy may connect to ip, or an error wrapping
// ErrDestinationBlocked that says which kind of address it is.
func (g *DialGuard) Check(ip netip.Addr) error {
	ip = ip.Unmap().WithZone("")
	if !ip.IsValid() {
		return blocked(ip, "not an address")
	}
	if ip.IsLoopback() {
		if g.AllowLoopback {
			return nil
		}
		return blocked(ip, "loopback")
	}
	switch {
	case ip.IsUnspecified():
		return blocked(ip, "unspecified")
	case ip.IsLinkLocalUnicast():
		return blocked(ip, "link-local (cloud metadata lives here)")
	case ip.IsMulticast():
		return blocked(ip, "multicast")
	}
	for _, p := range neverPrefixes {
		if p.Contains(ip) {
			return blocked(ip, "reserved ("+p.String()+")")
		}
	}
	for _, p := range g.Never {
		if p.Contains(ip) {
			return blocked(ip, "the guests' network")
		}
	}
	if g.isLocal(ip) {
		return blocked(ip, "an address of this node")
	}
	if ip.IsPrivate() || sharedPrefix.Contains(ip) {
		for _, p := range g.AllowCIDRs {
			if p.Contains(ip) {
				return nil
			}
		}
		return blocked(ip, "private network; the operator opens one with --egress-allow-cidr")
	}
	return nil
}

func blocked(ip netip.Addr, why string) error {
	return fmt.Errorf("%w: %s is %s", ErrDestinationBlocked, ip, why)
}

// Control is net.Dialer.Control: the dialer calls it with each address it is
// about to connect to, after resolving the name, and gives up on that address
// when it fails.
func (g *DialGuard) Control(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDestinationBlocked, err)
	}
	return g.Check(ap.Addr())
}

// DialContext connects to addr, a host:port, through the guard.
func (g *DialGuard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("%w: network %q", ErrDestinationBlocked, network)
	}
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second, Control: g.Control}
	return d.DialContext(ctx, network, addr)
}

// isLocal reports whether ip is assigned to one of the node's interfaces.
func (g *DialGuard) isLocal(ip netip.Addr) bool {
	g.localMu.Lock()
	if g.local == nil || time.Since(g.localAt) > 5*time.Second {
		if g.LocalAddrs != nil {
			g.local = g.LocalAddrs()
		} else {
			g.local = interfaceAddrs()
		}
		if g.local == nil {
			g.local = []netip.Addr{}
		}
		g.localAt = time.Now()
	}
	local := g.local
	g.localMu.Unlock()
	for _, a := range local {
		if a == ip {
			return true
		}
	}
	return false
}

func interfaceAddrs() []netip.Addr {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if addr, ok := netip.AddrFromSlice(ip); ok {
			out = append(out, addr.Unmap().WithZone(""))
		}
	}
	return out
}

// ParseCIDRs parses a comma-separated list of prefixes; a bare address is a
// single-host prefix.
func ParseCIDRs(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range splitList(s) {
		if p, err := netip.ParsePrefix(part); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(part)
		if err != nil {
			return nil, fmt.Errorf("%q is not an address or a CIDR", part)
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

func splitList(s string) []string {
	var out []string
	cur := ""
	flush := func() {
		for len(cur) > 0 && (cur[0] == ' ' || cur[0] == '\t') {
			cur = cur[1:]
		}
		for len(cur) > 0 && (cur[len(cur)-1] == ' ' || cur[len(cur)-1] == '\t') {
			cur = cur[:len(cur)-1]
		}
		if cur != "" {
			out = append(out, cur)
		}
		cur = ""
	}
	for _, r := range s {
		if r == ',' {
			flush()
			continue
		}
		cur += string(r)
	}
	flush()
	return out
}
