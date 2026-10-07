package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

func scrape(t *testing.T, h http.Handler, token string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// /metrics reports what the control plane did and what it holds: requests by
// route and status, creates by outcome, sandboxes by tenant and state, and each
// node's liveness and allocation.
func TestMetricsReportWhatTheControlPlaneDid(t *testing.T) {
	mem := store.NewMemoryStore()
	if _, err := mem.RegisterNode(context.Background(), store.RegisterNodeInput{ID: "node-a", AgentEndpoint: "http://127.0.0.1:9100", MaxSandboxes: 1}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(mem)
	h := srv.Instrument(testMux(srv))

	body := `{"tenant_id":"t1","image_ref":"debian","cpu_millis":1000,"memory_mib":512}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	// The node takes one sandbox: the second does not fit.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body)))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("a second sandbox on a node of one: %d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/sandboxes/nope", nil))
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/not/a/route/with/an/id/abc123", nil))

	code, out := scrape(t, h, "")
	if code != http.StatusOK {
		t.Fatalf("scrape: %d %s", code, out)
	}
	for _, want := range []string{
		`asp_http_requests_total{route="POST /v1/sandboxes",code="201"} 1`,
		`asp_http_requests_total{route="POST /v1/sandboxes",code="503"} 1`,
		`asp_http_requests_total{route="GET /v1/sandboxes/{id}",code="404"} 1`,
		// A path no route matches does not make a label of its own.
		`asp_http_requests_total{route="unmatched",code="404"} 1`,
		`asp_http_request_duration_seconds_count{route="POST /v1/sandboxes"} 2`,
		`asp_sandbox_creates_total{result="ok"} 1`,
		`asp_sandbox_creates_total{result="no_capacity"} 1`,
		`asp_sandboxes{tenant="t1",state="requested"} 1`,
		`asp_node_up{node="node-a"} 1`,
		`asp_node_schedulable{node="node-a"} 1`,
		`asp_node_sandboxes{node="node-a"} 1`,
		`asp_node_egress_enforced{node="node-a"} 0`,
		"go_goroutines ",
		"process_start_time_seconds ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	if strings.Contains(out, "abc123") || strings.Contains(out, "nope") {
		t.Error("a path parameter leaked into a label")
	}
}

// The numbers span every tenant: a tenant's key cannot read them, a platform key
// and an IdP admin or operator can.
func TestMetricsNeedAPlatformKeyOrAnOperator(t *testing.T) {
	key, kid, v := testIdP(t)
	h, mem := nodeAdminServer(t, AuthConfig{IdP: v})
	if _, err := mem.EnsureAPIKey(context.Background(), "t1", "tenant", store.APIKeyScopeTenant, "asp_tenant", store.HashAPIKeySecret("tenant-key")); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.EnsureAPIKey(context.Background(), "default", "ops", store.APIKeyScopePlatform, "asp_ops", store.HashAPIKeySecret("platform-key")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		caller, token string
		want          int
	}{
		{"tenant api key", "tenant-key", http.StatusForbidden},
		{"platform api key", "platform-key", http.StatusOK},
		{"idp admin", mintUserJWTWithGroups(t, key, kid, "user:admin", "", []string{"asp-admin"}), http.StatusOK},
		{"idp operator", mintUserJWTWithGroups(t, key, kid, "user:op", "", []string{"asp-operator"}), http.StatusOK},
		{"idp viewer", mintUserJWTWithGroups(t, key, kid, "user:view", "", []string{"asp-viewer"}), http.StatusForbidden},
		{"bootstrap token", "boot-secret", http.StatusUnauthorized},
		{"no credential", "", http.StatusUnauthorized},
	} {
		if code, _ := scrape(t, h, tc.token); code != tc.want {
			t.Errorf("%s: want %d, got %d", tc.caller, tc.want, code)
		}
	}
	open, _ := nodeAdminServer(t, AuthConfig{InsecureOpen: true})
	if code, _ := scrape(t, open, ""); code != http.StatusOK {
		t.Errorf("the open lab: want 200, got %d", code)
	}
	// With the IdP required the route is part of the IdP surface, like the node inventory.
	_, _, v2 := testIdP(t)
	req, _ := nodeAdminServer(t, AuthConfig{IdP: v2, IdPRequired: true})
	if code, _ := scrape(t, req, "anything"); code != http.StatusUnauthorized {
		t.Errorf("IdP required, no IdP token: want 401, got %d", code)
	}
}
