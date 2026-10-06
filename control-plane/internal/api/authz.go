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

// canManageNodes: admin only (cordon, uncordon, revoke, rotate-cert).
func canManageNodes(p idp.Principal) bool {
	return p.Role == idp.RoleAdmin
}

// authorizeNodeAdmin lets through callers that may administer nodes (cordon,
// uncordon, revoke, rotate-cert): an IdP admin or a platform-scoped API key.
// A tenant-scoped key never may: every tenant shares the nodes. A request
// without any identity only reaches a handler in the open lab (no API keys,
// no IdP), where it is allowed unless credentialRequired: rotate-cert sets it
// because it hands out a node's private key.
func authorizeNodeAdmin(w http.ResponseWriter, r *http.Request, action string, credentialRequired bool) bool {
	ctx := r.Context()
	if p, ok := IdPPrincipalFromContext(ctx); ok {
		if canManageNodes(p) {
			return true
		}
		forbid(w, "admin role required to "+action)
		return false
	}
	if key, ok := APIKeyFromContext(ctx); ok {
		if key.Scope == store.APIKeyScopePlatform {
			return true
		}
		forbid(w, "a platform-scoped api key is required to "+action+": tenant keys cannot manage the nodes every tenant shares")
		return false
	}
	if credentialRequired {
		writeError(w, http.StatusUnauthorized, "an idp admin token, a platform api key or the node bootstrap token is required to "+action)
		return false
	}
	return true
}

// authorizeNodeView lets through callers that may list nodes: an IdP admin
// or operator, or a platform-scoped API key (open lab: anyone).
func authorizeNodeView(w http.ResponseWriter, r *http.Request) bool {
	ctx := r.Context()
	if p, ok := IdPPrincipalFromContext(ctx); ok {
		if canViewNodes(p) {
			return true
		}
		forbid(w, "admin or operator role required to list nodes")
		return false
	}
	if key, ok := APIKeyFromContext(ctx); ok && key.Scope != store.APIKeyScopePlatform {
		forbid(w, "a platform-scoped api key is required to list nodes: the inventory spans every tenant")
		return false
	}
	return true
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
