package idp

import (
	"encoding/json"
	"os"
	"strings"
)

// Role is a tenant RBAC role (ADR-0007 phase 3).
type Role string

const (
	RoleNone   Role = ""
	RoleViewer Role = "viewer"
	// RoleUser creates sandboxes and acts only on its own: it neither sees nor
	// runs anything in the sandboxes of others.
	RoleUser     Role = "user"
	RoleOperator Role = "operator"
	RoleAdmin    Role = "admin"
)

// Rank returns a comparable privilege level (higher = more privilege).
func (r Role) Rank() int {
	switch r {
	case RoleAdmin:
		return 4
	case RoleOperator:
		return 3
	case RoleUser:
		return 2
	case RoleViewer:
		return 1
	default:
		return 0
	}
}

// Valid reports whether r is a known non-empty role.
func (r Role) Valid() bool {
	return r == RoleAdmin || r == RoleOperator || r == RoleUser || r == RoleViewer
}

const (
	defaultRoleClaim       = "groups"
	defaultRolePrefix      = "asp-"
	defaultDestroyAnyGroup = "sandbox:destroy-any"
	defaultExecAnyGroup    = "sandbox:exec-any"
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

	eag := strings.TrimSpace(os.Getenv("ASP_IDP_EXEC_ANY_GROUP"))
	if eag == "" {
		eag = defaultExecAnyGroup
	}
	cfg.ExecAnyGroup = eag
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

// Grants is what a token's role claim values give: the highest role, plus the
// explicit grants an operator needs to act on sandboxes it does not own.
type Grants struct {
	Role       Role
	DestroyAny bool // ASP_IDP_DESTROY_ANY_GROUP
	ExecAny    bool // ASP_IDP_EXEC_ANY_GROUP
}

// MapGrants maps claim values to Grants, keeping the highest role. viewer and
// user are orthogonal (one sees the whole tenant, the other creates and runs
// its own sandboxes), so holding both is operator, which is exactly their
// union.
func (c Config) MapGrants(values []string) Grants {
	var g Grants
	viewer, user := false, false
	for _, raw := range values {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		if matchGroup(v, c.DestroyAnyGroup, defaultDestroyAnyGroup) {
			g.DestroyAny = true
		}
		if matchGroup(v, c.ExecAnyGroup, defaultExecAnyGroup) {
			g.ExecAny = true
		}
		role := c.mapOne(v)
		viewer = viewer || role == RoleViewer
		user = user || role == RoleUser
		if role.Rank() > g.Role.Rank() {
			g.Role = role
		}
	}
	if viewer && user && g.Role.Rank() < RoleOperator.Rank() {
		g.Role = RoleOperator
	}
	return g
}

func matchGroup(v, group, fallback string) bool {
	group = strings.TrimSpace(group)
	if group == "" {
		group = fallback
	}
	return strings.EqualFold(v, group)
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
	if prefix := strings.ToLower(c.RolePrefix); prefix != "" {
		if rest, ok := strings.CutPrefix(low, prefix); ok && Role(rest).Valid() {
			return Role(rest)
		}
		return RoleNone
	}
	if len(c.RoleMap) > 0 {
		// A map is the whole vocabulary: a group it does not name grants nothing.
		return RoleNone
	}
	// No map and no prefix (only a Config built by hand: the environment always
	// supplies one of them): the groups are the role names themselves.
	if Role(low).Valid() {
		return Role(low)
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
