package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/attest"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/pki"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

func bootStatement(sandboxID, nodeID string) attest.BootStatement {
	return attest.BootStatement{
		SandboxID: sandboxID, NodeID: nodeID, ImageDigest: "sha256:abc", VMMProfile: "cloud-hypervisor", CID: 4,
		TS: time.Now().UTC().Format(time.RFC3339),
	}
}

func signStatement(t *testing.T, key *ecdsa.PrivateKey, stmt attest.BootStatement) []byte {
	t.Helper()
	ev, err := attest.NewSoftwareAttestorFromKey(key).Attest(context.Background(), stmt)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(ev)
	return b
}

// A bundle signed by a key the control plane does not trust is refused, even
// with that key attached.
func TestAttestRefusesABundleSignedByAnUnknownKey(t *testing.T) {
	cpKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	forger, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	mem := newTestStore(t, "node-a")
	srv := NewServer(mem)
	srv.Attestor = attest.NewSoftwareAttestorFromKey(cpKey)
	sb, err := mem.CreateSandbox(store.CreateSandboxInput{TenantID: "t1", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 512, NodeID: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	testMux(srv).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/attest",
		bytes.NewReader(signStatement(t, forger, bootStatement(sb.ID, "node-a")))))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("forged bundle: want 400, got %d %s", rr.Code, rr.Body.String())
	}
	if _, err := mem.GetAttestation(sb.ID); err == nil {
		t.Fatal("a refused bundle was stored")
	}
}

// Over mTLS a node signs with its certificate's key; the control plane
// verifies with the certificate the request came with.
func TestAttestWithTheNodeCertificateKey(t *testing.T) {
	ca, err := pki.GenerateCA("test", pki.DefaultNodeTTL)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := ca.IssueNodeCert("node-a", nil, pki.DefaultNodeTTL)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := pki.ParseCertPEM(issued.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	nodeKey, err := pki.ParseECKeyPEM(issued.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	cpKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	mem := newTestStore(t, "node-a")
	srv := NewServer(mem)
	srv.Attestor = attest.NewSoftwareAttestorFromKey(cpKey)
	h := AuthMiddleware(mem, AuthConfig{RequireNodeClientCert: true, InsecureOpen: true})(testMux(srv))
	sb, err := mem.CreateSandbox(store.CreateSandboxInput{TenantID: "t1", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 512, NodeID: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	post := func(body []byte, mtls bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/attest", bytes.NewReader(body))
		if mtls {
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	body := signStatement(t, nodeKey, bootStatement(sb.ID, "node-a"))
	if rr := post(body, false); rr.Code != http.StatusUnauthorized && rr.Code != http.StatusBadRequest {
		t.Fatalf("without the certificate the node key is not trusted: got %d %s", rr.Code, rr.Body.String())
	}
	if rr := post(body, true); rr.Code != http.StatusOK {
		t.Fatalf("signed with the node certificate key over mTLS: %d %s", rr.Code, rr.Body.String())
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if rr := post(signStatement(t, other, bootStatement(sb.ID, "node-a")), true); rr.Code != http.StatusBadRequest {
		t.Fatalf("over mTLS, a key other than the certificate's: want 400, got %d", rr.Code)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/sandboxes/"+sb.ID+"/attestation", nil))
	var got map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if rr.Code != http.StatusOK || got["fresh"] != true {
		t.Fatalf("attestation after the mTLS post: %d %v", rr.Code, got)
	}
}
