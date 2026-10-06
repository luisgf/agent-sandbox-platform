package api

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/pki"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// A node renews its own certificate with the current one, so certificates do
// not silently expire a year after enrollment.
func TestNodeRotatesItsOwnCertificate(t *testing.T) {
	ca, err := pki.GenerateCA("test", pki.DefaultNodeTTL)
	if err != nil {
		t.Fatal(err)
	}
	mem := store.NewMemoryStore()
	if _, err := mem.EnsureAPIKey("default", "ops", store.APIKeyScopePlatform, "asp_ops", store.HashAPIKeySecret("platform-key")); err != nil {
		t.Fatal(err)
	}
	certs := map[string]*x509.Certificate{}
	for _, id := range []string{"node-a", "node-b"} {
		issued, err := ca.IssueNodeCert(id, nil, pki.DefaultNodeTTL)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := mem.EnrollNode(store.EnrollNodeInput{ID: id, AgentEndpoint: "http://127.0.0.1:9100"},
			store.CertMeta{Fingerprint: issued.Fingerprint, Serial: issued.Serial, NotAfter: issued.NotAfter}, store.EnrollAuth{}); err != nil {
			t.Fatal(err)
		}
		if certs[id], err = pki.ParseCertPEM(issued.CertPEM); err != nil {
			t.Fatal(err)
		}
	}
	srv := NewServer(mem)
	srv.CA = ca
	h := AuthMiddleware(mem, AuthConfig{RequireNodeClientCert: true, RejectRevokedCerts: true})(testMux(srv))
	rotate := func(node string, peer *x509.Certificate) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/nodes/"+node+"/rotate-cert", nil)
		if peer != nil {
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{peer}}
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	if rr := rotate("node-a", certs["node-b"]); rr.Code != http.StatusForbidden {
		t.Fatalf("node-b rotating node-a: want 403, got %d %s", rr.Code, rr.Body.String())
	}
	if rr := rotate("node-a", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no certificate and no token: want 401, got %d %s", rr.Code, rr.Body.String())
	}

	before, _ := mem.GetNode("node-a")
	rr := rotate("node-a", certs["node-a"])
	if rr.Code != http.StatusOK {
		t.Fatalf("node-a rotating itself: %d %s", rr.Code, rr.Body.String())
	}
	var er enrollResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &er); err != nil {
		t.Fatal(err)
	}
	after, _ := mem.GetNode("node-a")
	if after.CertFingerprint != er.CertFingerprint || after.CertFingerprint == before.CertFingerprint {
		t.Fatalf("new fingerprint not recorded: before %s after %s response %s", before.CertFingerprint, after.CertFingerprint, er.CertFingerprint)
	}
	if revoked, _ := mem.IsCertRevoked(before.CertFingerprint); !revoked {
		t.Fatal("the certificate that was rotated out must be revoked")
	}
	if after.CertNotAfter == nil || time.Until(*after.CertNotAfter) < 300*24*time.Hour {
		t.Fatalf("cert_not_after: %v", after.CertNotAfter)
	}
	// The old certificate is revoked: it cannot rotate again.
	if rr := rotate("node-a", certs["node-a"]); rr.Code != http.StatusUnauthorized {
		t.Fatalf("rotating with the revoked certificate: want 401, got %d", rr.Code)
	}

	// Operators see the expiry in the node list.
	req := httptest.NewRequest(http.MethodGet, "/v1/nodes", nil)
	req.Header.Set("Authorization", "Bearer platform-key")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var list struct {
		Nodes []struct {
			ID           string     `json:"id"`
			CertNotAfter *time.Time `json:"cert_not_after"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil || len(list.Nodes) != 2 {
		t.Fatalf("node list: %d %s", rr.Code, rr.Body.String())
	}
	for _, n := range list.Nodes {
		if n.CertNotAfter == nil {
			t.Errorf("node %s: no cert_not_after in the list", n.ID)
		}
	}
}
