package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
	"strings"
)

const (
	EgressModeAllowAll    = "allow-all"
	EgressModeDenyDefault = "deny-default"
)

// EffectiveEgress builds the policy for a tenant.
// Rules with ≥1 enabled entry → deny-default + those rules.
// Empty enabled rules: allow-all only when ASP_EGRESS_DEFAULT_ALLOW=1;
// otherwise deny-default (ASP_EGRESS_DENY_DEFAULT=1 or fail-closed default).
func EffectiveEgress(tenantID string, rules []EgressRule) EgressPolicy {
	enabled := make([]EgressRule, 0)
	for _, r := range rules {
		if r.Enabled {
			cp := r
			cp.TenantID = tenantID
			enabled = append(enabled, cp)
		}
	}
	pol := EgressPolicy{TenantID: tenantID, Rules: enabled}
	if len(enabled) > 0 {
		pol.Mode = EgressModeDenyDefault
		return pol
	}
	if envTruthy("ASP_EGRESS_DEFAULT_ALLOW") {
		pol.Mode = EgressModeAllowAll
		return pol
	}
	// Harden / fail-closed (ASP_EGRESS_DENY_DEFAULT=1 or unset allow).
	pol.Mode = EgressModeDenyDefault
	return pol
}

func EnvTruthy(key string) bool {
	return envTruthy(key)
}

func envTruthy(key string) bool {
	v := strings.TrimSpace(os.Getenv(key))
	return v == "1" || strings.EqualFold(v, "true")
}

// Version identifies what the policy allows: a hash of its mode and enabled
// rules (host pattern and port), so it changes when the decision can change
// and not when rule ids or their order do. Nodes reapply a policy only when
// its version moves.
func (p EgressPolicy) Version() string {
	type rule struct {
		Host string `json:"h"`
		Port *int   `json:"p,omitempty"`
	}
	rules := make([]rule, 0, len(p.Rules))
	for _, r := range p.Rules {
		if r.Enabled {
			rules = append(rules, rule{Host: strings.ToLower(strings.TrimSpace(r.HostPattern)), Port: r.Port})
		}
	}
	sort.Slice(rules, func(i, j int) bool {
		if rules[i].Host != rules[j].Host {
			return rules[i].Host < rules[j].Host
		}
		pi, pj := -1, -1
		if rules[i].Port != nil {
			pi = *rules[i].Port
		}
		if rules[j].Port != nil {
			pj = *rules[j].Port
		}
		return pi < pj
	})
	b, _ := json.Marshal(struct {
		Mode  string `json:"m"`
		Rules []rule `json:"r"`
	}{p.Mode, rules})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
