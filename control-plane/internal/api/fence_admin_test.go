package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/fence"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// The fence target is an admin decision: a node cannot choose it, a tenant key
// cannot set it, and it is never serialized.
type fenceEnv struct {
	t   *testing.T
	h   http.Handler
	mem *store.MemoryStore
	srv *Server
}

func newFenceEnv(t *testing.T) *fenceEnv {
	t.Helper()
	t.Setenv("ASP_AUTO_PROVISION", "0")
	mem := newTestStore(t, "n1")
	if _, err := mem.EnsureAPIKey(context.Background(), "default", "ops", store.APIKeyScopePlatform, "asp_plat", store.HashAPIKeySecret("platform-key")); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.EnsureAPIKey(context.Background(), "t1", "svc", store.APIKeyScopeTenant, "asp_tnt1", store.HashAPIKeySecret("tenant-key")); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(mem)
	return &fenceEnv{t: t, h: AuthMiddleware(mem, AuthConfig{})(testMux(srv)), mem: mem, srv: srv}
}

func (e *fenceEnv) do(method, path, bearer, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr
}

func TestFenceTargetIsSetByAnAdminAndNeverShown(t *testing.T) {
	e := newFenceEnv(t)
	body := `{"endpoint":"https://bmc.example/redfish","token":"bmc-password"}`

	for _, key := range []string{"tenant-key"} {
		if rr := e.do("PUT", "/v1/nodes/n1/fence", key, body); rr.Code != http.StatusForbidden {
			t.Fatalf("%s: status %d, want 403: %s", key, rr.Code, rr.Body.String())
		}
	}
	if n, _ := e.mem.GetNode(context.Background(), "n1"); n.FenceEndpoint != "" {
		t.Fatalf("a refused request changed the target: %+v", n)
	}

	if rr := e.do("PUT", "/v1/nodes/n1/fence", "platform-key", body); rr.Code != http.StatusNoContent {
		t.Fatalf("platform key: status %d: %s", rr.Code, rr.Body.String())
	}
	n, _ := e.mem.GetNode(context.Background(), "n1")
	if n.FenceEndpoint != "https://bmc.example/redfish" || n.FenceToken != "bmc-password" {
		t.Fatalf("target not stored: %+v", n)
	}

	rr := e.do("GET", "/v1/nodes", "platform-key", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d", rr.Code)
	}
	list := rr.Body.String()
	if !strings.Contains(list, `"fence_configured":true`) {
		t.Fatalf("the node view does not say a target is configured: %s", list)
	}
	for _, leak := range []string{"bmc-password", "bmc.example", "fence_endpoint", "fence_token"} {
		if strings.Contains(list, leak) {
			t.Fatalf("the node list leaks %q: %s", leak, list)
		}
	}

	if rr := e.do("DELETE", "/v1/nodes/n1/fence", "platform-key", ""); rr.Code != http.StatusNoContent {
		t.Fatalf("clear: %d %s", rr.Code, rr.Body.String())
	}
	n, _ = e.mem.GetNode(context.Background(), "n1")
	if n.FenceEndpoint != "" || n.FenceToken != "" {
		t.Fatalf("target not cleared: %+v", n)
	}
	if list := e.do("GET", "/v1/nodes", "platform-key", "").Body.String(); !strings.Contains(list, `"fence_configured":false`) {
		t.Fatalf("still configured after clearing: %s", list)
	}
}

func TestFenceTargetValidation(t *testing.T) {
	e := newFenceEnv(t)
	for name, tc := range map[string]struct {
		path, body string
		want       int
	}{
		"unknown node":     {"/v1/nodes/nope/fence", `{"endpoint":"https://bmc.example"}`, http.StatusNotFound},
		"no endpoint":      {"/v1/nodes/n1/fence", `{"token":"x"}`, http.StatusBadRequest},
		"newline":          {"/v1/nodes/n1/fence", `{"endpoint":"https://bmc.example\nX-Evil: 1"}`, http.StatusBadRequest},
		"relative file":    {"/v1/nodes/n1/fence", `{"endpoint":"https://bmc.example","token":"file:relative"}`, http.StatusBadRequest},
		"env without name": {"/v1/nodes/n1/fence", `{"endpoint":"https://bmc.example","token":"env:"}`, http.StatusBadRequest},
		"not json":         {"/v1/nodes/n1/fence", `endpoint=x`, http.StatusBadRequest},
	} {
		if rr := e.do("PUT", tc.path, "platform-key", tc.body); rr.Code != tc.want {
			t.Errorf("%s: status %d, want %d: %s", name, rr.Code, tc.want, rr.Body.String())
		}
	}
	if rr := e.do("DELETE", "/v1/nodes/nope/fence", "platform-key", ""); rr.Code != http.StatusNotFound {
		t.Errorf("clear on an unknown node: %d", rr.Code)
	}
}

// A credential can be a reference: the secret then never reaches the database.
func TestFenceCredentialReferencesAreResolvedWhenFencing(t *testing.T) {
	var auth string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
	}))
	defer hook.Close()
	t.Setenv("ASP_FENCE_PROVIDER", "http_webhook")
	t.Setenv("BMC_PW", "pw-from-env")
	e := newFenceEnv(t)
	e.srv.Fence = fence.FromEnv()
	if rr := e.do("PUT", "/v1/nodes/n1/fence", "platform-key", `{"endpoint":"`+hook.URL+`","token":"env:BMC_PW"}`); rr.Code != http.StatusNoContent {
		t.Fatalf("set: %d %s", rr.Code, rr.Body.String())
	}
	n, _ := e.mem.GetNode(context.Background(), "n1")
	if n.FenceToken != "env:BMC_PW" {
		t.Fatalf("the reference was replaced by its value in the store: %q", n.FenceToken)
	}
	if ok, err := e.srv.fenceNode(t.Context(), n); err != nil || !ok {
		t.Fatalf("fenceNode: %v %v", ok, err)
	}
	if auth != "Bearer pw-from-env" {
		t.Fatalf("webhook got %q", auth)
	}

	t.Setenv("BMC_PW", "")
	if _, err := e.srv.fenceNode(t.Context(), n); err == nil {
		t.Fatal("an unresolvable reference must fail the fence, not send an empty credential")
	}
}
