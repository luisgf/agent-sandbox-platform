package api

import (
	"net/http"
	"strings"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/authn/idp"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// RBAC is enforced only when an IdP JWT principal is present (ADR-0007 phase 3).
// When IdP is off (lab) or the request used an API key without JWT, these helpers
// are not called and behavior matches phases 1–2.

func isOwner(p idp.Principal, sb store.Sandbox) bool {
	sub := strings.TrimSpace(p.Sub)
	owner := strings.TrimSpace(sb.OwnerSub)
	return sub != "" && owner != "" && sub == owner
}

// canCreate: admin | operator (ADR matrix).
func canCreate(p idp.Principal) bool {
	return p.Role == idp.RoleAdmin || p.Role == idp.RoleOperator
}

// canList: any of admin | operator | viewer.
func canList(p idp.Principal) bool {
	return p.Role == idp.RoleAdmin || p.Role == idp.RoleOperator || p.Role == idp.RoleViewer
}

// canGet: owner | admin | operator | viewer.
func canGet(p idp.Principal, sb store.Sandbox) bool {
	if isOwner(p, sb) {
		return true
	}
	return p.Role == idp.RoleAdmin || p.Role == idp.RoleOperator || p.Role == idp.RoleViewer
}

// canExec: owner | admin | operator (viewer no).
func canExec(p idp.Principal, sb store.Sandbox) bool {
	if isOwner(p, sb) {
		return true
	}
	return p.Role == idp.RoleAdmin || p.Role == idp.RoleOperator
}

// canDestroy: owner | admin | operator-own | operator+DestroyAny (ADR default).
func canDestroy(p idp.Principal, sb store.Sandbox) bool {
	if isOwner(p, sb) {
		return true
	}
	if p.Role == idp.RoleAdmin {
		return true
	}
	if p.Role == idp.RoleOperator && p.DestroyAny {
		return true
	}
	return false
}

// canViewNodes: admin | operator. The node inventory is operations data, not tenant data.
func canViewNodes(p idp.Principal) bool {
	return p.Role == idp.RoleAdmin || p.Role == idp.RoleOperator
}

// canManageEgress: admin only (ADR matrix).
func canManageEgress(p idp.Principal) bool {
	return p.Role == idp.RoleAdmin
}

// filterSandboxesForList applies list visibility (documented phase-3 choice):
//
//	admin / operator / viewer → tenant-wide (no owner filter; tenant_id query still applies).
//
// Ownership gates exec/destroy, not list visibility within the tenant.
// ADR-0007 allows operator "todos o filtro"; we chose tenant-wide ("todos").
func filterSandboxesForList(p idp.Principal, list []store.Sandbox) []store.Sandbox {
	_ = p
	if list == nil {
		return []store.Sandbox{}
	}
	return list
}

func forbid(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusForbidden, msg)
}
