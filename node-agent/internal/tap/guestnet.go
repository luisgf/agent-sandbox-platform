package tap

import (
	"fmt"
	"net/netip"
)

// DefaultGuestSubnet is the pool each sandbox's /30 is carved from.
const DefaultGuestSubnet = "10.200.0.0/16"

// GuestNet is one sandbox's point-to-point /30 between its TAP and the guest.
// Every TAP gets its own prefix, so the host routes each guest's replies to
// the right TAP and the egress proxy can tell sandboxes apart by source IP.
type GuestNet struct {
	Prefix netip.Prefix // e.g. 10.200.0.4/30
	Host   netip.Addr   // TAP address, the guest's gateway (.5)
	Guest  netip.Addr   // guest eth0 address (.6)
}

// HostCIDR is the address to put on the TAP ("10.200.0.5/30").
func (n GuestNet) HostCIDR() string {
	return netip.PrefixFrom(n.Host, n.Prefix.Bits()).String()
}

// KernelIPArg is the kernel ip= parameter that configures the guest's eth0
// at boot (CONFIG_IP_PNP). Kernels without it ignore the argument; the guest
// image's cmdline-ip.service applies the same token from /proc/cmdline.
func (n GuestNet) KernelIPArg() string {
	return fmt.Sprintf("ip=%s::%s:255.255.255.252::eth0:off", n.Guest, n.Host)
}

// Slot returns the index-th /30 of subnet (an IPv4 prefix of /29 or larger).
func Slot(subnet netip.Prefix, index int) (GuestNet, error) {
	subnet = subnet.Masked()
	if !subnet.Addr().Is4() || subnet.Bits() > 29 {
		return GuestNet{}, fmt.Errorf("guest subnet %s: need IPv4 /29 or larger", subnet)
	}
	if index < 0 || index >= 1<<(30-subnet.Bits()) {
		return GuestNet{}, fmt.Errorf("guest subnet %s exhausted (slot %d)", subnet, index)
	}
	b := subnet.Addr().As4()
	base := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	base += uint32(index) * 4
	at := func(off uint32) netip.Addr {
		v := base + off
		return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
	}
	return GuestNet{
		Prefix: netip.PrefixFrom(at(0), 30),
		Host:   at(1),
		Guest:  at(2),
	}, nil
}
