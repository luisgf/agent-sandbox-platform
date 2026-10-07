package api

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/authn/idp"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/oidc"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

const (
	keyTenantA  = "secret-tenant-a"
	keyTenantB  = "secret-tenant-b"
	keyPlatform = "secret-platform"
)

type tenantFixture struct {
	t        *testing.T
	mem      *store.MemoryStore
	h        http.Handler
	sbA, sbB string
}

func newTenantFixture(t *testing.T, cfg AuthConfig) *tenantFixture {
	t.Helper()
	mem := newTestStore(t, "n1")
	for _, k := range []struct{ tenant, name, scope, secret string }{
		{"tenant-a", "a", store.APIKeyScopeTenant, keyTenantA},
		{"tenant-b", "b", store.APIKeyScopeTenant, keyTenantB},
		{"default", "ops", store.APIKeyScopePlatform, keyPlatform},
	} {
		if _, err := mem.EnsureAPIKey(k.tenant, k.name, k.scope, store.KeyPrefix(k.secret), store.HashAPIKeySecret(k.secret)); err != nil {
			t.Fatal(err)
		}
	}
	srv := NewServer(mem)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv.OIDC = oidc.NewSignerFromKey(key, "")
	f := &tenantFixture{t: t, mem: mem, h: AuthMiddleware(mem, cfg)(testMux(srv))}
	f.sbA = f.sandbox("tenant-a")
	f.sbB = f.sandbox("tenant-b")
	return f
}

func (f *tenantFixture) sandbox(tenant string) string {
	f.t.Helper()
	sb, err := f.mem.CreateSandbox(store.CreateSandboxInput{TenantID: tenant, ImageRef: "img", CPUMillis: 100, MemoryMiB: 64})
	if err != nil {
		f.t.Fatal(err)
	}
	runSandbox(f.t, f.mem, sb.ID)
	return sb.ID
}

func (f *tenantFixture) do(bearer, method, path, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rr := httptest.NewRecorder()
	f.h.ServeHTTP(rr, req)
	return rr
}

// A tenant-scoped API key reaches nothing of another tenant: sandboxes look as
// if they did not exist (404), and naming the other tenant is refused (403).
func TestTenantKeyCannotReachAnotherTenant(t *testing.T) {
	f := newTenantFixture(t, AuthConfig{})
	b := "/v1/sandboxes/" + f.sbB
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, b, "", http.StatusNotFound},
		{http.MethodGet, b + "/events", "", http.StatusNotFound},
		{http.MethodGet, b + "/attestation", "", http.StatusNotFound},
		{http.MethodPost, b + "/exec", `{"cmd":["true"]}`, http.StatusNotFound},
		{http.MethodPost, b + "/exec/stdin", `{"exec_id":"e1","data":"x"}`, http.StatusNotFound},
		{http.MethodPost, b + "/local-net/grant", `{}`, http.StatusNotFound},
		{http.MethodPost, b + "/local-net/heartbeat", `{"grant":"g","client_public_key":"k"}`, http.StatusNotFound},
		{http.MethodDelete, b + "/local-net/attach", "", http.StatusNotFound},
		{http.MethodDelete, b, "", http.StatusNotFound},
		// Minting a workload token is a node route: a tenant key is refused outright.
		{http.MethodPost, "/v1/internal/oidc/token", `{"sandbox_id":"` + f.sbB + `","aud":"x"}`, http.StatusForbidden},
		{http.MethodPost, "/v1/sandboxes", `{"tenant_id":"tenant-b","image_ref":"img","cpu_millis":100,"memory_mib":64}`, http.StatusForbidden},
		{http.MethodGet, "/v1/sandboxes?tenant_id=tenant-b", "", http.StatusForbidden},
		{http.MethodGet, "/v1/tenants/tenant-b/egress", "", http.StatusForbidden},
		{http.MethodPut, "/v1/tenants/tenant-b/egress", `{"rules":[]}`, http.StatusForbidden},
		{http.MethodPost, "/v1/tenants/tenant-b/egress/check", `{"host":"example.com"}`, http.StatusForbidden},
	} {
		if rr := f.do(keyTenantA, c.method, c.path, c.body); rr.Code != c.want {
			t.Errorf("%s %s with tenant-a key: want %d, got %d %s", c.method, c.path, c.want, rr.Code, rr.Body.String())
		}
	}
	if sb, err := f.mem.GetSandbox(f.sbB); err != nil || sb.State != store.SandboxRunning {
		t.Fatalf("tenant-b sandbox changed by a tenant-a caller: %+v %v", sb, err)
	}
}

func TestTenantKeyWorksWithinItsTenant(t *testing.T) {
	f := newTenantFixture(t, AuthConfig{})
	a := "/v1/sandboxes/" + f.sbA
	if rr := f.do(keyTenantA, http.MethodGet, a, ""); rr.Code != http.StatusOK {
		t.Fatalf("own sandbox: %d %s", rr.Code, rr.Body.String())
	}
	if rr := f.do(keyTenantA, http.MethodGet, "/v1/tenants/tenant-a/egress", ""); rr.Code != http.StatusOK {
		t.Fatalf("own egress: %d %s", rr.Code, rr.Body.String())
	}
	// Minting is what a node does for its guests, not something a tenant key does.
	if rr := f.do(keyTenantA, http.MethodPost, "/v1/internal/oidc/token", `{"sandbox_id":"`+f.sbA+`","aud":"x"}`); rr.Code != http.StatusForbidden {
		t.Fatalf("a tenant key minted a workload token: %d %s", rr.Code, rr.Body.String())
	}

	// Without tenant_id a create lands in the caller's tenant.
	rr := f.do(keyTenantA, http.MethodPost, "/v1/sandboxes", `{"image_ref":"img","cpu_millis":100,"memory_mib":64}`)
	var created store.Sandbox
	if rr.Code != http.StatusCreated || json.Unmarshal(rr.Body.Bytes(), &created) != nil || created.TenantID != "tenant-a" {
		t.Fatalf("create without tenant_id: %d %s", rr.Code, rr.Body.String())
	}

	// The list is the caller's tenant, with or without the filter.
	for _, path := range []string{"/v1/sandboxes", "/v1/sandboxes?tenant_id=tenant-a"} {
		rr := f.do(keyTenantA, http.MethodGet, path, "")
		var list listSandboxesResponse
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &list) != nil {
			t.Fatalf("list %s: %d %s", path, rr.Code, rr.Body.String())
		}
		for _, sb := range list.Sandboxes {
			if sb.TenantID != "tenant-a" {
				t.Fatalf("list %s leaked %s of %s", path, sb.ID, sb.TenantID)
			}
		}
		if len(list.Sandboxes) != 2 {
			t.Fatalf("list %s: want 2 tenant-a sandboxes, got %d", path, len(list.Sandboxes))
		}
	}
}

func TestPlatformKeySeesEveryTenant(t *testing.T) {
	f := newTenantFixture(t, AuthConfig{})
	if rr := f.do(keyPlatform, http.MethodGet, "/v1/sandboxes/"+f.sbB, ""); rr.Code != http.StatusOK {
		t.Fatalf("platform key, tenant-b sandbox: %d", rr.Code)
	}
	rr := f.do(keyPlatform, http.MethodGet, "/v1/sandboxes", "")
	var list listSandboxesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil || len(list.Sandboxes) != 2 {
		t.Fatalf("platform list: %d %s", rr.Code, rr.Body.String())
	}
	if rr := f.do(keyPlatform, http.MethodPost, "/v1/sandboxes", `{"tenant_id":"tenant-b","image_ref":"img","cpu_millis":100,"memory_mib":64}`); rr.Code != http.StatusCreated {
		t.Fatalf("platform create in tenant-b: %d %s", rr.Code, rr.Body.String())
	}
}

// The open lab (no keys, no IdP) keeps working without tenants; a create
// without tenant_id lands in ASP_DEFAULT_TENANT.
func TestOpenLabHasNoTenantConfinement(t *testing.T) {
	mem := newTestStore(t, "n1")
	h := AuthMiddleware(mem, AuthConfig{InsecureOpen: true})(testMux(NewServer(mem)))
	t.Setenv(EnvDefaultTenant, "lab")
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(`{"image_ref":"img","cpu_millis":100,"memory_mib":64}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var sb store.Sandbox
	if rr.Code != http.StatusCreated || json.Unmarshal(rr.Body.Bytes(), &sb) != nil || sb.TenantID != "lab" {
		t.Fatalf("open lab create: %d %s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/tenants/other/egress", nil)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("open lab egress: %d", rr.Code)
	}
}

func TestIdPPrincipalIsConfinedToItsTenant(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const iss, aud, kid = "https://idp.test.local", "asp-api", "k"
	v, err := idp.NewValidatorWithPublicKeys(idp.Config{Issuer: iss, Audience: aud}, map[string]*rsa.PublicKey{kid: &key.PublicKey})
	if err != nil {
		t.Fatal(err)
	}
	f := newTenantFixture(t, AuthConfig{IdP: v})
	token := func(extra map[string]any) string {
		extra["groups"] = []string{"asp-admin"}
		tok, err := idp.SignTestTokenClaims(key, kid, iss, "user:alice", aud, "", time.Now().Add(time.Hour), extra)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	alice := token(map[string]any{"tenant_id": "tenant-a"})
	if rr := f.do(alice, http.MethodGet, "/v1/sandboxes/"+f.sbA, ""); rr.Code != http.StatusOK {
		t.Fatalf("own tenant: %d %s", rr.Code, rr.Body.String())
	}
	if rr := f.do(alice, http.MethodGet, "/v1/sandboxes/"+f.sbB, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("other tenant's sandbox: want 404, got %d", rr.Code)
	}
	if rr := f.do(alice, http.MethodPut, "/v1/tenants/tenant-b/egress", `{"rules":[]}`); rr.Code != http.StatusForbidden {
		t.Fatalf("other tenant's egress (admin role): want 403, got %d", rr.Code)
	}
	if rr := f.do(token(map[string]any{}), http.MethodGet, "/v1/sandboxes", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("token without tenant: want 401, got %d %s", rr.Code, rr.Body.String())
	}
	if rr := f.do(token(map[string]any{"tenant_id": []string{"tenant-a", "tenant-b"}}), http.MethodGet, "/v1/sandboxes", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("token naming two tenants: want 401, got %d", rr.Code)
	}
}
