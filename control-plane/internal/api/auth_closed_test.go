package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// Authentication is always on: with no key, no IdP and no explicit opt-out, no
// route that does anything answers a request without a credential (#95).
func TestNothingIsOpenByDefault(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	mem := newTestStore(t, "n1")
	h := AuthMiddleware(mem, AuthConfig{})(testMux(NewServer(mem)))
	for _, route := range [][2]string{
		{"POST", "/v1/sandboxes"}, {"GET", "/v1/sandboxes"}, {"GET", "/v1/sandboxes/x"},
		{"POST", "/v1/sandboxes/x/exec"}, {"POST", "/v1/sandboxes/x/exec/stdin"}, {"DELETE", "/v1/sandboxes/x"},
		{"GET", "/v1/nodes"}, {"POST", "/v1/nodes/n1/cordon"}, {"POST", "/v1/nodes/enroll-tokens"},
		{"POST", "/v1/nodes/register"}, {"POST", "/v1/nodes/n1/heartbeat"}, {"GET", "/v1/nodes/n1/work"},
		{"POST", "/v1/sandboxes/x/claim"}, {"POST", "/v1/sandboxes/x/status"}, {"POST", "/v1/sandboxes/x/attest"},
		{"PUT", "/v1/tenants/default/egress"}, {"POST", "/v1/tenants/default/egress/check"},
		{"POST", "/v1/attestation/verify"}, {"POST", "/v1/internal/oidc/token"},
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(route[0], route[1], bytes.NewBufferString(`{}`)))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no credential: %d %s, want 401", route[0], route[1], rr.Code, rr.Body.String())
		}
	}
	// What has to be public still is.
	for _, path := range []string{"/healthz", "/.well-known/openid-configuration", "/oidc/jwks.json"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code == http.StatusUnauthorized {
			t.Errorf("%s is public but answered 401", path)
		}
	}
	// A bearer that is no key is no credential either: with zero keys, nobody gets in.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer made-up")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("an unknown bearer with zero keys: %d", rr.Code)
	}
}

// The old behaviour, on purpose and only on request.
func TestInsecureOpenAllowsRequestsWithNoCredential(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	mem := newTestStore(t, "n1")
	if _, err := mem.EnsureAPIKey(context.Background(), "t1", "k", store.APIKeyScopeTenant, "asp_k", store.HashAPIKeySecret("tenant-key")); err != nil {
		t.Fatal(err)
	}
	h := AuthMiddleware(mem, AuthConfig{InsecureOpen: true})(testMux(NewServer(mem)))
	do := func(bearer string) int {
		req := httptest.NewRequest("GET", "/v1/sandboxes", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	if got := do(""); got != http.StatusOK {
		t.Fatalf("no credential, open on purpose: %d", got)
	}
	if got := do("tenant-key"); got != http.StatusOK {
		t.Fatalf("a valid key still works: %d", got)
	}
	// A credential that is wrong is refused even in the open lab.
	if got := do("wrong"); got != http.StatusUnauthorized {
		t.Fatalf("a wrong credential in the open lab: %d", got)
	}
}

// Removing the last key leaves the API closed: it never falls back to open.
func TestLosingTheLastKeyDoesNotOpenTheAPI(t *testing.T) {
	mem := newTestStore(t, "n1")
	if _, err := mem.EnsureAPIKey(context.Background(), "default", "ops", store.APIKeyScopePlatform, "asp_ops", store.HashAPIKeySecret("platform-key")); err != nil {
		t.Fatal(err)
	}
	h := AuthMiddleware(mem, AuthConfig{})(testMux(NewServer(mem)))
	req := httptest.NewRequest("GET", "/v1/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer platform-key")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("with the key: %d", rr.Code)
	}
	// A different store with no key at all: the same middleware refuses.
	empty := newTestStore(t, "n1")
	h2 := AuthMiddleware(empty, AuthConfig{})(testMux(NewServer(empty)))
	for _, bearer := range []string{"", "platform-key"} {
		req := httptest.NewRequest("GET", "/v1/sandboxes", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rr := httptest.NewRecorder()
		h2.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("zero keys, bearer %q: %d", bearer, rr.Code)
		}
	}
}

// A node authenticates with its certificate or, over plain HTTP, a platform
// key. A tenant's key is not a node.
func TestNodeRoutesNeedAPlatformKeyOrACertificate(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	mem := newTestStore(t, "n1")
	for _, k := range []struct{ tenant, name, scope, prefix, secret string }{
		{"default", "node", store.APIKeyScopePlatform, "asp_node", "node-key"},
		{"t1", "svc", store.APIKeyScopeTenant, "asp_svc1", "tenant-key"},
	} {
		if _, err := mem.EnsureAPIKey(context.Background(), k.tenant, k.name, k.scope, k.prefix, store.HashAPIKeySecret(k.secret)); err != nil {
			t.Fatal(err)
		}
	}
	h := AuthMiddleware(mem, AuthConfig{})(testMux(NewServer(mem)))
	for _, route := range [][2]string{
		{"GET", "/v1/nodes/n1/work"}, {"POST", "/v1/nodes/n1/heartbeat"}, {"POST", "/v1/nodes/register"},
		{"POST", "/v1/sandboxes/x/claim"}, {"POST", "/v1/sandboxes/x/status"}, {"POST", "/v1/internal/oidc/token"},
	} {
		for bearer, check := range map[string]func(int) bool{
			"tenant-key": func(c int) bool { return c == http.StatusForbidden },
			"node-key":   func(c int) bool { return c != http.StatusUnauthorized && c != http.StatusForbidden },
			"":           func(c int) bool { return c == http.StatusUnauthorized },
		} {
			req := httptest.NewRequest(route[0], route[1], bytes.NewBufferString(`{}`))
			if bearer != "" {
				req.Header.Set("Authorization", "Bearer "+bearer)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if !check(rr.Code) {
				t.Errorf("%s %s with %q: %d %s", route[0], route[1], bearer, rr.Code, rr.Body.String())
			}
		}
	}
}
