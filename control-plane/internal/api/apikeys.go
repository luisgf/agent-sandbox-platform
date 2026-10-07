package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// API key lifecycle: create, list, revoke and rotate keys through the API
// instead of editing the database. Who may:
//
//   - a platform-scoped key, or an IdP principal with no tenant confinement
//     concept (none today): any tenant, any scope;
//   - an IdP admin: keys of their own tenant, tenant-scoped only.
//
// A tenant key, and every IdP role below admin, may not manage keys. The
// secret is returned once, by create and rotate, and never stored or logged.

var apiKeyNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

const maxAPIKeyTTL = 5 * 365 * 24 * time.Hour

type createAPIKeyRequest struct {
	// TenantID is the tenant the key belongs to. Default: the caller's, or
	// "default" for a platform key.
	TenantID string `json:"tenant_id,omitempty"`
	Name     string `json:"name"`
	// Scope is "tenant" (default) or "platform" (every tenant, node routes;
	// only a platform key can make one).
	Scope string `json:"scope,omitempty"`
	// TTL is how long the key lasts: a Go duration ("720h") or days ("30d").
	// Empty: it does not expire.
	TTL string `json:"ttl,omitempty"`
}

// apiKeyResponse is a key and, only right after create or rotate, its secret.
type apiKeyResponse struct {
	store.ApiKey
	Secret string `json:"secret,omitempty"`
}

type listAPIKeysResponse struct {
	Keys []store.ApiKey `json:"keys"`
}

// parseKeyTTL reads "720h", "90m" or "30d".
func parseKeyTTL(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("ttl %q: want a Go duration (720h) or days (30d)", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("ttl %q: want a Go duration (720h) or days (30d)", s)
	}
	return d, nil
}

// authorizeKeyAdmin lets through a platform key or an IdP admin; it reports
// the tenant the caller is confined to ("" for a platform key).
func authorizeKeyAdmin(w http.ResponseWriter, r *http.Request) (confinedTenant string, ok bool) {
	ctx := r.Context()
	if p, has := IdPPrincipalFromContext(ctx); has {
		if !canManageNodes(p) { // admin role
			forbid(w, "admin role required to manage api keys")
			return "", false
		}
		t, _ := CallerTenant(ctx)
		return t, true
	}
	if key, has := APIKeyFromContext(ctx); has {
		if key.Scope == store.APIKeyScopePlatform {
			return "", true
		}
		forbid(w, "a platform-scoped api key is required to manage api keys: a tenant key cannot mint or revoke keys")
		return "", false
	}
	writeError(w, http.StatusUnauthorized, "an idp admin token or a platform api key is required to manage api keys")
	return "", false
}

func logKeyChange(r *http.Request, what string, k store.ApiKey) {
	// Never the secret, never its hash.
	slog.Info("api key "+what, "id", k.ID, "tenant", k.TenantID, "name", k.Name, "scope", k.Scope, "prefix", k.KeyPrefix,
		"actor_sub", resolveActorSub(r, "", ""))
}

// CreateAPIKey makes a new key and returns its secret once.
func (s *Server) CreateAPIKey(w http.ResponseWriter, r *http.Request) {
	confined, ok := authorizeKeyAdmin(w, r)
	if !ok {
		return
	}
	var req createAPIKeyRequest
	if !decodeJSON(w, r, maxSmallBodyBytes, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if !apiKeyNameRE.MatchString(req.Name) {
		writeError(w, http.StatusBadRequest, "name must be 1-64 characters of letters, digits, '.', '_' or '-', starting with a letter or digit")
		return
	}
	tenant := strings.TrimSpace(req.TenantID)
	if tenant == "" {
		tenant = confined
	}
	if tenant == "" {
		tenant = "default"
	}
	if strings.ContainsAny(tenant, " /\x00\n") || len(tenant) > 128 {
		writeError(w, http.StatusBadRequest, "tenant_id is not a valid tenant id")
		return
	}
	scope := strings.TrimSpace(req.Scope)
	if scope == "" {
		scope = store.APIKeyScopeTenant
	}
	if confined != "" && (tenant != confined || scope != store.APIKeyScopeTenant) {
		forbid(w, "an idp admin can create tenant-scoped keys for their own tenant only")
		return
	}
	ttl, err := parseKeyTTL(req.TTL)
	if err != nil || ttl > maxAPIKeyTTL {
		msg := "ttl must be at most 5 years"
		if err != nil {
			msg = err.Error()
		}
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	var expires *time.Time
	if ttl > 0 {
		t := time.Now().UTC().Add(ttl)
		expires = &t
	}
	existing, err := s.Store.ListAPIKeys(r.Context(), tenant)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, k := range existing {
		if k.Name == req.Name {
			writeError(w, http.StatusConflict, fmt.Sprintf("tenant %s already has an api key named %q (revoke or rotate it)", tenant, req.Name))
			return
		}
	}
	// The display prefix is unique per key: a clash is a bad draw, not an error.
	for attempt := 0; attempt < 8; attempt++ {
		secret, err := store.GenerateAPIKeySecret()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "generate secret: "+err.Error())
			return
		}
		key, err := s.Store.CreateAPIKey(r.Context(), tenant, req.Name, scope, store.KeyPrefix(secret), store.HashAPIKeySecret(secret), expires)
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		if err != nil {
			if errors.Is(err, store.ErrInvalidInput) {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		logKeyChange(r, "created", key)
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusCreated, apiKeyResponse{ApiKey: key, Secret: secret})
		return
	}
	writeError(w, http.StatusConflict, "could not find a free key prefix; try again")
}

// ListAPIKeys lists keys, without secrets. A tenant-confined admin sees their own.
func (s *Server) ListAPIKeys(w http.ResponseWriter, r *http.Request) {
	confined, ok := authorizeKeyAdmin(w, r)
	if !ok {
		return
	}
	tenant := strings.TrimSpace(r.URL.Query().Get("tenant_id"))
	if confined != "" {
		if tenant != "" && tenant != confined {
			forbid(w, "an idp admin lists the keys of their own tenant only")
			return
		}
		tenant = confined
	}
	keys, err := s.Store.ListAPIKeys(r.Context(), tenant)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, listAPIKeysResponse{Keys: keys})
}

// keyForAdmin loads the key {id}, hiding other tenants' keys from a confined admin.
func (s *Server) keyForAdmin(w http.ResponseWriter, r *http.Request, confined string) (store.ApiKey, bool) {
	id := strings.TrimSpace(r.PathValue("id"))
	k, err := s.Store.GetAPIKey(r.Context(), id)
	if err != nil || (confined != "" && k.TenantID != confined) {
		if err == nil || errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "api key not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return store.ApiKey{}, false
	}
	return k, true
}

// RevokeAPIKey stops a key from authenticating. The row stays, marked revoked.
func (s *Server) RevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	confined, ok := authorizeKeyAdmin(w, r)
	if !ok {
		return
	}
	k, ok := s.keyForAdmin(w, r, confined)
	if !ok {
		return
	}
	revoked, err := s.Store.RevokeAPIKey(r.Context(), k.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logKeyChange(r, "revoked", revoked)
	writeJSON(w, http.StatusOK, apiKeyResponse{ApiKey: revoked})
}

// RotateAPIKey gives a key a new secret, returned once; the old one stops at once.
func (s *Server) RotateAPIKey(w http.ResponseWriter, r *http.Request) {
	confined, ok := authorizeKeyAdmin(w, r)
	if !ok {
		return
	}
	k, ok := s.keyForAdmin(w, r, confined)
	if !ok {
		return
	}
	for attempt := 0; attempt < 8; attempt++ {
		secret, err := store.GenerateAPIKeySecret()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "generate secret: "+err.Error())
			return
		}
		rotated, err := s.Store.RotateAPIKey(r.Context(), k.ID, store.KeyPrefix(secret), store.HashAPIKeySecret(secret))
		if errors.Is(err, store.ErrConflict) && rotatedIsRevoked(r.Context(), s.Store, k.ID) {
			writeError(w, http.StatusConflict, "api key is revoked: create a new one")
			return
		}
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		logKeyChange(r, "rotated", rotated)
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, apiKeyResponse{ApiKey: rotated, Secret: secret})
		return
	}
	writeError(w, http.StatusConflict, "could not find a free key prefix; try again")
}

func rotatedIsRevoked(ctx context.Context, st store.Store, id string) bool {
	k, err := st.GetAPIKey(ctx, id)
	return err == nil && k.RevokedAt != nil
}
