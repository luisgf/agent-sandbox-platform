package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/authn/idp"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

func TestRBACViewerCannotCreate(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	key, kid, v := testIdP(t)
	mem := store.NewMemoryStore()
	srv := NewServer(mem)
	h := AuthMiddleware(mem, AuthConfig{IdP: v})(testMux(srv))

	tok := mintUserJWTWithGroups(t, key, kid, "user:view", "v@ex.com", []string{"asp-viewer"})
	body := `{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":128}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestRBACOperatorCreateAdminDestroyAny(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	key, kid, v := testIdP(t)
	mem := store.NewMemoryStore()
	srv := NewServer(mem)
	h := AuthMiddleware(mem, AuthConfig{IdP: v})(testMux(srv))

	alice := mintUserJWTWithGroups(t, key, kid, "user:alice", "", []string{"asp-operator"})
	bob := mintUserJWTWithGroups(t, key, kid, "user:bob", "", []string{"asp-operator"})
	admin := mintUserJWTWithGroups(t, key, kid, "user:admin", "", []string{"asp-admin"})
	viewer := mintUserJWTWithGroups(t, key, kid, "user:view", "", []string{"asp-viewer"})

	// alice creates
	body := `{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":128}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+alice)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", rr.Code, rr.Body.String())
	}
	var sb store.Sandbox
	_ = json.Unmarshal(rr.Body.Bytes(), &sb)
	if sb.OwnerSub != "user:alice" {
		t.Fatalf("owner=%q", sb.OwnerSub)
	}

	// bob cannot destroy alice's (operator own-only)
	req = httptest.NewRequest(http.MethodDelete, "/v1/sandboxes/"+sb.ID, nil)
	req.Header.Set("Authorization", "Bearer "+bob)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("bob destroy want 403, got %d %s", rr.Code, rr.Body.String())
	}

	// viewer can get
	req = httptest.NewRequest(http.MethodGet, "/v1/sandboxes/"+sb.ID, nil)
	req.Header.Set("Authorization", "Bearer "+viewer)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("viewer get=%d %s", rr.Code, rr.Body.String())
	}

	// viewer cannot exec
	req = httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/exec", bytes.NewBufferString(`{"cmd":["true"]}`))
	req.Header.Set("Authorization", "Bearer "+viewer)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("viewer exec want 403, got %d", rr.Code)
	}

	// admin can destroy any
	req = httptest.NewRequest(http.MethodDelete, "/v1/sandboxes/"+sb.ID, nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin destroy=%d %s", rr.Code, rr.Body.String())
	}
}

func TestRBACOperatorDestroyAnyClaim(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	key, kid, v := testIdP(t)
	mem := store.NewMemoryStore()
	srv := NewServer(mem)
	h := AuthMiddleware(mem, AuthConfig{IdP: v})(testMux(srv))

	alice := mintUserJWTWithGroups(t, key, kid, "user:alice", "", []string{"asp-operator"})
	opsAny := mintUserJWTWithGroups(t, key, kid, "user:ops", "", []string{"asp-operator", "sandbox:destroy-any"})

	body := `{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":128}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+alice)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var sb store.Sandbox
	_ = json.Unmarshal(rr.Body.Bytes(), &sb)

	req = httptest.NewRequest(http.MethodDelete, "/v1/sandboxes/"+sb.ID, nil)
	req.Header.Set("Authorization", "Bearer "+opsAny)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("destroy-any want 200, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestRBACOwnerCanDestroyOwnEvenAsViewer(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	key, kid, v := testIdP(t)
	mem := store.NewMemoryStore()
	srv := NewServer(mem)
	h := AuthMiddleware(mem, AuthConfig{IdP: v})(testMux(srv))

	// create as operator, then act as same sub with viewer-only groups (ownership wins for destroy)
	op := mintUserJWTWithGroups(t, key, kid, "user:alice", "", []string{"asp-operator"})
	body := `{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":128}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+op)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var sb store.Sandbox
	_ = json.Unmarshal(rr.Body.Bytes(), &sb)

	viewerSame := mintUserJWTWithGroups(t, key, kid, "user:alice", "", []string{"asp-viewer"})
	req = httptest.NewRequest(http.MethodDelete, "/v1/sandboxes/"+sb.ID, nil)
	req.Header.Set("Authorization", "Bearer "+viewerSame)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("owner-as-viewer destroy want 200, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestRBACListTenantWideForOperatorViewer(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	key, kid, v := testIdP(t)
	mem := store.NewMemoryStore()
	srv := NewServer(mem)
	h := AuthMiddleware(mem, AuthConfig{IdP: v})(testMux(srv))

	alice := mintUserJWTWithGroups(t, key, kid, "user:alice", "", []string{"asp-operator"})
	bob := mintUserJWTWithGroups(t, key, kid, "user:bob", "", []string{"asp-operator"})
	viewer := mintUserJWTWithGroups(t, key, kid, "user:view", "", []string{"asp-viewer"})

	for _, tok := range []string{alice, bob} {
		body := `{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":128}`
		req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create=%d %s", rr.Code, rr.Body.String())
		}
	}

	// operator bob sees both (tenant-wide choice)
	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes?tenant_id=t1", nil)
	req.Header.Set("Authorization", "Bearer "+bob)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var out listSandboxesResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out.Sandboxes) != 2 {
		t.Fatalf("operator list want 2, got %d", len(out.Sandboxes))
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/sandboxes?tenant_id=t1", nil)
	req.Header.Set("Authorization", "Bearer "+viewer)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out.Sandboxes) != 2 {
		t.Fatalf("viewer list want 2 (tenant-wide RO), got %d", len(out.Sandboxes))
	}
}

func TestRBACNoRoleForbidden(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	key, kid, v := testIdP(t)
	mem := store.NewMemoryStore()
	srv := NewServer(mem)
	h := AuthMiddleware(mem, AuthConfig{IdP: v})(testMux(srv))

	tok := mintUserJWTWithGroups(t, key, kid, "user:nobody", "", nil) // no groups
	body := `{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":128}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestRBACIdPOffNoEnforcement(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	mem := store.NewMemoryStore()
	srv := NewServer(mem)
	h := AuthMiddleware(mem, AuthConfig{})(testMux(srv))
	body := `{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":128,"owner_sub":"lab"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("lab create=%d", rr.Code)
	}
}

func TestRBACEgressAdminOnly(t *testing.T) {
	key, kid, v := testIdP(t)
	mem := store.NewMemoryStore()
	srv := NewServer(mem)
	h := AuthMiddleware(mem, AuthConfig{IdP: v})(testMux(srv))

	op := mintUserJWTWithGroups(t, key, kid, "user:op", "", []string{"asp-operator"})
	admin := mintUserJWTWithGroups(t, key, kid, "user:adm", "", []string{"asp-admin"})

	req := httptest.NewRequest(http.MethodPut, "/v1/tenants/t1/egress", bytes.NewBufferString(`{"rules":[]}`))
	req.Header.Set("Authorization", "Bearer "+op)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("op egress want 403, got %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodPut, "/v1/tenants/t1/egress", bytes.NewBufferString(`{"rules":[]}`))
	req.Header.Set("Authorization", "Bearer "+admin)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin egress=%d %s", rr.Code, rr.Body.String())
	}
}

func TestRBACRoleMapFromEnv(t *testing.T) {
	t.Setenv("ASP_IDP_ROLE_CLAIM", "roles")
	t.Setenv("ASP_IDP_ROLE_MAP", "Corp.Admin:admin,Corp.Ops:operator,Corp.View:viewer")
	t.Setenv("ASP_IDP_ROLE_PREFIX", "")
	cfg := idp.ConfigFromEnv()
	if cfg.RoleClaim != "roles" {
		t.Fatalf("claim=%q", cfg.RoleClaim)
	}
	role, _ := cfg.MapRoles([]string{"Corp.Ops"})
	if role != idp.RoleOperator {
		t.Fatalf("role=%q", role)
	}
	role, _ = cfg.MapRoles([]string{"Corp.Admin", "Corp.View"})
	if role != idp.RoleAdmin {
		t.Fatalf("want admin highest, got %q", role)
	}
}
