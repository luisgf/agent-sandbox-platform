package api

import (
	"bytes"
	"context"
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
	mem := newTestStore(t)
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
	mem := newTestStore(t)
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
	mem := newTestStore(t)
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
	mem := newTestStore(t)
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
	mem := newTestStore(t)
	srv := NewServer(mem)
	h := AuthMiddleware(mem, AuthConfig{InsecureOpen: true})(testMux(srv))
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
	t.Setenv("ASP_IDP_ROLE_MAP", "Corp.Admin:admin,Corp.Ops:operator,Corp.Dev:user,Corp.View:viewer")
	t.Setenv("ASP_IDP_ROLE_PREFIX", "")
	cfg := idp.ConfigFromEnv()
	if cfg.RoleClaim != "roles" {
		t.Fatalf("claim=%q", cfg.RoleClaim)
	}
	if role := cfg.MapGrants([]string{"Corp.Ops"}).Role; role != idp.RoleOperator {
		t.Fatalf("role=%q", role)
	}
	if role := cfg.MapGrants([]string{"Corp.Dev"}).Role; role != idp.RoleUser {
		t.Fatalf("role=%q", role)
	}
	if role := cfg.MapGrants([]string{"Corp.Admin", "Corp.View"}).Role; role != idp.RoleAdmin {
		t.Fatalf("want admin highest, got %q", role)
	}
}

func TestRBACNodeListAdminOrOperator(t *testing.T) {
	key, kid, v := testIdP(t)
	mem := store.NewMemoryStore()
	if _, err := mem.RegisterNode(context.Background(), store.RegisterNodeInput{ID: "n1", AgentEndpoint: "http://127.0.0.1:9100"}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(mem)
	h := AuthMiddleware(mem, AuthConfig{IdP: v, IdPRequired: true})(testMux(srv))

	list := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/nodes", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	if got := list(""); got != http.StatusUnauthorized {
		t.Fatalf("no token: want 401 with ASP_IDP_REQUIRED, got %d", got)
	}
	if got := list(mintUserJWTWithGroups(t, key, kid, "user:view", "", []string{"asp-viewer"})); got != http.StatusForbidden {
		t.Fatalf("viewer: want 403, got %d", got)
	}
	if got := list(mintUserJWTWithGroups(t, key, kid, "user:op", "", []string{"asp-operator"})); got != http.StatusOK {
		t.Fatalf("operator: want 200, got %d", got)
	}
	if got := list(mintUserJWTWithGroups(t, key, kid, "user:admin", "", []string{"asp-admin"})); got != http.StatusOK {
		t.Fatalf("admin: want 200, got %d", got)
	}
}

// rbacDo sends one request as tok and returns the status and body.
func rbacDo(t *testing.T, h http.Handler, tok, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

func rbacCreate(t *testing.T, h http.Handler, tok string) store.Sandbox {
	t.Helper()
	code, body := rbacDo(t, h, tok, http.MethodPost, "/v1/sandboxes", `{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":128}`)
	if code != http.StatusCreated {
		t.Fatalf("create=%d %s", code, body)
	}
	var sb store.Sandbox
	_ = json.Unmarshal([]byte(body), &sb)
	return sb
}

// asp-user creates sandboxes and acts only on its own: it neither sees nor
// runs anything in another user's sandbox.
func TestRBACUserActsOnlyOnOwnSandboxes(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	key, kid, v := testIdP(t)
	mem := newTestStore(t)
	h := AuthMiddleware(mem, AuthConfig{IdP: v})(testMux(NewServer(mem)))

	alice := mintUserJWTWithGroups(t, key, kid, "user:alice", "", []string{"asp-user"})
	bob := mintUserJWTWithGroups(t, key, kid, "user:bob", "", []string{"asp-user"})
	sbA := rbacCreate(t, h, alice)
	sbB := rbacCreate(t, h, bob)
	if sbA.OwnerSub != "user:alice" {
		t.Fatalf("owner=%q", sbA.OwnerSub)
	}

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/sandboxes/" + sbA.ID, ""},
		{http.MethodGet, "/v1/sandboxes/" + sbA.ID + "/events", ""},
		{http.MethodPost, "/v1/sandboxes/" + sbA.ID + "/exec", `{"cmd":["true"]}`},
		{http.MethodPost, "/v1/sandboxes/" + sbA.ID + "/exec?stream=1", `{"cmd":["true"]}`},
		{http.MethodDelete, "/v1/sandboxes/" + sbA.ID, ""},
	} {
		if code, body := rbacDo(t, h, bob, tc.method, tc.path, tc.body); code != http.StatusForbidden {
			t.Fatalf("bob %s %s: want 403, got %d %s", tc.method, tc.path, code, body)
		}
	}
	// Her own: allowed (the exec then fails on state, not on the role).
	if code, body := rbacDo(t, h, alice, http.MethodGet, "/v1/sandboxes/"+sbA.ID, ""); code != http.StatusOK {
		t.Fatalf("alice get own=%d %s", code, body)
	}
	if code, body := rbacDo(t, h, alice, http.MethodPost, "/v1/sandboxes/"+sbA.ID+"/exec", `{"cmd":["true"]}`); code == http.StatusForbidden {
		t.Fatalf("alice exec own was forbidden: %s", body)
	}

	// The list holds only the caller's sandboxes.
	code, body := rbacDo(t, h, bob, http.MethodGet, "/v1/sandboxes?tenant_id=t1", "")
	var out listSandboxesResponse
	_ = json.Unmarshal([]byte(body), &out)
	if code != http.StatusOK || len(out.Sandboxes) != 1 || out.Sandboxes[0].ID != sbB.ID {
		t.Fatalf("bob list=%d %+v", code, out.Sandboxes)
	}

	if code, body := rbacDo(t, h, alice, http.MethodDelete, "/v1/sandboxes/"+sbA.ID, ""); code != http.StatusOK {
		t.Fatalf("alice destroy own=%d %s", code, body)
	}
}

// An operator execs in another user's sandbox only with the exec-any grant.
func TestRBACOperatorExecAnyIsExplicit(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	key, kid, v := testIdP(t)
	mem := newTestStore(t)
	h := AuthMiddleware(mem, AuthConfig{IdP: v})(testMux(NewServer(mem)))

	alice := mintUserJWTWithGroups(t, key, kid, "user:alice", "", []string{"asp-user"})
	op := mintUserJWTWithGroups(t, key, kid, "user:op", "", []string{"asp-operator"})
	opAny := mintUserJWTWithGroups(t, key, kid, "user:ops", "", []string{"asp-operator", "sandbox:exec-any"})
	admin := mintUserJWTWithGroups(t, key, kid, "user:adm", "", []string{"asp-admin"})
	sb := rbacCreate(t, h, alice)
	exec := "/v1/sandboxes/" + sb.ID + "/exec"

	if code, body := rbacDo(t, h, op, http.MethodPost, exec, `{"cmd":["true"]}`); code != http.StatusForbidden {
		t.Fatalf("operator without exec-any: want 403, got %d %s", code, body)
	}
	if code, body := rbacDo(t, h, op, http.MethodGet, "/v1/sandboxes/"+sb.ID, ""); code != http.StatusOK {
		t.Fatalf("operator still sees the tenant: get=%d %s", code, body)
	}
	for name, tok := range map[string]string{"operator with exec-any": opAny, "admin": admin} {
		if code, body := rbacDo(t, h, tok, http.MethodPost, exec, `{"cmd":["true"]}`); code == http.StatusForbidden {
			t.Fatalf("%s exec was forbidden: %s", name, body)
		}
	}
}

// asp-viewer plus asp-user is the union of both: operator.
func TestRBACViewerPlusUserIsOperator(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	key, kid, v := testIdP(t)
	mem := newTestStore(t)
	h := AuthMiddleware(mem, AuthConfig{IdP: v})(testMux(NewServer(mem)))

	alice := mintUserJWTWithGroups(t, key, kid, "user:alice", "", []string{"asp-user"})
	both := mintUserJWTWithGroups(t, key, kid, "user:both", "", []string{"asp-viewer", "asp-user"})
	rbacCreate(t, h, alice)
	rbacCreate(t, h, both)
	code, body := rbacDo(t, h, both, http.MethodGet, "/v1/sandboxes?tenant_id=t1", "")
	var out listSandboxesResponse
	_ = json.Unmarshal([]byte(body), &out)
	if code != http.StatusOK || len(out.Sandboxes) != 2 {
		t.Fatalf("viewer+user list=%d, want both sandboxes, got %d", code, len(out.Sandboxes))
	}
}
