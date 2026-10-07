package api

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/pki"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

func newSANEnv(t *testing.T) (http.Handler, *Server) {
	t.Helper()
	t.Setenv("ASP_NODE_BOOTSTRAP_TOKEN", "boot-secret")
	ca, err := pki.GenerateCA("test", pki.DefaultNodeTTL)
	if err != nil {
		t.Fatal(err)
	}
	mem := store.NewMemoryStore()
	if _, err := mem.EnsureAPIKey("default", "ops", store.APIKeyScopePlatform, "asp_ops1", store.HashAPIKeySecret("platform-key")); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(mem)
	srv.CA = ca
	srv.ReservedNodeNames = []string{"cp.example.com", "203.0.113.9", "Control-Plane"}
	return AuthMiddleware(mem, AuthConfig{})(testMux(srv)), srv
}

func postJSON(h http.Handler, path, bearer, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// The endpoints a node names at enrollment are its own business; they must
// not end up in its certificate, where they would make it valid for them (#104).
func TestNodeCertificateCarriesOnlyTheNodeID(t *testing.T) {
	h, _ := newSANEnv(t)
	rr := postJSON(h, "/v1/nodes/enroll", "boot-secret",
		`{"id":"node-a","agent_endpoint":"https://cp.example.com:9443","endpoint":"https://203.0.113.9:9443"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("enroll: %d %s", rr.Code, rr.Body.String())
	}
	var out enrollResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(out.ClientCertPEM))
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "node-a" || len(cert.IPAddresses) != 0 {
		t.Fatalf("names in the certificate: dns=%v ip=%v", cert.DNSNames, cert.IPAddresses)
	}
	// It is not valid for the control plane's name.
	if err := cert.VerifyHostname("cp.example.com"); err == nil {
		t.Fatal("a node certificate is valid for the control plane's hostname")
	}
	if err := cert.VerifyHostname("203.0.113.9"); err == nil {
		t.Fatal("a node certificate is valid for the control plane's address")
	}

	// Rotating issues the same: still only the id.
	rr = postJSON(h, "/v1/nodes/node-a/rotate-cert", "platform-key", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rr.Code, rr.Body.String())
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	block, _ = pem.Decode([]byte(out.ClientCertPEM))
	cert, _ = x509.ParseCertificate(block.Bytes)
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "node-a" {
		t.Fatalf("rotated certificate names: %v", cert.DNSNames)
	}
}

// A node cannot take the control plane's name as its id.
func TestNodeCannotEnrollUnderTheControlPlanesName(t *testing.T) {
	h, _ := newSANEnv(t)
	for _, id := range []string{"cp.example.com", "CP.Example.COM", "control-plane", "203.0.113.9", "10.0.0.5", "::1"} {
		body := `{"id":"` + id + `","agent_endpoint":"http://127.0.0.1:9100"}`
		if rr := postJSON(h, "/v1/nodes/enroll", "boot-secret", body); rr.Code != http.StatusBadRequest {
			t.Errorf("enroll as %q: %d %s, want 400", id, rr.Code, rr.Body.String())
		}
		if rr := postJSON(h, "/v1/nodes/enroll-tokens", "platform-key", `{"node_id":"`+id+`"}`); rr.Code != http.StatusBadRequest {
			t.Errorf("a token pinned to %q: %d %s, want 400", id, rr.Code, rr.Body.String())
		}
	}
	// An ordinary name next to it is fine.
	if rr := postJSON(h, "/v1/nodes/enroll", "boot-secret", `{"id":"cp-node.example.com","agent_endpoint":"http://127.0.0.1:9100"}`); rr.Code != http.StatusCreated {
		t.Fatalf("an ordinary node id: %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(postJSON(h, "/v1/nodes/enroll", "boot-secret", `{"id":"cp.example.com"}`).Body.String(), "control plane") {
		t.Error("the refusal does not say why")
	}
}

// A wildcard in the control plane's certificate reserves every name it covers.
func TestReservedWildcardNames(t *testing.T) {
	s := &Server{ReservedNodeNames: []string{"*.lab.example.com", "cp.example.com"}}
	for id, reserved := range map[string]bool{
		"asp.lab.example.com":   true,
		"ASP.Lab.Example.com":   true,
		"lab.example.com":       false, // the wildcard does not cover its own apex
		"a.b.lab.example.com":   false, // one label only, as in a certificate
		"asp.other.example.com": false,
		"cp.example.com":        true,
		"node-1":                false,
	} {
		if got := s.checkNodeNameFree(id) != nil; got != reserved {
			t.Errorf("checkNodeNameFree(%q): reserved=%v, want %v", id, got, reserved)
		}
	}
}
