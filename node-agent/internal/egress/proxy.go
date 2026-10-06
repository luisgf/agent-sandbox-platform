package egress

import (
	"errors"
	"strconv"
	"strings"
	"sync"
)

// ErrDenied is returned when a host is not on the allowlist.
var ErrDenied = errors.New("host not allowed")

// Rule is a single host_pattern (+ optional port) allowlist entry.
type Rule struct {
	HostPattern string
	Port        *int
}

// Allowlist is a deny-by-default (or allow-all) host checker for HTTP(S) egress.
type Allowlist struct {
	mu       sync.RWMutex
	rules    []Rule
	allowAll bool // when true and no rules / empty mode
}

func NewAllowlist(hosts ...string) *Allowlist {
	a := &Allowlist{}
	for _, h := range hosts {
		a.Add(h)
	}
	return a
}

// NewAllowlistFromPolicy builds from control-plane EgressPolicy fields.
func NewAllowlistFromPolicy(mode string, rules []Rule) *Allowlist {
	a := &Allowlist{allowAll: mode == "allow-all"}
	for _, r := range rules {
		a.AddRule(r)
	}
	return a
}

func (a *Allowlist) SetAllowAll(v bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.allowAll = v
}

func (a *Allowlist) Add(host string) {
	a.AddRule(Rule{HostPattern: host})
}

func (a *Allowlist) AddRule(r Rule) {
	hp := normalizeHost(r.HostPattern)
	if hp == "" {
		return
	}
	nr := Rule{HostPattern: hp}
	if r.Port != nil {
		p := *r.Port
		nr.Port = &p
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rules = append(a.rules, nr)
}

// ReplaceRules atomically replaces the rule set (and optionally allow-all mode).
func (a *Allowlist) ReplaceRules(mode string, rules []Rule) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.allowAll = mode == "allow-all"
	a.rules = nil
	for _, r := range rules {
		hp := normalizeHost(r.HostPattern)
		if hp == "" {
			continue
		}
		nr := Rule{HostPattern: hp}
		if r.Port != nil {
			p := *r.Port
			nr.Port = &p
		}
		a.rules = append(a.rules, nr)
	}
}

// Check returns nil if host is allowed, or ErrDenied otherwise.
// Matching is case-insensitive exact host (no port); subdomain wildcards like *.example.com are supported.
func (a *Allowlist) Check(host string) error {
	return a.CheckHostPort(host, 0)
}

// CheckHostPort checks host and optional port (port<=0 means any port / ignore port constraint).
func (a *Allowlist) CheckHostPort(host string, port int) error {
	host = normalizeHost(host)
	if host == "" {
		return ErrDenied
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.allowAll && len(a.rules) == 0 {
		return nil
	}
	if len(a.rules) == 0 {
		return ErrDenied
	}
	for _, r := range a.rules {
		if !hostMatches(host, r.HostPattern) {
			continue
		}
		if r.Port == nil {
			return nil
		}
		if port <= 0 || *r.Port == port {
			return nil
		}
	}
	return ErrDenied
}

func hostMatches(host, pattern string) bool {
	if host == pattern {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:] // ".example.com"
		if strings.HasSuffix(host, suffix) || host == pattern[2:] {
			return true
		}
	}
	return false
}

func normalizeHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	// strip path
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	// strip port for host-only normalize; callers pass port separately
	if i := strings.LastIndex(host, ":"); i >= 0 {
		// avoid mangling IPv6; MVP hosts are DNS names
		if _, err := strconv.Atoi(host[i+1:]); err == nil {
			host = host[:i]
		}
	}
	return host
}

// ParseHostPort splits "host:443" or "https://host/path".
func ParseHostPort(raw string) (host string, port int) {
	raw = strings.TrimSpace(raw)
	host = normalizeHost(raw)
	// try extract port before normalize stripped it
	tmp := strings.TrimSpace(strings.ToLower(raw))
	if i := strings.Index(tmp, "://"); i >= 0 {
		tmp = tmp[i+3:]
	}
	if i := strings.Index(tmp, "/"); i >= 0 {
		tmp = tmp[:i]
	}
	if i := strings.LastIndex(tmp, ":"); i >= 0 {
		if p, err := strconv.Atoi(tmp[i+1:]); err == nil {
			port = p
		}
	}
	return host, port
}
