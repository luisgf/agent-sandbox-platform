package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/attest"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/oidc"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/pki"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

type identityFixture struct {
	t       *testing.T
	h       http.Handler
	certs   map[string]*x509.Certificate
	sbID    string
	attestK *attest.SoftwareAttestor
}

// newIdentityFixture: nodes n1 and n2, one sandbox assigned to n1, and an mTLS-mode
// middleware (ASP_CLIENT_CA set) in front of the routes.
func newIdentityFixture(t *testing.T) *identityFixture {
	t.Helper()
	t.Setenv("ASP_AUTO_PROVISION", "0")
	ca, err := pki.GenerateCA("test", pki.DefaultNodeTTL)
	if err != nil {
		t.Fatal(err)
	}
	mem := store.NewMemoryStore()
	f := &identityFixture{t: t, certs: map[string]*x509.Certificate{}}
	for _, id := range []string{"n1", "n2"} {
		if _, err := mem.RegisterNode(store.RegisterNodeInput{ID: id, AgentEndpoint: "http://127.0.0.1:9100"}); err != nil {
			t.Fatal(err)
		}
		issued, err := ca.IssueNodeClient(id, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := pki.ParseCertPEM(issued.CertPEM)
		if err != nil {
			t.Fatal(err)
		}
		f.certs[id] = cert
	}
	sb, err := mem.CreateSandbox(store.CreateSandboxInput{
		TenantID: "t1", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 512, NodeID: "n1",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.sbID = sb.ID

	srv := NewServer(mem)
	attKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.attestK = attest.NewSoftwareAttestorFromKey(attKey)
	srv.Attestor = f.attestK
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv.OIDC = oidc.NewSignerFromKey(rsaKey, "http://issuer.test")
	f.h = AuthMiddleware(mem, AuthConfig{RequireNodeClientCert: true, RejectRevokedCerts: true})(testMux(srv))
	return f
}

func (f *identityFixture) do(cert *x509.Certificate, method, path, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if cert != nil {
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	}
	rr := httptest.NewRecorder()
	f.h.ServeHTTP(rr, req)
	return rr
}

func (f *identityFixture) attestBody(nodeID string) string {
	f.t.Helper()
	ev, err := f.attestK.Attest(context.Background(), attest.BootStatement{
		SandboxID: f.sbID, ImageDigest: "sha256:abc", VMMProfile: "cloud-hypervisor",
		CID: 3, NodeID: nodeID, TS: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	b, _ := json.Marshal(ev)
	return string(b)
}

func TestNodeRoutesRejectAnotherNodesCertificate(t *testing.T) {
	f := newIdentityFixture(t)
	n2 := f.certs["n2"]
	sb := "/v1/sandboxes/" + f.sbID
	cases := []struct {
		name, method, path, body string
	}{
		{"register another node", http.MethodPost, "/v1/nodes/register", `{"id":"n1","agent_endpoint":"http://attacker:9100"}`},
		{"heartbeat another node", http.MethodPost, "/v1/nodes/n1/heartbeat", ""},
		{"work of another node", http.MethodGet, "/v1/nodes/n1/work", ""},
		{"claim as another node", http.MethodPost, sb + "/claim", `{"node_id":"n1"}`},
		{"status of a sandbox on another node", http.MethodPost, sb + "/status", `{"state":"failed","detail":"x"}`},
		{"oidc mint for a sandbox on another node", http.MethodPost, "/v1/internal/oidc/token", `{"sandbox_id":"` + f.sbID + `","aud":"https://api.example"}`},
		{"local-net key for a sandbox on another node", http.MethodPost, sb + "/local-net/node-public", `{"public_key":"ERERERERERERERERERERERERERERERERERERERERERE="}`},
		{"attest a sandbox on another node", http.MethodPost, sb + "/attest", f.attestBody("n2")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := f.do(n2, tc.method, tc.path, tc.body)
			// The 403 must come from the identity check, not from another guard.
			if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), `node \"n2\"`) && !strings.Contains(rr.Body.String(), "not n2") {
				t.Fatalf("want an identity 403, got %d %s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestNodeRoutesAcceptTheNodesOwnCertificate(t *testing.T) {
	f := newIdentityFixture(t)
	n1 := f.certs["n1"]
	sb := "/v1/sandboxes/" + f.sbID
	cases := []struct {
		name, method, path, body string
	}{
		{"register itself with an empty id", http.MethodPost, "/v1/nodes/register", `{"agent_endpoint":"http://127.0.0.1:9100"}`},
		{"heartbeat", http.MethodPost, "/v1/nodes/n1/heartbeat", ""},
		{"work", http.MethodGet, "/v1/nodes/n1/work", ""},
		{"claim with node from the certificate", http.MethodPost, sb + "/claim", `{}`},
		{"attest", http.MethodPost, sb + "/attest", f.attestBody("n1")},
		{"oidc mint", http.MethodPost, "/v1/internal/oidc/token", `{"sandbox_id":"` + f.sbID + `","aud":"https://api.example"}`},
		{"status", http.MethodPost, sb + "/status", `{"state":"running","detail":"ok"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := f.do(n1, tc.method, tc.path, tc.body)
			if rr.Code != http.StatusOK {
				t.Fatalf("want 200, got %d %s", rr.Code, rr.Body.String())
			}
		})
	}
	// register with an empty id registered n1 (from the certificate), not a new node.
	rr := f.do(n1, http.MethodPost, "/v1/nodes/register", `{"agent_endpoint":"http://127.0.0.1:9101"}`)
	var node store.Node
	_ = json.Unmarshal(rr.Body.Bytes(), &node)
	if node.ID != "n1" || node.AgentEndpoint != "http://127.0.0.1:9101" {
		t.Fatalf("register from certificate: %+v", node)
	}
}

func TestNodeRoutesRejectCertificatesThatAreNotNodeCertificates(t *testing.T) {
	f := newIdentityFixture(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(42),
		Subject:      pkix.Name{CommonName: "n1", OrganizationalUnit: []string{"humans"}},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	notNode, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if rr := f.do(notNode, http.MethodPost, "/v1/nodes/n1/heartbeat", ""); rr.Code != http.StatusForbidden {
		t.Fatalf("want 403 for a non-node certificate, got %d %s", rr.Code, rr.Body.String())
	}
	if rr := f.do(nil, http.MethodPost, "/v1/nodes/n1/heartbeat", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without a certificate, got %d", rr.Code)
	}
}

// Open lab (no ASP_CLIENT_CA): node routes keep their previous behaviour.
func TestNodeIdentityNotEnforcedInOpenLab(t *testing.T) {
	mem := store.NewMemoryStore()
	if _, err := mem.RegisterNode(store.RegisterNodeInput{ID: "n1", AgentEndpoint: "http://127.0.0.1:9100"}); err != nil {
		t.Fatal(err)
	}
	h := AuthMiddleware(mem, AuthConfig{})(testMux(NewServer(mem)))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/nodes/n1/heartbeat", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("open lab heartbeat: want 200, got %d %s", rr.Code, rr.Body.String())
	}
}

// Lease renewal is gone; an old node agent learns why instead of a 404.
func TestRenewLeaseIsGone(t *testing.T) {
	f := newIdentityFixture(t)
	rr := f.do(f.certs["n1"], http.MethodPost, "/v1/sandboxes/"+f.sbID+"/renew-lease", `{"node_id":"n1"}`)
	if rr.Code != http.StatusGone || !strings.Contains(rr.Body.String(), "/work") {
		t.Fatalf("renew-lease: want 410 pointing at the work poll, got %d %s", rr.Code, rr.Body.String())
	}
}
