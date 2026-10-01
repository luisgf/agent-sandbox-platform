package idp

import (
	"encoding/json"
	"os"
	"strings"
)

// Role is a tenant RBAC role (ADR-0007 phase 3).
type Role string

const (
	RoleNone     Role = ""
	RoleViewer   Role = "viewer"
	RoleOperator Role = "operator"
	RoleAdmin    Role = "admin"
)

// Rank returns a comparable privilege level (higher = more privilege).
func (r Role) Rank() int {
	switch r {
	case RoleAdmin:
		return 3
	case RoleOperator:
		return 2
	case RoleViewer:
		return 1
	default:
		return 0
	}
}

// Valid reports whether r is a known non-empty role.
func (r Role) Valid() bool {
	return r == RoleAdmin || r == RoleOperator || r == RoleViewer
}

const (
	defaultRoleClaim       = "groups"
	defaultRolePrefix      = "asp-"
	defaultDestroyAnyGroup = "sandbox:destroy-any"
)

// RoleConfigFromEnv reads ASP_IDP_ROLE_* into cfg (mutates Role* fields).
func RoleConfigFromEnv(cfg *Config) {
	if cfg == nil {
		return
	}
	claim := strings.TrimSpace(os.Getenv("ASP_IDP_ROLE_CLAIM"))
	if claim == "" {
		claim = defaultRoleClaim
	}
	cfg.RoleClaim = claim

	cfg.RoleMap = parseRoleMap(os.Getenv("ASP_IDP_ROLE_MAP"))
	prefix := strings.TrimSpace(os.Getenv("ASP_IDP_ROLE_PREFIX"))
	if prefix == "" && len(cfg.RoleMap) == 0 {
		prefix = defaultRolePrefix
	}
	cfg.RolePrefix = prefix

	dag := strings.TrimSpace(os.Getenv("ASP_IDP_DESTROY_ANY_GROUP"))
	if dag == "" {
		dag = defaultDestroyAnyGroup
	}
	cfg.DestroyAnyGroup = dag
}

// parseRoleMap parses "asp-admin:admin,asp-ops:operator" (comma-separated claim→role).
func parseRoleMap(raw string) map[string]Role {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := make(map[string]Role)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(strings.ToLower(v))
		role := Role(v)
		if k == "" || !role.Valid() {
			continue
		}
		out[k] = role
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// MapRoles picks the highest privilege Role from claim values and whether destroy-any applies.
func (c Config) MapRoles(values []string) (Role, bool) {
	var best Role
	destroyAny := false
	dag := strings.TrimSpace(c.DestroyAnyGroup)
	if dag == "" {
		dag = defaultDestroyAnyGroup
	}
	prefix := c.RolePrefix
	for _, raw := range values {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		if dag != "" && (v == dag || strings.EqualFold(v, dag)) {
			destroyAny = true
		}
		role := c.mapOne(v)
		if role.Rank() > best.Rank() {
			best = role
		}
		_ = prefix
	}
	return best, destroyAny
}

func (c Config) mapOne(v string) Role {
	if len(c.RoleMap) > 0 {
		if r, ok := c.RoleMap[v]; ok {
			return r
		}
		// also try lower-case key
		if r, ok := c.RoleMap[strings.ToLower(v)]; ok {
			return r
		}
	}
	low := strings.ToLower(v)
	// bare role names
	if Role(low).Valid() {
		return Role(low)
	}
	prefix := c.RolePrefix
	if prefix != "" && strings.HasPrefix(low, strings.ToLower(prefix)) {
		rest := strings.TrimPrefix(low, strings.ToLower(prefix))
		if Role(rest).Valid() {
			return Role(rest)
		}
	}
	return RoleNone
}

// stringSliceFlex unmarshals JWT claim as string, []string, or JSON array embedded.
type stringSliceFlex []string

func (s *stringSliceFlex) UnmarshalJSON(b []byte) error {
	b = bytesTrim(b)
	if len(b) == 0 || string(b) == "null" {
		*s = nil
		return nil
	}
	if b[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		str = strings.TrimSpace(str)
		if str == "" {
			*s = nil
			return nil
		}
		// space or comma separated single string
		if strings.Contains(str, ",") {
			parts := strings.Split(str, ",")
			out := make([]string, 0, len(parts))
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p != "" {
					out = append(out, p)
				}
			}
			*s = out
			return nil
		}
		*s = []string{str}
		return nil
	}
	var arr []string
	if err := json.Unmarshal(b, &arr); err != nil {
		return err
	}
	*s = arr
	return nil
}
