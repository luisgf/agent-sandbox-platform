package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/authn/idp"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/pki"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

type ctxKey int

const (
	apiKeyContextKey ctxKey = 1
	idpPrincipalKey  ctxKey = 2
	nodeIdentityKey  ctxKey = 3
	callerTenantKey  ctxKey = 4
)

// AuthConfig controls optional Bearer API-key middleware, IdP JWT (ADR-0007), and mTLS route policy.
type AuthConfig struct {
	// Require forces API-key auth even when no keys exist (ASP_REQUIRE_API_KEY=1).
	Require bool
	// RequireNodeClientCert enforces a verified peer cert on node agent routes
	// (register/heartbeat/oidc mint). Enrollment is exempt and uses the bootstrap token.
	// Used with TLS ClientAuth=VerifyClientCertIfGiven so enroll can proceed
	// with server-auth only over the same listener (unless ASP_MTLS_STRICT=1).
	RequireNodeClientCert bool
	// RejectRevokedCerts checks peer cert fingerprints against the store revocation set.
	RejectRevokedCerts bool
	// IdP validates corporate OIDC JWTs when non-nil (ASP_IDP_ISSUER configured).
	IdP *idp.Validator
	// IdPRequired forces a valid IdP JWT on user-facing routes (ASP_IDP_REQUIRED=1).
	IdPRequired bool
}

func AuthConfigFromEnv() AuthConfig {
	v := strings.TrimSpace(os.Getenv("ASP_REQUIRE_API_KEY"))
	clientCA := strings.TrimSpace(os.Getenv("ASP_CLIENT_CA"))
	return AuthConfig{
		Require:               v == "1" || strings.EqualFold(v, "true"),
		RequireNodeClientCert: clientCA != "",
		RejectRevokedCerts:    clientCA != "",
		IdPRequired:           idp.ConfigFromEnv().Required,
	}
}

// APIKeyFromContext returns the authenticated ApiKey if present.
func APIKeyFromContext(ctx context.Context) (store.ApiKey, bool) {
	k, ok := ctx.Value(apiKeyContextKey).(store.ApiKey)
	return k, ok
}

// IdPPrincipalFromContext returns the IdP JWT principal when present.
func IdPPrincipalFromContext(ctx context.Context) (idp.Principal, bool) {
	p, ok := ctx.Value(idpPrincipalKey).(idp.Principal)
	return p, ok
}

// NodeIdentityFromContext returns the node id proven by a verified mTLS client
// certificate (leaf CN with OU "nodes") on node-agent routes. It is absent in open
// lab and API-key mode, where node routes keep their pre-mTLS behaviour.
func NodeIdentityFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(nodeIdentityKey).(string)
	return id, ok && id != ""
}

// CallerTenant returns the tenant a request is confined to: the tenant of a
// tenant-scoped API key, or of an IdP principal. ok is false for callers that
// see every tenant: the open lab (no keys, no IdP) and platform-scoped keys.
func CallerTenant(ctx context.Context) (string, bool) {
	t, ok := ctx.Value(callerTenantKey).(string)
	return t, ok && t != ""
}

func withCallerTenant(ctx context.Context, tenant string) context.Context {
	return context.WithValue(ctx, callerTenantKey, tenant)
}

// publicPaths never require API keys.
func isPublicPath(path string) bool {
	switch path {
	case "/healthz",
		"/v1/nodes/enroll",
		"/.well-known/openid-configuration",
		"/oidc/jwks.json":
		return true
	default:
		return false
	}
}

// isRotateCertPath is POST /v1/nodes/{id}/rotate-cert, which also takes the
// node bootstrap token (a node re-keying itself).
func isRotateCertPath(path string) bool {
	return strings.HasPrefix(path, "/v1/nodes/") && strings.HasSuffix(path, "/rotate-cert")
}

// isNodeAdminPath is node administration: cordon, uncordon, revoke,
// rotate-cert and enroll tokens. Handlers allow an IdP admin or a
// platform-scoped API key.
func isNodeAdminPath(path string) bool {
	if path == "/v1/nodes/enroll-tokens" {
		return true
	}
	if !strings.HasPrefix(path, "/v1/nodes/") {
		return false
	}
	for _, suffix := range []string{"/cordon", "/uncordon", "/revoke", "/rotate-cert"} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

// isNodeAgentPath is protected by mTLS client certs when RequireNodeClientCert.
func isNodeAgentPath(path string) bool {
	if path == "/v1/nodes/register" || path == "/v1/internal/oidc/token" {
		return true
	}
	if strings.HasPrefix(path, "/v1/nodes/") && strings.HasSuffix(path, "/heartbeat") {
		return true
	}
	if strings.HasPrefix(path, "/v1/nodes/") && strings.HasSuffix(path, "/work") {
		return true
	}
	if strings.HasPrefix(path, "/v1/sandboxes/") && (strings.HasSuffix(path, "/claim") || strings.HasSuffix(path, "/status") || strings.HasSuffix(path, "/attest") || strings.HasSuffix(path, "/renew-lease") || strings.HasSuffix(path, "/local-net/node-public")) {
		return true
	}
	return false
}

// isUserFacingPath is the IdP JWT surface: sandbox create/list/get/exec/destroy/events,
// the node inventory and node administration. Node claim/status/attest/renew-lease stay on mTLS / internal auth.
func isUserFacingPath(path string) bool {
	if path == "/v1/sandboxes" || path == "/v1/nodes" || isNodeAdminPath(path) {
		return true
	}
	if !strings.HasPrefix(path, "/v1/sandboxes/") {
		return false
	}
	rest := strings.TrimPrefix(path, "/v1/sandboxes/")
	if rest == "" {
		return false
	}
	// node-internal suffixes
	for _, suf := range []string{"/claim", "/status", "/attest", "/renew-lease", "/local-net/node-public"} {
		if strings.HasSuffix(path, suf) {
			return false
		}
	}
	return true
}

// PeerCertFingerprint returns lowercase hex SHA-256 of the leaf peer cert DER, or "".
func PeerCertFingerprint(r *http.Request) string {
	if r == nil || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	sum := sha256.Sum256(r.TLS.PeerCertificates[0].Raw)
	return hex.EncodeToString(sum[:])
}

// keyCountTTL is how long the middleware trusts its count of API keys. Keys are
// created at start-up (ASP_BOOTSTRAP_API_KEY), so the count rarely changes.
const keyCountTTL = 10 * time.Second

// keyTouchEvery is the most often a key's last_used_at is written.
const keyTouchEvery = time.Minute

// keyCountCache saves the SELECT count(*) every request used to run.
type keyCountCache struct {
	mu sync.Mutex
	at time.Time
	n  int64
}

func (c *keyCountCache) get(s store.Store) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < keyCountTTL {
		return c.n, nil
	}
	n, err := s.CountAPIKeys()
	if err != nil {
		return 0, err
	}
	c.n, c.at = n, time.Now()
	return n, nil
}

// keyTouches writes last_used_at at most every keyTouchEvery per key, instead
// of an UPDATE on every request.
type keyTouches struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (k *keyTouches) touch(s store.Store, id string) {
	now := time.Now()
	k.mu.Lock()
	if t, ok := k.last[id]; ok && now.Sub(t) < keyTouchEvery {
		k.mu.Unlock()
		return
	}
	if k.last == nil {
		k.last = map[string]time.Time{}
	}
	k.last[id] = now
	k.mu.Unlock()
	_ = s.TouchAPIKey(id)
}

// AuthMiddleware enforces Bearer API keys when required or when any keys exist,
// and validates IdP JWTs when configured (ADR-0007 phase 2).
// /healthz, OIDC discovery/JWKS, and /v1/nodes/enroll are always public for API keys.
// When RequireNodeClientCert is set, node agent routes require a verified peer cert.
// When RejectRevokedCerts is set, revoked fingerprints are rejected with 401.
// Node mTLS routes never require human IdP JWTs.
func AuthMiddleware(s store.Store, cfg AuthConfig) func(http.Handler) http.Handler {
	keyCount := &keyCountCache{}
	touches := &keyTouches{}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if cfg.RejectRevokedCerts && r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
				fp := PeerCertFingerprint(r)
				revoked, err := s.IsCertRevoked(fp)
				if err != nil {
					writeError(w, http.StatusInternalServerError, "cert revocation check failed")
					return
				}
				if revoked {
					writeError(w, http.StatusUnauthorized, "client certificate revoked")
					return
				}
			}

			if cfg.RequireNodeClientCert && isNodeAgentPath(r.URL.Path) {
				if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
					writeError(w, http.StatusUnauthorized, "client certificate required")
					return
				}
				// The TLS listener verified the chain (ClientCAs). Bind the request to the
				// node the certificate names; handlers compare it with the node they act for.
				leaf := r.TLS.PeerCertificates[0]
				nodeID := strings.TrimSpace(leaf.Subject.CommonName)
				if nodeID == "" || !slices.Contains(leaf.Subject.OrganizationalUnit, pki.OUNodes) {
					writeError(w, http.StatusForbidden, "client certificate is not a node certificate")
					return
				}
				r = r.WithContext(context.WithValue(r.Context(), nodeIdentityKey, nodeID))
			}

			if isPublicPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			// rotate-cert also takes the node bootstrap token, which is not an API
			// key; the handler checks it again. Every other credential, and every
			// other node admin route, goes through the checks below.
			if isRotateCertPath(r.URL.Path) && checkBootstrapToken(r) {
				next.ServeHTTP(w, r)
				return
			}
			// Node agent routes authenticated via mTLS skip API key / IdP when client cert present.
			if cfg.RequireNodeClientCert && isNodeAgentPath(r.URL.Path) &&
				r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
				next.ServeHTTP(w, r)
				return
			}
			// Lab HTTP: allow oidc mint without API key (mTLS optional); still not public to internet.
			if r.URL.Path == "/v1/internal/oidc/token" && !cfg.Require {
				n, _ := keyCount.get(s)
				if n == 0 {
					next.ServeHTTP(w, r)
					return
				}
			}

			raw := bearerToken(r.Header.Get("Authorization"))

			// IdP JWT path: when validator configured and bearer looks like a JWT,
			// validate and attach principal. Node-agent paths never take this branch
			// when mTLS already short-circuited above; without mTLS, node paths still
			// prefer API key / open lab (JWT not required).
			if cfg.IdP != nil && idp.LooksLikeJWT(raw) && !isNodeAgentPath(r.URL.Path) {
				p, err := cfg.IdP.Validate(raw)
				if err != nil {
					writeError(w, http.StatusUnauthorized, "invalid idp token")
					return
				}
				if strings.TrimSpace(p.TenantID) == "" {
					// Without a tenant the principal would see every tenant.
					writeError(w, http.StatusUnauthorized, "idp token names no tenant: add the tenant claim (ASP_IDP_TENANT_CLAIM) or set ASP_IDP_DEFAULT_TENANT")
					return
				}
				ctx := context.WithValue(r.Context(), idpPrincipalKey, p)
				ctx = withCallerTenant(ctx, strings.TrimSpace(p.TenantID))
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			// ASP_IDP_REQUIRED: user-facing routes need a validated IdP JWT.
			if cfg.IdPRequired && isUserFacingPath(r.URL.Path) {
				if cfg.IdP == nil {
					writeError(w, http.StatusUnauthorized, "idp not configured")
					return
				}
				writeError(w, http.StatusUnauthorized, "missing or invalid idp bearer token")
				return
			}

			n, err := keyCount.get(s)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "auth store error")
				return
			}
			enabled := cfg.Require || n > 0
			if !enabled {
				next.ServeHTTP(w, r)
				return
			}
			if raw == "" {
				writeError(w, http.StatusUnauthorized, "missing bearer token")
				return
			}
			hash := store.HashAPIKeySecret(raw)
			key, err := s.LookupAPIKeyByHash(hash)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "invalid api key")
				return
			}
			touches.touch(s, key.ID)
			ctx := context.WithValue(r.Context(), apiKeyContextKey, key)
			if key.Scope != store.APIKeyScopePlatform {
				ctx = withCallerTenant(ctx, key.TenantID)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func bearerToken(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// BootstrapAPIKey ensures the key ASP_BOOTSTRAP_API_KEY names: the operator's
// key, platform-scoped (it sees every tenant) in tenant "default". A lab can
// make it a tenant key with ASP_BOOTSTRAP_API_KEY_SCOPE=tenant and pick the
// tenant with ASP_BOOTSTRAP_API_KEY_TENANT.
func BootstrapAPIKey(s store.Store, secret string) (store.ApiKey, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return store.ApiKey{}, nil
	}
	tenant := strings.TrimSpace(os.Getenv("ASP_BOOTSTRAP_API_KEY_TENANT"))
	if tenant == "" {
		tenant = "default"
	}
	scope := strings.TrimSpace(os.Getenv("ASP_BOOTSTRAP_API_KEY_SCOPE"))
	if scope == "" {
		scope = store.APIKeyScopePlatform
	}
	return s.EnsureAPIKey(
		tenant,
		"bootstrap",
		scope,
		store.KeyPrefix(secret),
		store.HashAPIKeySecret(secret),
	)
}

// EnvTruthy reports ASP-style truthy env values.
func EnvTruthy(key string) bool {
	v := strings.TrimSpace(os.Getenv(key))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}
