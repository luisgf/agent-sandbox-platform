package store

import (
	"os"
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
