package api

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/pki"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

func TestRevokedNodeCannotHeartbeatOrRegister(t *testing.T) {
	mem := store.NewMemoryStore()
	if _, err := mem.RegisterNode(store.RegisterNodeInput{ID: "n1", AgentEndpoint: "http://127.0.0.1:9100"}); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.RevokeNode("n1"); err != nil {
		t.Fatal(err)
	}
	mux := testMux(NewServer(mem))

	for _, tc := range []struct{ path, body string }{
		{"/v1/nodes/n1/heartbeat", ""},
		{"/v1/nodes/register", `{"id":"n1","agent_endpoint":"http://127.0.0.1:9100"}`},
	} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, tc.path, bytes.NewBufferString(tc.body)))
		if rr.Code != http.StatusConflict {
			t.Errorf("%s: want 409 for a revoked node, got %d %s", tc.path, rr.Code, rr.Body.String())
		}
	}
}

func TestEnrollIssuesServerCertificateForTheNode(t *testing.T) {
	t.Setenv("ASP_NODE_BOOTSTRAP_TOKEN", "boot-secret")
	ca, err := pki.GenerateCA("test", pki.DefaultNodeTTL)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(store.NewMemoryStore())
	srv.CA = ca
	mux := testMux(srv)

	enroll := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/nodes/enroll", bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer boot-secret")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		return rr
	}
	rr := enroll(`{"id":"node-a","agent_endpoint":"https://10.0.0.7:9443"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("enroll status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er enrollResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &er); err != nil {
		t.Fatal(err)
	}
	cert, err := pki.ParseCertPEM([]byte(er.ClientCertPEM))
	if err != nil {
		t.Fatal(err)
	}
	// The control plane dials the agent with ServerName = node id.
	if _, err := cert.Verify(x509.VerifyOptions{Roots: ca.CertPool(), DNSName: "node-a", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("node certificate is not a server certificate for node-a: %v", err)
	}
	// The endpoint the node names is not a name of its certificate (#104).
	if len(cert.IPAddresses) != 0 || len(cert.DNSNames) != 1 {
		t.Fatalf("a node certificate names more than the node: dns=%v ip=%v", cert.DNSNames, cert.IPAddresses)
	}

	if rr := enroll(`{"id":"bad id","agent_endpoint":"https://10.0.0.7:9443"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid node id: want 400, got %d %s", rr.Code, rr.Body.String())
	}
	if rr := enroll(`{"id":"asp-control-plane"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("reserved node id: want 400, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestCreateExplainsPlacementRefusals(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	mem := store.NewMemoryStore()
	mux := testMux(NewServer(mem))
	create := func(body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body)))
		return rr
	}
	const sb = `{"tenant_id":"t1","image_ref":"img","cpu_millis":1000,"memory_mib":512`

	rr := create(sb + `}`)
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") != "30" ||
		!bytes.Contains(rr.Body.Bytes(), []byte("no schedulable nodes registered")) {
		t.Fatalf("no nodes: %d %q %s", rr.Code, rr.Header().Get("Retry-After"), rr.Body.String())
	}

	if _, err := mem.RegisterNode(store.RegisterNodeInput{ID: "n1", AgentEndpoint: "http://127.0.0.1:9100", MaxSandboxes: 1}); err != nil {
		t.Fatal(err)
	}
	if rr := create(sb + `}`); rr.Code != http.StatusCreated {
		t.Fatalf("first create: %d %s", rr.Code, rr.Body.String())
	}
	rr = create(sb + `}`)
	var refused placementErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &refused); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusServiceUnavailable || refused.Reasons["max_sandboxes"] != 1 {
		t.Fatalf("full node: %d %s", rr.Code, rr.Body.String())
	}

	rr = create(sb + `,"node_id":"ghost"}`)
	if rr.Code != http.StatusConflict || !bytes.Contains(rr.Body.Bytes(), []byte("node ghost is not registered")) {
		t.Fatalf("unknown pin: %d %s", rr.Code, rr.Body.String())
	}
}

func TestWorkPollForUnknownNodeIs404(t *testing.T) {
	mux := testMux(NewServer(store.NewMemoryStore()))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/nodes/ghost/work", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404 so the agent re-registers, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestNodeListShowsAllocationAndCordonNeedsAdmin(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	key, kid, v := testIdP(t)
	mem := store.NewMemoryStore()
	for _, id := range []string{"node-b", "node-a"} {
		if _, err := mem.RegisterNode(store.RegisterNodeInput{
			ID: id, AgentEndpoint: "http://127.0.0.1:9100", CapacityCPU: 2, CapacityMemMiB: 4096, MaxSandboxes: 4,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := mem.SetNodeFence(id, "https://bmc.example/"+id, "bmc-secret"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := mem.CreateSandbox(store.CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 1500, MemoryMiB: 1024, NodeID: "node-a"}); err != nil {
		t.Fatal(err)
	}
	h := AuthMiddleware(mem, AuthConfig{IdP: v})(testMux(NewServer(mem)))
	admin := mintUserJWTWithGroups(t, key, kid, "user:admin", "", []string{"asp-admin"})
	operator := mintUserJWTWithGroups(t, key, kid, "user:op", "", []string{"asp-operator"})
	do := func(method, path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	if rr := do(http.MethodPost, "/v1/nodes/node-a/cordon", operator); rr.Code != http.StatusForbidden {
		t.Fatalf("operator cordon: want 403, got %d %s", rr.Code, rr.Body.String())
	}
	rr := do(http.MethodPost, "/v1/nodes/node-a/cordon", admin)
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"unschedulable_reason":"cordoned"`)) {
		t.Fatalf("admin cordon: %d %s", rr.Code, rr.Body.String())
	}
	if rr := do(http.MethodPost, "/v1/nodes/ghost/cordon", admin); rr.Code != http.StatusNotFound {
		t.Fatalf("cordon unknown node: %d", rr.Code)
	}

	rr = do(http.MethodGet, "/v1/nodes", operator)
	if rr.Code != http.StatusOK || bytes.Contains(rr.Body.Bytes(), []byte("bmc-secret")) {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	var list listNodesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Nodes) != 2 || list.Nodes[0].ID != "node-a" || list.Nodes[1].ID != "node-b" {
		t.Fatalf("nodes sorted by id: %+v", list.Nodes)
	}
	a := list.Nodes[0]
	// Allocated memory counts the per-VM overhead: 1024 MiB asked + 64 MiB.
	if a.Allocated.CPUMillis != 1500 || a.Allocated.MemoryMiB != 1024+64 || a.Allocated.Sandboxes != 1 || a.VMOverheadMiB != 64 ||
		a.Allocatable.CPUMillis != 8000 || a.Allocatable.MemoryMiB != 4096 || a.Allocatable.Sandboxes != 4 ||
		a.Schedulable || a.UnschedulableReason != "cordoned" || !a.Cordoned {
		t.Fatalf("node-a view: %+v", a)
	}
	if b := list.Nodes[1]; !b.Schedulable || b.UnschedulableReason != "" {
		t.Fatalf("node-b view: %+v", b)
	}
	if rr := do(http.MethodPost, "/v1/nodes/node-a/uncordon", admin); rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"schedulable":true`)) {
		t.Fatalf("uncordon: %d %s", rr.Code, rr.Body.String())
	}
}

func TestLateStatusReportIs409(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	mem := newTestStore(t)
	sb, err := mem.CreateSandbox(store.CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mem.StopSandbox(sb.ID, ""); err != nil { // never claimed → stopped
		t.Fatal(err)
	}
	mux := testMux(NewServer(mem))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/status", bytes.NewBufferString(`{"state":"running"}`)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("late running report: want 409, got %d %s", rr.Code, rr.Body.String())
	}
}
