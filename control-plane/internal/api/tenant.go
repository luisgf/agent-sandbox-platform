package api

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// Tenant isolation. AuthMiddleware confines a request to a tenant when the
// caller has one (a tenant-scoped API key, an IdP principal); every handler
// that reads or changes tenant data checks it here. Node-agent calls (mTLS)
// are not tenants and are authorised by node identity instead.

// EnvDefaultTenant is the tenant of a create without tenant_id from a caller
// that sees every tenant (open lab, platform key). Default "default".
const EnvDefaultTenant = "ASP_DEFAULT_TENANT"

// authorizeTenant writes 403 and returns false when the request is confined
// to another tenant than the one it names (create, list filter, egress).
func authorizeTenant(w http.ResponseWriter, r *http.Request, tenantID string) bool {
	own, ok := CallerTenant(r.Context())
	if !ok || strings.TrimSpace(tenantID) == own {
		return true
	}
	writeError(w, http.StatusForbidden, fmt.Sprintf("forbidden: this caller acts within tenant %q, not %q", own, tenantID))
	return false
}

// sandboxVisible writes 404 and returns false when sb belongs to another
// tenant than the request's: the caller learns no more than for a sandbox
// that does not exist.
func sandboxVisible(w http.ResponseWriter, r *http.Request, sb store.Sandbox) bool {
	if _, isNode := NodeIdentityFromContext(r.Context()); isNode {
		return true
	}
	own, ok := CallerTenant(r.Context())
	if !ok || sb.TenantID == own {
		return true
	}
	writeError(w, http.StatusNotFound, "sandbox not found")
	return false
}

// createTenant is the tenant of a create that omits tenant_id: the caller's,
// else ASP_DEFAULT_TENANT ("default").
func createTenant(r *http.Request) string {
	if own, ok := CallerTenant(r.Context()); ok {
		return own
	}
	if t := strings.TrimSpace(os.Getenv(EnvDefaultTenant)); t != "" {
		return t
	}
	return "default"
}
