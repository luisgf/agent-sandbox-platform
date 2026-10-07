package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

const doctorReport = `{"node_id":"n1","at":"2026-10-07T12:00:00Z","results":[{"name":"kvm","status":"ok","detail":"/dev/kvm opens read-write"},{"name":"nft","status":"fail","detail":"table missing","fix":"restart the agent"}]}`

// agentWithDoctor is a node-agent that answers the doctor route and counts the calls.
func agentWithDoctor(t *testing.T, status int, body string) (*httptest.Server, *int) {
	t.Helper()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/internal/doctor" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		n++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func getDoctor(h http.Handler, node, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/nodes/"+node+"/doctor", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// The control plane asks the node-agent of the node for its report and returns it as
// it is.
func TestNodeDoctorReturnsTheReportOfTheNodeAgent(t *testing.T) {
	agent, calls := agentWithDoctor(t, http.StatusOK, doctorReport)
	mem := newTestStore(t)
	if _, err := mem.RegisterNode(context.Background(), store.RegisterNodeInput{ID: "n1", AgentEndpoint: agent.URL}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(mem)
	rr := getDoctor(testMux(srv), "n1", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	var got struct {
		NodeID  string `json:"node_id"`
		Results []struct{ Name, Status string }
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || got.NodeID != "n1" || len(got.Results) != 2 || got.Results[1].Status != "fail" {
		t.Fatalf("%+v %v (%s)", got, err, rr.Body.String())
	}
	if *calls != 1 || !strings.Contains(rr.Header().Get("Content-Type"), "json") {
		t.Fatalf("calls=%d content-type=%q", *calls, rr.Header().Get("Content-Type"))
	}
}

func TestNodeDoctorErrors(t *testing.T) {
	mem := newTestStore(t)
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close() // nobody listens there any more
	noDoctor, _ := agentWithDoctor(t, http.StatusNotImplemented, `{"error":"this node-agent has no doctor"}`)
	for id, endpoint := range map[string]string{"down": downURL, "old": noDoctor.URL, "revoked": noDoctor.URL} {
		if _, err := mem.RegisterNode(context.Background(), store.RegisterNodeInput{ID: id, AgentEndpoint: endpoint}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := mem.RevokeNode(context.Background(), "revoked"); err != nil {
		t.Fatal(err)
	}
	h := testMux(NewServer(mem))
	for _, tc := range []struct {
		node string
		want int
		body string
	}{
		{"ghost", http.StatusNotFound, "node not found"},
		{"revoked", http.StatusConflict, "revoked"},
		{"down", http.StatusBadGateway, "node-agent unreachable"},
		{"old", http.StatusNotImplemented, "has no doctor: upgrade it"},
	} {
		rr := getDoctor(h, tc.node, "")
		if rr.Code != tc.want || !strings.Contains(rr.Body.String(), tc.body) {
			t.Errorf("%s: %d %s, want %d containing %q", tc.node, rr.Code, rr.Body.String(), tc.want, tc.body)
		}
	}
}

// Nodes are shared by every tenant: a tenant's key cannot run a doctor on them, an
// admin, an operator or a platform key can.
func TestNodeDoctorHasTheSameCallersAsTheNodeList(t *testing.T) {
	key, kid, v := testIdP(t)
	agent, calls := agentWithDoctor(t, http.StatusOK, doctorReport)
	mem := store.NewMemoryStore()
	if _, err := mem.RegisterNode(context.Background(), store.RegisterNodeInput{ID: "n1", AgentEndpoint: agent.URL}); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.EnsureAPIKey(context.Background(), "t1", "tenant", store.APIKeyScopeTenant, "asp_tenant", store.HashAPIKeySecret("tenant-key")); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.EnsureAPIKey(context.Background(), "default", "ops", store.APIKeyScopePlatform, "asp_ops", store.HashAPIKeySecret("platform-key")); err != nil {
		t.Fatal(err)
	}
	h := AuthMiddleware(mem, AuthConfig{IdP: v})(testMux(NewServer(mem)))
	for _, tc := range []struct {
		caller, token string
		want          int
	}{
		{"tenant api key", "tenant-key", http.StatusForbidden},
		{"platform api key", "platform-key", http.StatusOK},
		{"idp admin", mintUserJWTWithGroups(t, key, kid, "user:admin", "", []string{"asp-admin"}), http.StatusOK},
		{"idp operator", mintUserJWTWithGroups(t, key, kid, "user:op", "", []string{"asp-operator"}), http.StatusOK},
		{"idp viewer", mintUserJWTWithGroups(t, key, kid, "user:view", "", []string{"asp-viewer"}), http.StatusForbidden},
		{"no credential", "", http.StatusUnauthorized},
	} {
		before := *calls
		rr := getDoctor(h, "n1", tc.token)
		if rr.Code != tc.want {
			t.Errorf("%s: want %d, got %d %s", tc.caller, tc.want, rr.Code, rr.Body.String())
		}
		if tc.want != http.StatusOK && *calls != before {
			t.Errorf("%s: the node was asked although the caller was refused", tc.caller)
		}
	}
}
