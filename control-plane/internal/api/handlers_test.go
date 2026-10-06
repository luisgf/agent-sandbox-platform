package api

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/oidc"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/pki"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

func testMux(s *Server) http.Handler {
	return s.Routes()
}

// newTestStore returns a memory store with healthy nodes, so creates without
// ASP_AUTO_PROVISION have somewhere to go (ADR-0011).
func newTestStore(t *testing.T, ids ...string) *store.MemoryStore {
	t.Helper()
	mem := store.NewMemoryStore()
	if len(ids) == 0 {
		ids = []string{"test-node"}
	}
	for _, id := range ids {
		if _, err := mem.RegisterNode(store.RegisterNodeInput{ID: id, AgentEndpoint: "http://127.0.0.1:9100"}); err != nil {
			t.Fatal(err)
		}
	}
	return mem
}

// runSandbox moves a placed sandbox to running, as its node agent would after
// booting the VM. Exec only reaches the agent for a running sandbox.
func runSandbox(t *testing.T, st store.Store, id string) {
	t.Helper()
	sb, err := st.GetSandbox(id)
	if err != nil {
		t.Fatal(err)
	}
	if sb.State == store.SandboxRequested {
		if sb.NodeID == nil {
			t.Fatalf("sandbox %s has no node", id)
		}
		if _, err := st.ClaimSandbox(id, *sb.NodeID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.UpdateSandboxStatus(id, store.SandboxRunning, "test"); err != nil {
		t.Fatal(err)
	}
}

func TestCreateGetListSandbox(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "1")
	srv := NewServer(store.NewMemoryStore())
	mux := testMux(srv)

	body := `{"tenant_id":"t1","image_ref":"debian:bookworm","cpu_millis":1000,"memory_mib":512}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rr.Code, rr.Body.String())
	}
	var sb store.Sandbox
	if err := json.Unmarshal(rr.Body.Bytes(), &sb); err != nil {
		t.Fatal(err)
	}
	if sb.State != store.SandboxRunning || sb.ID == "" {
		t.Fatalf("unexpected create response: %+v", sb)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/sandboxes/"+sb.ID, nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/sandboxes?tenant_id=t1", nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status=%d", rr.Code)
	}
	var list listSandboxesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Sandboxes) != 1 {
		t.Fatalf("list len=%d", len(list.Sandboxes))
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/sandboxes/"+sb.ID+"/events", nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("events status=%d body=%s", rr.Code, rr.Body.String())
	}
	var ev listEventsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &ev); err != nil {
		t.Fatal(err)
	}
	if len(ev.Events) < 3 {
		t.Fatalf("events len=%d", len(ev.Events))
	}
}

func TestCreateSandboxBadRequest(t *testing.T) {
	srv := NewServer(store.NewMemoryStore())
	mux := testMux(srv)
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(`{"tenant_id":"t"}`))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestGetSandboxNotFound(t *testing.T) {
	srv := NewServer(store.NewMemoryStore())
	mux := testMux(srv)
	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/does-not-exist", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestRegisterAndListNodes(t *testing.T) {
	srv := NewServer(store.NewMemoryStore())
	mux := testMux(srv)
	body := `{"id":"n1","name":"dev","endpoint":"http://127.0.0.1:1","agent_endpoint":"http://127.0.0.1:9100","capacity_cpu":4,"capacity_mem_mib":8192}`
	req := httptest.NewRequest(http.MethodPost, "/v1/nodes/register", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/nodes", nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status=%d", rr.Code)
	}
	var list listNodesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Nodes) != 1 || list.Nodes[0].ID != "n1" {
		t.Fatalf("nodes=%+v", list.Nodes)
	}
	if list.Nodes[0].AgentEndpoint != "http://127.0.0.1:9100" {
		t.Fatalf("agent_endpoint=%q", list.Nodes[0].AgentEndpoint)
	}
}

func TestHeartbeatNode(t *testing.T) {
	mem := store.NewMemoryStore()
	srv := NewServer(mem)
	mux := testMux(srv)
	_, err := mem.RegisterNode(store.RegisterNodeInput{ID: "hb1", Name: "hb1", Endpoint: "http://127.0.0.1:9"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/nodes/hb1/heartbeat", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestEnrollNode(t *testing.T) {
	t.Setenv("ASP_NODE_BOOTSTRAP_TOKEN", "boot-secret")
	ca, err := pki.GenerateCA("test", pki.DefaultNodeTTL)
	if err != nil {
		t.Fatal(err)
	}
	mem := store.NewMemoryStore()
	srv := NewServer(mem)
	srv.CA = ca
	mux := testMux(srv)

	req := httptest.NewRequest(http.MethodPost, "/v1/nodes/enroll", bytes.NewBufferString(
		`{"id":"en1","name":"en1","agent_endpoint":"http://127.0.0.1:9100","capacity_cpu":2,"capacity_mem_mib":1024}`,
	))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without token, got %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/nodes/enroll", bytes.NewBufferString(
		`{"id":"en1","name":"en1","agent_endpoint":"http://127.0.0.1:9100","capacity_cpu":2,"capacity_mem_mib":1024}`,
	))
	req.Header.Set("Authorization", "Bearer boot-secret")
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("enroll status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er enrollResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &er); err != nil {
		t.Fatal(err)
	}
	if er.NodeID != "en1" || er.ClientCertPEM == "" || er.ClientKeyPEM == "" || er.CACertPEM == "" {
		t.Fatalf("incomplete enroll: %+v", er)
	}
	node, err := mem.GetNode("en1")
	if err != nil {
		t.Fatal(err)
	}
	if node.CertFingerprint == "" || node.EnrolledAt == nil {
		t.Fatalf("node enrollment fields missing: %+v", node)
	}
	if _, err := ca.VerifyClientCert([]byte(er.ClientCertPEM)); err != nil {
		t.Fatalf("issued cert verify: %v", err)
	}
}

func TestExecProxiesToAgent(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/internal/exec" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stdout":"hello\n","stderr":"","exit_code":0}`))
	}))
	defer agent.Close()

	mem := store.NewMemoryStore()
	mem.SetProvisionNodeID("exec-node")
	_, err := mem.RegisterNode(store.RegisterNodeInput{
		ID: "exec-node", Name: "exec-node",
		Endpoint: agent.URL, AgentEndpoint: agent.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(mem)
	srv.Client = agent.Client()
	mux := testMux(srv)

	body := `{"tenant_id":"t1","image_ref":"img","cpu_millis":1,"memory_mib":64,"node_id":"exec-node"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", rr.Code, rr.Body.String())
	}
	var sb store.Sandbox
	_ = json.Unmarshal(rr.Body.Bytes(), &sb)
	runSandbox(t, mem, sb.ID)

	req = httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/exec",
		bytes.NewBufferString(`{"cmd":["echo","hello"]}`))
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("exec status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out execResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 0 || out.Stdout != "hello\n" {
		t.Fatalf("exec resp=%+v", out)
	}
}

func TestAuthMiddlewareOptionalOff(t *testing.T) {
	mem := newTestStore(t)
	srv := NewServer(mem)
	h := AuthMiddleware(mem, AuthConfig{Require: false})(testMux(srv))
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz=%d", rr.Code)
	}
	body := `{"tenant_id":"t1","image_ref":"img","cpu_millis":1,"memory_mib":64}`
	req = httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create without auth status=%d", rr.Code)
	}
}

func TestAuthMiddlewareRequire(t *testing.T) {
	mem := newTestStore(t)
	secret := "test-key-abc"
	_, err := BootstrapAPIKey(mem, secret)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(mem)
	h := AuthMiddleware(mem, AuthConfig{Require: true})(testMux(srv))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz should be public, got %d", rr.Code)
	}

	t.Setenv("ASP_NODE_BOOTSTRAP_TOKEN", "boot-for-auth-test")
	req = httptest.NewRequest(http.MethodPost, "/v1/nodes/enroll", bytes.NewBufferString(`{"id":"x"}`))
	req.Header.Set("Authorization", "Bearer boot-for-auth-test")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	// Public to API-key middleware; handler returns 503 because CA is nil (not 401 invalid api key).
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("enroll should reach handler without API key, want 503 got %d body=%s", rr.Code, rr.Body.String())
	}

	body := `{"tenant_id":"t1","image_ref":"img","cpu_millis":1,"memory_mib":64}`
	req = httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without key, got %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201 with key, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestTenantEgressPutGetCheck(t *testing.T) {
	t.Setenv("ASP_EGRESS_DEFAULT_ALLOW", "")
	t.Setenv("ASP_EGRESS_DENY_DEFAULT", "1")
	mem := store.NewMemoryStore()
	srv := NewServer(mem)
	mux := testMux(srv)

	req := httptest.NewRequest(http.MethodPut, "/v1/tenants/t1/egress", bytes.NewBufferString(
		`{"rules":[{"host_pattern":"*.github.com","enabled":true},{"host_pattern":"api.example.com","port":443}]}`,
	))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/tenants/t1/egress", nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d", rr.Code)
	}
	var got egressRulesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Rules) != 2 || got.Policy.Mode != store.EgressModeDenyDefault {
		t.Fatalf("got=%+v", got)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/tenants/t1/egress/check",
		bytes.NewBufferString(`{"host":"api.github.com"}`))
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("check status=%d", rr.Code)
	}
	var chk map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &chk)
	if chk["allowed"] != true {
		t.Fatalf("check=%v", chk)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/tenants/t1/egress/check",
		bytes.NewBufferString(`{"host":"evil.com"}`))
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	_ = json.Unmarshal(rr.Body.Bytes(), &chk)
	if chk["allowed"] != false {
		t.Fatalf("evil should deny: %v", chk)
	}
}

func TestOIDCMintAndJWKS(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	mem := newTestStore(t)
	srv := NewServer(mem)
	srv.OIDC = oidc.NewSignerFromKey(key, "http://issuer.test")
	mux := testMux(srv)

	body := `{"tenant_id":"t1","image_ref":"img","cpu_millis":1,"memory_mib":64}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	var sb store.Sandbox
	_ = json.Unmarshal(rr.Body.Bytes(), &sb)

	req = httptest.NewRequest(http.MethodGet, "/oidc/jwks.json", nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("jwks=%d", rr.Code)
	}
	jwks := rr.Body.Bytes()

	req = httptest.NewRequest(http.MethodPost, "/v1/internal/oidc/token",
		bytes.NewBufferString(`{"sandbox_id":"`+sb.ID+`","aud":"https://api.example.com","nonce":"n"}`))
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("mint=%d body=%s", rr.Code, rr.Body.String())
	}
	var mint map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &mint)
	token, _ := mint["access_token"].(string)
	claims, err := oidc.VerifyAgainstJWKS(token, jwks)
	if err != nil {
		t.Fatal(err)
	}
	if claims.TenantID != "t1" || claims.SandboxID != sb.ID {
		t.Fatalf("claims=%+v", claims)
	}
	// Node cannot override tenant via body — only sandbox_id is looked up.
}

func TestClaimWorkStatusDestroy(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	mem := store.NewMemoryStore()
	srv := NewServer(mem)
	mux := testMux(srv)

	_, err := mem.RegisterNode(store.RegisterNodeInput{
		ID: "n1", Name: "n1", Endpoint: "http://127.0.0.1:9",
		AgentEndpoint: "http://127.0.0.1:9100",
	})
	if err != nil {
		t.Fatal(err)
	}

	body := `{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":128}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rr.Code, rr.Body.String())
	}
	var sb store.Sandbox
	if err := json.Unmarshal(rr.Body.Bytes(), &sb); err != nil {
		t.Fatal(err)
	}
	if sb.State != store.SandboxRequested {
		t.Fatalf("want requested, got %s", sb.State)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/nodes/n1/work", nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("work status=%d", rr.Code)
	}
	var work listWorkResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &work); err != nil {
		t.Fatal(err)
	}
	if len(work.Sandboxes) < 1 {
		t.Fatalf("work empty: %+v", work)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/claim",
		bytes.NewBufferString(`{"node_id":"n1"}`))
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("claim status=%d body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/status",
		bytes.NewBufferString(`{"state":"running","detail":"ok"}`))
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/v1/sandboxes/"+sb.ID, nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", rr.Code, rr.Body.String())
	}
	var stopped store.Sandbox
	_ = json.Unmarshal(rr.Body.Bytes(), &stopped)
	if stopped.State != store.SandboxStopping {
		t.Fatalf("want stopping, got %s", stopped.State)
	}
}

func TestRotateAndRevokeNodeCert(t *testing.T) {
	t.Setenv("ASP_NODE_BOOTSTRAP_TOKEN", "boot-secret")
	ca, err := pki.GenerateCA("test", pki.DefaultNodeTTL)
	if err != nil {
		t.Fatal(err)
	}
	mem := store.NewMemoryStore()
	srv := NewServer(mem)
	srv.CA = ca
	mux := testMux(srv)

	req := httptest.NewRequest(http.MethodPost, "/v1/nodes/enroll", bytes.NewBufferString(
		`{"id":"rot1","name":"rot1","agent_endpoint":"http://127.0.0.1:9100"}`,
	))
	req.Header.Set("Authorization", "Bearer boot-secret")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("enroll=%d %s", rr.Code, rr.Body.String())
	}
	var er enrollResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &er); err != nil {
		t.Fatal(err)
	}
	oldFP := er.CertFingerprint

	req = httptest.NewRequest(http.MethodPost, "/v1/nodes/rot1/rotate-cert", nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("rotate without auth want 401 got %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/nodes/rot1/rotate-cert", nil)
	req.Header.Set("Authorization", "Bearer boot-secret")
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("rotate=%d %s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &er); err != nil {
		t.Fatal(err)
	}
	if er.CertFingerprint == "" || er.CertFingerprint == oldFP {
		t.Fatalf("expected new fingerprint, got %q old=%q", er.CertFingerprint, oldFP)
	}
	if er.CertSerial == "" {
		t.Fatal("expected cert_serial")
	}
	revoked, err := mem.IsCertRevoked(oldFP)
	if err != nil || !revoked {
		t.Fatalf("old fingerprint should be revoked: %v %v", revoked, err)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/nodes/rot1/revoke", nil)
	req.Header.Set("Authorization", "Bearer boot-secret")
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("revoke=%d %s", rr.Code, rr.Body.String())
	}
	var node store.Node
	if err := json.Unmarshal(rr.Body.Bytes(), &node); err != nil {
		t.Fatal(err)
	}
	if node.RevokedAt == nil {
		t.Fatal("expected revoked_at on node")
	}
	revoked, err = mem.IsCertRevoked(er.CertFingerprint)
	if err != nil || !revoked {
		t.Fatalf("current fp revoked after revoke: %v %v", revoked, err)
	}
}

func TestAuthMiddlewareRejectsRevokedCert(t *testing.T) {
	ca, err := pki.GenerateCA("test", pki.DefaultNodeTTL)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := ca.IssueNodeClient("n-rev", pki.DefaultNodeTTL)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := pki.ParseCertPEM(issued.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	mem := store.NewMemoryStore()
	_, err = mem.EnrollNode(store.EnrollNodeInput{ID: "n-rev", Name: "n-rev"}, store.CertMeta{
		Fingerprint: issued.Fingerprint, Serial: issued.Serial,
	}, store.EnrollAuth{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mem.RevokeNode("n-rev"); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(mem)
	h := AuthMiddleware(mem, AuthConfig{
		RejectRevokedCerts:    true,
		RequireNodeClientCert: true,
	})(testMux(srv))

	req := httptest.NewRequest(http.MethodPost, "/v1/nodes/register", bytes.NewBufferString(
		`{"id":"n-rev","name":"n-rev","endpoint":"http://127.0.0.1:1"}`,
	))
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 revoked, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte("revoked")) {
		t.Fatalf("body should mention revoked: %s", rr.Body.String())
	}
}

func TestAuthConfigStrictEnv(t *testing.T) {
	t.Setenv("ASP_REQUIRE_API_KEY", "0")
	t.Setenv("ASP_CLIENT_CA", "/tmp/fake-ca.pem")
	cfg := AuthConfigFromEnv()
	if !cfg.RequireNodeClientCert || !cfg.RejectRevokedCerts {
		t.Fatalf("cfg=%+v", cfg)
	}
	if !EnvTruthy("ASP_MTLS_STRICT") {
		t.Setenv("ASP_MTLS_STRICT", "1")
		if !EnvTruthy("ASP_MTLS_STRICT") {
			t.Fatal("EnvTruthy")
		}
	}
}

func TestRotateWithBootstrapWhenAPIKeysExist(t *testing.T) {
	t.Setenv("ASP_NODE_BOOTSTRAP_TOKEN", "boot-secret")
	ca, err := pki.GenerateCA("test", pki.DefaultNodeTTL)
	if err != nil {
		t.Fatal(err)
	}
	mem := store.NewMemoryStore()
	_, err = mem.EnsureAPIKey("default", "k", store.APIKeyScopePlatform, "asp_test", store.HashAPIKeySecret("not-the-bootstrap"))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(mem)
	srv.CA = ca
	h := AuthMiddleware(mem, AuthConfig{Require: false})(testMux(srv))

	req := httptest.NewRequest(http.MethodPost, "/v1/nodes/enroll", bytes.NewBufferString(
		`{"id":"rb1","name":"rb1","agent_endpoint":"http://127.0.0.1:9"}`,
	))
	req.Header.Set("Authorization", "Bearer boot-secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("enroll=%d %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/nodes/rb1/rotate-cert", nil)
	req.Header.Set("Authorization", "Bearer boot-secret")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("rotate with bootstrap while API keys exist: %d %s", rr.Code, rr.Body.String())
	}
}

func TestCreateSandboxOwnerAndActorHeader(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	srv := NewServer(newTestStore(t))
	mux := testMux(srv)

	// Empty owner OK (lab)
	body := `{"tenant_id":"t1","image_ref":"debian:bookworm","cpu_millis":1000,"memory_mib":512}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("empty owner create status=%d body=%s", rr.Code, rr.Body.String())
	}
	var sb0 store.Sandbox
	if err := json.Unmarshal(rr.Body.Bytes(), &sb0); err != nil {
		t.Fatal(err)
	}
	if sb0.OwnerSub != "" {
		t.Fatalf("want empty owner_sub, got %q", sb0.OwnerSub)
	}

	// Body owner + header actor
	body = `{"tenant_id":"t1","image_ref":"debian:bookworm","cpu_millis":1000,"memory_mib":512,"owner_sub":"user:alice","owner_email":"alice@ex.com"}`
	req = httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	req.Header.Set(HeaderASPActorSub, "user:operator")
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rr.Code, rr.Body.String())
	}
	var sb store.Sandbox
	if err := json.Unmarshal(rr.Body.Bytes(), &sb); err != nil {
		t.Fatal(err)
	}
	if sb.OwnerSub != "user:alice" || sb.OwnerEmail != "alice@ex.com" {
		t.Fatalf("create response owner: %+v", sb)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/sandboxes/"+sb.ID, nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d", rr.Code)
	}
	var got store.Sandbox
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.OwnerSub != "user:alice" || got.OwnerEmail != "alice@ex.com" {
		t.Fatalf("get owner: %+v", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/sandboxes/"+sb.ID+"/events", nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	var ev listEventsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &ev); err != nil {
		t.Fatal(err)
	}
	if len(ev.Events) < 1 || ev.Events[0].ActorSub != "user:operator" {
		t.Fatalf("create event actor_sub want user:operator, got %+v", ev.Events)
	}

	// Destroy with actor header
	req = httptest.NewRequest(http.MethodDelete, "/v1/sandboxes/"+sb.ID, nil)
	req.Header.Set(HeaderASPActorSub, "user:operator")
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("destroy status=%d body=%s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/sandboxes/"+sb.ID+"/events", nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if err := json.Unmarshal(rr.Body.Bytes(), &ev); err != nil {
		t.Fatal(err)
	}
	last := ev.Events[len(ev.Events)-1]
	if last.ActorSub != "user:operator" {
		t.Fatalf("destroy actor_sub=%q last=%+v", last.ActorSub, last)
	}
}

func TestCreateSandboxActorFallsBackToOwner(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	srv := NewServer(newTestStore(t))
	mux := testMux(srv)
	body := `{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":128,"owner_sub":"user:bob"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d", rr.Code)
	}
	var sb store.Sandbox
	_ = json.Unmarshal(rr.Body.Bytes(), &sb)
	req = httptest.NewRequest(http.MethodGet, "/v1/sandboxes/"+sb.ID+"/events", nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	var ev listEventsResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &ev)
	if len(ev.Events) < 1 || ev.Events[0].ActorSub != "user:bob" {
		t.Fatalf("fallback actor_sub: %+v", ev.Events)
	}
}

func TestOIDCMintIncludesUserSubFromOwner(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	mem := newTestStore(t)
	srv := NewServer(mem)
	srv.OIDC = oidc.NewSignerFromKey(key, "http://issuer.test")
	mux := testMux(srv)

	body := `{"tenant_id":"t1","image_ref":"img","cpu_millis":1,"memory_mib":64,"owner_sub":"user:alice"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", rr.Code, rr.Body.String())
	}
	var sb store.Sandbox
	_ = json.Unmarshal(rr.Body.Bytes(), &sb)

	jwksRR := httptest.NewRecorder()
	mux.ServeHTTP(jwksRR, httptest.NewRequest(http.MethodGet, "/oidc/jwks.json", nil))
	jwks := jwksRR.Body.Bytes()

	mintBody := `{"sandbox_id":"` + sb.ID + `","aud":"https://api.example.com"}`
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/internal/oidc/token", bytes.NewBufferString(mintBody)))
	if rr.Code != http.StatusOK {
		t.Fatalf("mint=%d %s", rr.Code, rr.Body.String())
	}
	var mint map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &mint)
	claimsOut, _ := mint["claims"].(map[string]any)
	if claimsOut["user_sub"] != "user:alice" {
		t.Fatalf("response claims user_sub=%v", claimsOut["user_sub"])
	}
	act, _ := claimsOut["act"].(map[string]any)
	if act == nil || act["sub"] != "user:alice" {
		t.Fatalf("response claims act=%v", claimsOut["act"])
	}
	token, _ := mint["access_token"].(string)
	claims, err := oidc.VerifyAgainstJWKS(token, jwks)
	if err != nil {
		t.Fatal(err)
	}
	if claims.UserSub != "user:alice" {
		t.Fatalf("jwt user_sub=%q", claims.UserSub)
	}
	if claims.Act == nil || claims.Act.Sub != "user:alice" {
		t.Fatalf("jwt act=%v", claims.Act)
	}
	if claims.Subject != "sandbox/"+sb.ID {
		t.Fatalf("sub=%q", claims.Subject)
	}
}

func TestOIDCMintIgnoresGuestUserSubOverride(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	mem := newTestStore(t)
	srv := NewServer(mem)
	srv.OIDC = oidc.NewSignerFromKey(key, "http://issuer.test")
	mux := testMux(srv)

	body := `{"tenant_id":"t1","image_ref":"img","cpu_millis":1,"memory_mib":64,"owner_sub":"user:alice"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	var sb store.Sandbox
	_ = json.Unmarshal(rr.Body.Bytes(), &sb)

	// Guest/node tries to forge user_sub and act — must be ignored.
	mintBody := `{"sandbox_id":"` + sb.ID + `","aud":"https://api.example.com","user_sub":"user:eve","act":{"sub":"user:eve"}}`
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/internal/oidc/token", bytes.NewBufferString(mintBody)))
	if rr.Code != http.StatusOK {
		t.Fatalf("mint=%d %s", rr.Code, rr.Body.String())
	}
	var mint map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &mint)
	claimsOut, _ := mint["claims"].(map[string]any)
	if claimsOut["user_sub"] != "user:alice" {
		t.Fatalf("forged user_sub accepted: %v", claimsOut["user_sub"])
	}
	act, _ := claimsOut["act"].(map[string]any)
	if act == nil || act["sub"] != "user:alice" {
		t.Fatalf("forged act accepted: %v", claimsOut["act"])
	}
}

func TestOIDCMintLabWithoutOwnerStillWorks(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	mem := newTestStore(t)
	srv := NewServer(mem)
	srv.OIDC = oidc.NewSignerFromKey(key, "http://issuer.test")
	mux := testMux(srv)

	body := `{"tenant_id":"t1","image_ref":"img","cpu_millis":1,"memory_mib":64}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	var sb store.Sandbox
	_ = json.Unmarshal(rr.Body.Bytes(), &sb)
	if sb.OwnerSub != "" {
		t.Fatalf("lab owner should be empty, got %q", sb.OwnerSub)
	}

	mintBody := `{"sandbox_id":"` + sb.ID + `","aud":"https://api.example.com"}`
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/internal/oidc/token", bytes.NewBufferString(mintBody)))
	if rr.Code != http.StatusOK {
		t.Fatalf("lab mint=%d %s", rr.Code, rr.Body.String())
	}
	var mint map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &mint)
	claimsOut, _ := mint["claims"].(map[string]any)
	if _, ok := claimsOut["user_sub"]; ok {
		t.Fatalf("lab mint must omit user_sub: %v", claimsOut)
	}
	if _, ok := claimsOut["act"]; ok {
		t.Fatalf("lab mint must omit act: %v", claimsOut)
	}
	if claimsOut["tenant_id"] != "t1" {
		t.Fatalf("claims=%v", claimsOut)
	}
}
