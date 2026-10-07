package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

type keyEnv struct {
	t   *testing.T
	h   http.Handler
	mem *store.MemoryStore
}

func newKeyEnv(t *testing.T) *keyEnv {
	t.Helper()
	mem := newTestStore(t, "n1")
	for _, k := range []struct{ tenant, name, scope, prefix, secret string }{
		{"default", "ops", store.APIKeyScopePlatform, "asp_ops1", "platform-key"},
		{"acme", "svc", store.APIKeyScopeTenant, "asp_acm1", "tenant-key"},
	} {
		if _, err := mem.EnsureAPIKey(context.Background(), k.tenant, k.name, k.scope, k.prefix, store.HashAPIKeySecret(k.secret)); err != nil {
			t.Fatal(err)
		}
	}
	return &keyEnv{t: t, h: AuthMiddleware(mem, AuthConfig{})(testMux(NewServer(mem))), mem: mem}
}

func (e *keyEnv) do(method, path, bearer, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr
}

func (e *keyEnv) create(bearer, body string) (apiKeyResponse, int) {
	e.t.Helper()
	rr := e.do("POST", "/v1/api-keys", bearer, body)
	var out apiKeyResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return out, rr.Code
}

// A platform key makes a key; the secret is returned once and works.
func TestCreateAPIKeyReturnsTheSecretOnce(t *testing.T) {
	e := newKeyEnv(t)
	k, code := e.create("platform-key", `{"tenant_id":"acme","name":"ci","ttl":"30d"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	if !strings.HasPrefix(k.Secret, "asp_") || k.Scope != store.APIKeyScopeTenant || k.TenantID != "acme" || k.Name != "ci" {
		t.Fatalf("created %+v", k)
	}
	if k.ExpiresAt == nil || time.Until(*k.ExpiresAt) < 29*24*time.Hour || time.Until(*k.ExpiresAt) > 31*24*time.Hour {
		t.Fatalf("expires_at = %v", k.ExpiresAt)
	}
	// It authenticates, confined to its tenant.
	if rr := e.do("GET", "/v1/sandboxes", k.Secret, ""); rr.Code != http.StatusOK {
		t.Fatalf("the new key does not work: %d %s", rr.Code, rr.Body.String())
	}
	// Listing never shows the secret or its hash.
	rr := e.do("GET", "/v1/api-keys?tenant_id=acme", "platform-key", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d", rr.Code)
	}
	body := rr.Body.String()
	if strings.Contains(body, k.Secret) || strings.Contains(body, store.HashAPIKeySecret(k.Secret)) || strings.Contains(body, "secret") {
		t.Fatalf("the list leaks the secret: %s", body)
	}
	var list listAPIKeysResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &list)
	if len(list.Keys) != 2 {
		t.Fatalf("acme has %d keys, want svc and ci", len(list.Keys))
	}
	// The same name twice is a conflict.
	if _, code := e.create("platform-key", `{"tenant_id":"acme","name":"ci"}`); code != http.StatusConflict {
		t.Fatalf("duplicate name: %d", code)
	}
}

func TestOnlyPlatformKeysAndIdPAdminsManageKeys(t *testing.T) {
	e := newKeyEnv(t)
	for _, tc := range []struct {
		name, method, path, bearer, body string
		want                             int
	}{
		{"tenant key creates", "POST", "/v1/api-keys", "tenant-key", `{"name":"x"}`, http.StatusForbidden},
		{"tenant key lists", "GET", "/v1/api-keys", "tenant-key", "", http.StatusForbidden},
		{"tenant key revokes", "DELETE", "/v1/api-keys/anything", "tenant-key", "", http.StatusForbidden},
		{"tenant key rotates", "POST", "/v1/api-keys/anything/rotate", "tenant-key", "", http.StatusForbidden},
		{"no credential creates", "POST", "/v1/api-keys", "", `{"name":"x"}`, http.StatusUnauthorized},
		{"no credential lists", "GET", "/v1/api-keys", "", "", http.StatusUnauthorized},
	} {
		if rr := e.do(tc.method, tc.path, tc.bearer, tc.body); rr.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, rr.Code, rr.Body.String(), tc.want)
		}
	}
	if keys, _ := e.mem.ListAPIKeys(context.Background(), ""); len(keys) != 2 {
		t.Fatalf("a refused request changed the keys: %d", len(keys))
	}
}

func TestCreateAPIKeyValidation(t *testing.T) {
	e := newKeyEnv(t)
	for name, body := range map[string]string{
		"no name":      `{}`,
		"bad name":     `{"name":"has space"}`,
		"leading dash": `{"name":"-x"}`,
		"long name":    `{"name":"` + strings.Repeat("a", 65) + `"}`,
		"bad scope":    `{"name":"x","scope":"root"}`,
		"bad ttl":      `{"name":"x","ttl":"soon"}`,
		"negative ttl": `{"name":"x","ttl":"-5h"}`,
		"huge ttl":     `{"name":"x","ttl":"99999d"}`,
		"bad tenant":   `{"name":"x","tenant_id":"a/b"}`,
		"not json":     `name=x`,
	} {
		if _, code := e.create("platform-key", body); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, code)
		}
	}
	// A platform key can make a platform key; default tenant for it is "default".
	k, code := e.create("platform-key", `{"name":"node-1","scope":"platform"}`)
	if code != http.StatusCreated || k.Scope != store.APIKeyScopePlatform || k.TenantID != "default" {
		t.Fatalf("platform key: %d %+v", code, k)
	}
}

func TestRotateAndRevokeAPIKey(t *testing.T) {
	e := newKeyEnv(t)
	k, _ := e.create("platform-key", `{"tenant_id":"acme","name":"ci"}`)

	rr := e.do("POST", "/v1/api-keys/"+k.ID+"/rotate", "platform-key", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rr.Code, rr.Body.String())
	}
	var rotated apiKeyResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &rotated)
	if rotated.ID != k.ID || rotated.Secret == "" || rotated.Secret == k.Secret {
		t.Fatalf("rotated %+v", rotated)
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Error("a response with a secret must not be cached")
	}
	if rr := e.do("GET", "/v1/sandboxes", k.Secret, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("the old secret still works: %d", rr.Code)
	}
	if rr := e.do("GET", "/v1/sandboxes", rotated.Secret, ""); rr.Code != http.StatusOK {
		t.Fatalf("the new secret does not work: %d", rr.Code)
	}

	rr = e.do("DELETE", "/v1/api-keys/"+k.ID, "platform-key", "")
	var revoked apiKeyResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &revoked)
	if rr.Code != http.StatusOK || revoked.RevokedAt == nil || revoked.Secret != "" {
		t.Fatalf("revoke: %d %s", rr.Code, rr.Body.String())
	}
	if rr := e.do("GET", "/v1/sandboxes", rotated.Secret, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked key still works: %d", rr.Code)
	}
	if rr := e.do("POST", "/v1/api-keys/"+k.ID+"/rotate", "platform-key", ""); rr.Code != http.StatusConflict {
		t.Fatalf("rotating a revoked key: %d %s", rr.Code, rr.Body.String())
	}
	for _, path := range []string{"/v1/api-keys/nope", "/v1/api-keys/nope/rotate"} {
		method := "DELETE"
		if strings.HasSuffix(path, "rotate") {
			method = "POST"
		}
		if rr := e.do(method, path, "platform-key", ""); rr.Code != http.StatusNotFound {
			t.Errorf("%s %s: %d, want 404", method, path, rr.Code)
		}
	}
}

// Revoking every key does not open the API.
func TestRevokingEveryKeyDoesNotOpenTheAPI(t *testing.T) {
	e := newKeyEnv(t)
	keys, _ := e.mem.ListAPIKeys(context.Background(), "")
	for _, k := range keys {
		if _, err := e.mem.RevokeAPIKey(context.Background(), k.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, bearer := range []string{"", "platform-key", "tenant-key"} {
		if rr := e.do("GET", "/v1/sandboxes", bearer, ""); rr.Code != http.StatusUnauthorized {
			t.Errorf("bearer %q after revoking everything: %d", bearer, rr.Code)
		}
	}
}

// An IdP admin manages the keys of their own tenant and nothing else.
func TestIdPAdminManagesOnlyTheirTenantsKeys(t *testing.T) {
	mem := newTestStore(t, "n1")
	if _, err := mem.EnsureAPIKey(context.Background(), "other", "svc", store.APIKeyScopeTenant, "asp_oth1", store.HashAPIKeySecret("other-key")); err != nil {
		t.Fatal(err)
	}
	key, kid, v := testIdP(t)
	h := AuthMiddleware(mem, AuthConfig{IdP: v})(testMux(NewServer(mem)))
	admin := mintUserJWTWithGroups(t, key, kid, "user:admin", "", []string{"asp-admin"})
	operator := mintUserJWTWithGroups(t, key, kid, "user:op", "", []string{"asp-operator"})
	do := func(method, path, bearer, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	// The admin's tenant ("t1", the test IdP's default) comes from the token;
	// creating without tenant_id uses it.
	rr := do("POST", "/v1/api-keys", admin, `{"name":"ci"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("admin create: %d %s", rr.Code, rr.Body.String())
	}
	var made apiKeyResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &made)
	if made.Scope != store.APIKeyScopeTenant || made.TenantID != "t1" {
		t.Fatalf("scope %q tenant %q", made.Scope, made.TenantID)
	}
	// Not another tenant, not a platform key.
	if rr := do("POST", "/v1/api-keys", admin, `{"name":"x","tenant_id":"other"}`); rr.Code != http.StatusForbidden {
		t.Fatalf("admin creating in another tenant: %d", rr.Code)
	}
	if rr := do("POST", "/v1/api-keys", admin, `{"name":"x","scope":"platform"}`); rr.Code != http.StatusForbidden {
		t.Fatalf("admin creating a platform key: %d", rr.Code)
	}
	// Lists are limited to their tenant and other tenants' keys are invisible.
	rr = do("GET", "/v1/api-keys", admin, "")
	var list listAPIKeysResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &list)
	for _, k := range list.Keys {
		if k.TenantID == "other" {
			t.Fatalf("an admin sees another tenant's key: %+v", k)
		}
	}
	if rr := do("GET", "/v1/api-keys?tenant_id=other", admin, ""); rr.Code != http.StatusForbidden {
		t.Fatalf("admin listing another tenant: %d", rr.Code)
	}
	others, _ := mem.ListAPIKeys(context.Background(), "other")
	if rr := do("DELETE", "/v1/api-keys/"+others[0].ID, admin, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("admin revoking another tenant's key: %d", rr.Code)
	}
	if rr := do("POST", "/v1/api-keys/"+made.ID+"/rotate", admin, ""); rr.Code != http.StatusOK {
		t.Fatalf("admin rotating their own key: %d", rr.Code)
	}
	// Lower roles may not.
	if rr := do("POST", "/v1/api-keys", operator, `{"name":"y"}`); rr.Code != http.StatusForbidden {
		t.Fatalf("operator creating: %d", rr.Code)
	}
}
