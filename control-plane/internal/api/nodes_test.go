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
	if len(cert.IPAddresses) != 1 || cert.IPAddresses[0].String() != "10.0.0.7" {
		t.Fatalf("endpoint host missing from SANs: %v", cert.IPAddresses)
	}

	if rr := enroll(`{"id":"bad id","agent_endpoint":"https://10.0.0.7:9443"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid node id: want 400, got %d %s", rr.Code, rr.Body.String())
	}
	if rr := enroll(`{"id":"asp-control-plane"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("reserved node id: want 400, got %d %s", rr.Code, rr.Body.String())
	}
}
