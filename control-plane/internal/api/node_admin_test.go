package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/pki"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// nodeAdminRoutes are the node routes no tenant may use. An empty suffix is
// GET /v1/nodes.
var nodeAdminRoutes = []string{"/rotate-cert", "/revoke", "/cordon", "/uncordon", ""}

func nodeAdminServer(t *testing.T, cfg AuthConfig) (http.Handler, *store.MemoryStore) {
	t.Helper()
	t.Setenv("ASP_NODE_BOOTSTRAP_TOKEN", "boot-secret")
	ca, err := pki.GenerateCA("test", pki.DefaultNodeTTL)
	if err != nil {
		t.Fatal(err)
	}
	mem := store.NewMemoryStore()
	srv := NewServer(mem)
	srv.CA = ca
	return AuthMiddleware(mem, cfg)(testMux(srv)), mem
}

// callNodeRoute registers a fresh node and calls one admin route on it, so
// one call's revoke never changes what the next one sees.
func callNodeRoute(t *testing.T, h http.Handler, mem *store.MemoryStore, node, suffix, token string) *httptest.ResponseRecorder {
	t.Helper()
	if _, err := mem.RegisterNode(store.RegisterNodeInput{ID: node, AgentEndpoint: "http://127.0.0.1:9100"}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/nodes/"+node+suffix, nil)
	if suffix == "" {
		req = httptest.NewRequest(http.MethodGet, "/v1/nodes", nil)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestNodeAdministrationNeedsAnAdminOrAPlatformKey(t *testing.T) {
	key, kid, v := testIdP(t)
	h, mem := nodeAdminServer(t, AuthConfig{IdP: v})
	if _, err := mem.EnsureAPIKey("t1", "tenant", store.APIKeyScopeTenant, "asp_tenant", store.HashAPIKeySecret("tenant-key")); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.EnsureAPIKey("default", "ops", store.APIKeyScopePlatform, "asp_ops", store.HashAPIKeySecret("platform-key")); err != nil {
		t.Fatal(err)
	}

	const ok, forbidden, unauthorized = http.StatusOK, http.StatusForbidden, http.StatusUnauthorized
	nodes := 0
	for _, tc := range []struct {
		caller, token string
		want          [5]int // rotate-cert, revoke, cordon, uncordon, list
	}{
		{"tenant api key", "tenant-key", [5]int{forbidden, forbidden, forbidden, forbidden, forbidden}},
		{"platform api key", "platform-key", [5]int{ok, ok, ok, ok, ok}},
		{"idp admin", mintUserJWTWithGroups(t, key, kid, "user:admin", "", []string{"asp-admin"}), [5]int{ok, ok, ok, ok, ok}},
		{"idp operator", mintUserJWTWithGroups(t, key, kid, "user:op", "", []string{"asp-operator"}), [5]int{forbidden, forbidden, forbidden, forbidden, ok}},
		{"idp viewer", mintUserJWTWithGroups(t, key, kid, "user:view", "", []string{"asp-viewer"}), [5]int{forbidden, forbidden, forbidden, forbidden, forbidden}},
		// Every node holds the bootstrap token: it enrolls, and does nothing else.
		{"bootstrap token", "boot-secret", [5]int{unauthorized, unauthorized, unauthorized, unauthorized, unauthorized}},
		{"no credential", "", [5]int{unauthorized, unauthorized, unauthorized, unauthorized, unauthorized}},
	} {
		for i, suffix := range nodeAdminRoutes {
			nodes++
			rr := callNodeRoute(t, h, mem, fmt.Sprintf("node-%d", nodes), suffix, tc.token)
			if rr.Code != tc.want[i] {
				t.Errorf("%s on %q: want %d, got %d %s", tc.caller, suffix, tc.want[i], rr.Code, rr.Body.String())
			}
		}
	}
}

// The open lab (no API keys, no IdP) keeps node administration open, except
// rotate-cert: it hands out a node's private key.
func TestNodeAdministrationInTheOpenLab(t *testing.T) {
	h, mem := nodeAdminServer(t, AuthConfig{})
	for i, suffix := range []string{"/revoke", "/cordon", "/uncordon", ""} {
		if rr := callNodeRoute(t, h, mem, fmt.Sprintf("lab-%d", i), suffix, ""); rr.Code != http.StatusOK {
			t.Errorf("open lab %q: want 200, got %d %s", suffix, rr.Code, rr.Body.String())
		}
	}
	for i, token := range []string{"", "boot-secret"} {
		if rr := callNodeRoute(t, h, mem, fmt.Sprintf("lab-rotate%d", i), "/rotate-cert", token); rr.Code != http.StatusUnauthorized {
			t.Fatalf("open lab rotate-cert with token %q: want 401, got %d %s", token, rr.Code, rr.Body.String())
		}
	}
}

// With ASP_IDP_REQUIRED node administration needs an IdP admin; the bootstrap
// token does not re-key a node.
func TestNodeAdministrationWithIdPRequired(t *testing.T) {
	_, _, v := testIdP(t)
	h, mem := nodeAdminServer(t, AuthConfig{IdP: v, IdPRequired: true})
	if _, err := mem.EnsureAPIKey("default", "ops", store.APIKeyScopePlatform, "asp_ops", store.HashAPIKeySecret("platform-key")); err != nil {
		t.Fatal(err)
	}
	if rr := callNodeRoute(t, h, mem, "idp-1", "/revoke", "platform-key"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("platform key with ASP_IDP_REQUIRED: want 401, got %d", rr.Code)
	}
	if rr := callNodeRoute(t, h, mem, "idp-2", "/rotate-cert", "boot-secret"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bootstrap rotate-cert with ASP_IDP_REQUIRED: want 401, got %d %s", rr.Code, rr.Body.String())
	}
}
