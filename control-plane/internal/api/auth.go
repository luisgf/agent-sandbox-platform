package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"strings"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/authn/idp"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

type ctxKey int

const (
	apiKeyContextKey ctxKey = 1
	idpPrincipalKey  ctxKey = 2
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
	// IdPRequired forces a valid IdP JWT on user-facing sandbox routes (ASP_IDP_REQUIRED=1).
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

// isNodeCertAdminPath is authorized inside the handler (bootstrap token or API key).
// Middleware must not require a stored API key alone, or bootstrap-only ops break.
func isNodeCertAdminPath(path string) bool {
	if !strings.HasPrefix(path, "/v1/nodes/") {
		return false
	}
	return strings.HasSuffix(path, "/rotate-cert") || strings.HasSuffix(path, "/revoke")
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

// isUserFacingSandboxPath is the IdP JWT surface (create/list/get/exec/destroy/events).
// Node claim/status/attest/renew-lease stay on mTLS / internal auth.
func isUserFacingSandboxPath(path string) bool {
	if path == "/v1/sandboxes" {
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

// AuthMiddleware enforces Bearer API keys when required or when any keys exist,
// and validates IdP JWTs when configured (ADR-0007 phase 2).
// /healthz, OIDC discovery/JWKS, and /v1/nodes/enroll are always public for API keys.
// When RequireNodeClientCert is set, node agent routes require a verified peer cert.
// When RejectRevokedCerts is set, revoked fingerprints are rejected with 401.
// Node mTLS routes never require human IdP JWTs.
func AuthMiddleware(s store.Store, cfg AuthConfig) func(http.Handler) http.Handler {
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
			}

			if isPublicPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			// rotate-cert / revoke: handler checks bootstrap token or API key.
			if isNodeCertAdminPath(r.URL.Path) {
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
				n, _ := s.CountAPIKeys()
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
				ctx := context.WithValue(r.Context(), idpPrincipalKey, p)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			// ASP_IDP_REQUIRED: user-facing sandbox routes need a validated IdP JWT.
			if cfg.IdPRequired && isUserFacingSandboxPath(r.URL.Path) {
				if cfg.IdP == nil {
					writeError(w, http.StatusUnauthorized, "idp not configured")
					return
				}
				writeError(w, http.StatusUnauthorized, "missing or invalid idp bearer token")
				return
			}

			n, err := s.CountAPIKeys()
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
			_ = s.TouchAPIKey(key.ID)
			ctx := context.WithValue(r.Context(), apiKeyContextKey, key)
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

// BootstrapAPIKey ensures a key for tenant "default" when ASP_BOOTSTRAP_API_KEY is set.
func BootstrapAPIKey(s store.Store, secret string) (store.ApiKey, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return store.ApiKey{}, nil
	}
	return s.EnsureAPIKey(
		"default",
		"bootstrap",
		store.KeyPrefix(secret),
		store.HashAPIKeySecret(secret),
	)
}

// EnvTruthy reports ASP-style truthy env values.
func EnvTruthy(key string) bool {
	v := strings.TrimSpace(os.Getenv(key))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}
