package egress

import (
	"net"
	"net/netip"
	"strings"
	"sync"
)

// PolicyCache maps each guest source prefix to its sandbox, and each sandbox
// to the allowlist the control plane attached to its last exec. The proxy and
// the DNS sink look a request up by source address, so one sandbox's policy
// never applies to another sandbox's traffic.
type PolicyCache struct {
	mu       sync.RWMutex
	prefixes map[string]netip.Prefix // sandbox ID → guest prefix
	policies map[string]*Allowlist   // sandbox ID → allowlist
	tenants  map[string]string       // sandbox ID → tenant, for the metrics
}

// Bind records that traffic from prefix belongs to sandboxID.
func (c *PolicyCache) Bind(sandboxID string, prefix netip.Prefix) {
	if c == nil || sandboxID == "" || !prefix.IsValid() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.prefixes == nil {
		c.prefixes = make(map[string]netip.Prefix)
	}
	c.prefixes[sandboxID] = prefix.Masked()
}

// Set stores the allowlist for sandboxID.
func (c *PolicyCache) Set(sandboxID string, al *Allowlist) {
	if c == nil || sandboxID == "" || al == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.policies == nil {
		c.policies = make(map[string]*Allowlist)
	}
	c.policies[sandboxID] = al
}

// SetTenant records the tenant of sandboxID. It decides nothing: the proxy and the
// sink use it to say whose traffic they counted.
func (c *PolicyCache) SetTenant(sandboxID, tenant string) {
	if c == nil || sandboxID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tenants == nil {
		c.tenants = make(map[string]string)
	}
	c.tenants[sandboxID] = tenant
}

// TenantOf is the tenant recorded for sandboxID, or "".
func (c *PolicyCache) TenantOf(sandboxID string) string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tenants[sandboxID]
}

// Forget drops the prefix, the policy and the tenant of sandboxID.
func (c *PolicyCache) Forget(sandboxID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.prefixes, sandboxID)
	delete(c.policies, sandboxID)
	delete(c.tenants, sandboxID)
}

// Get returns the allowlist stored for sandboxID, or nil.
func (c *PolicyCache) Get(sandboxID string) *Allowlist {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.policies[sandboxID]
}

// SandboxFor returns the sandbox whose prefix contains addr, or "".
func (c *PolicyCache) SandboxFor(addr netip.Addr) string {
	if c == nil || !addr.IsValid() {
		return ""
	}
	addr = addr.Unmap()
	c.mu.RLock()
	defer c.mu.RUnlock()
	for id, p := range c.prefixes {
		if p.Contains(addr) {
			return id
		}
	}
	return ""
}

// ForAddr returns the allowlist for the sandbox that owns addr. known is false
// when addr is not a sandbox address. A known sandbox with no policy yet gets
// deny-default, never another sandbox's policy or the node-wide one.
func (c *PolicyCache) ForAddr(addr netip.Addr) (al *Allowlist, known bool) {
	id := c.SandboxFor(addr)
	if id == "" {
		return nil, false
	}
	if al := c.Get(id); al != nil {
		return al, true
	}
	return NewAllowlistFromPolicy("deny-default", nil), true
}

// remoteAddr parses "ip:port" (or a bare IP) into an address.
func remoteAddr(s string) netip.Addr {
	s = strings.TrimSpace(s)
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

// addrOf extracts the IP from a net.Addr (UDP or TCP).
func addrOf(a net.Addr) netip.Addr {
	switch v := a.(type) {
	case *net.UDPAddr:
		if ip, ok := netip.AddrFromSlice(v.IP); ok {
			return ip.Unmap()
		}
	case *net.TCPAddr:
		if ip, ok := netip.AddrFromSlice(v.IP); ok {
			return ip.Unmap()
		}
	}
	if a == nil {
		return netip.Addr{}
	}
	return remoteAddr(a.String())
}
