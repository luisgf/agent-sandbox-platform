package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/pki"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// Fence credentials can power a node off: no node response may carry them.
func TestNodeResponsesNeverIncludeFenceCredentials(t *testing.T) {
	t.Setenv("ASP_NODE_BOOTSTRAP_TOKEN", "boot-secret")
	ca, err := pki.GenerateCA("test", pki.DefaultNodeTTL)
	if err != nil {
		t.Fatal(err)
	}
	mem := store.NewMemoryStore()
	srv := NewServer(mem)
	srv.CA = ca
	mux := testMux(srv)

	do := func(method, path, body string, bootstrap bool) string {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		if bootstrap {
			req.Header.Set("Authorization", "Bearer boot-secret")
		}
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code >= 300 {
			t.Fatalf("%s %s status=%d body=%s", method, path, rr.Code, rr.Body.String())
		}
		return rr.Body.String()
	}

	responses := map[string]string{
		"register": do(http.MethodPost, "/v1/nodes/register",
			`{"id":"n1","agent_endpoint":"http://127.0.0.1:9100","fence_endpoint":"https://bmc.example/redfish","fence_token":"bmc-password"}`, false),
	}
	responses["enroll"] = do(http.MethodPost, "/v1/nodes/enroll", `{"id":"n1","agent_endpoint":"http://127.0.0.1:9100"}`, true)
	responses["heartbeat"] = do(http.MethodPost, "/v1/nodes/n1/heartbeat", "", false)
	responses["list"] = do(http.MethodGet, "/v1/nodes", "", false)
	responses["revoke"] = do(http.MethodPost, "/v1/nodes/n1/revoke", "", true)

	for name, body := range responses {
		if strings.Contains(body, "bmc-password") || strings.Contains(body, "fence_") || strings.Contains(body, "bmc.example") {
			t.Errorf("%s response leaks fence credentials: %s", name, body)
		}
	}

	// The control plane still has them for fencing.
	node, err := mem.GetNode("n1")
	if err != nil {
		t.Fatal(err)
	}
	if node.FenceToken != "bmc-password" || node.FenceEndpoint != "https://bmc.example/redfish" {
		t.Fatalf("fence credentials lost in store: %+v", node)
	}
}
