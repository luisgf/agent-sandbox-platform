package api

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/pki"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// startMTLSAgent serves h as a node agent's control-plane listener does: TLS with
// the given server certificate, client certificates required and verified against
// the CA, and HTTP/2 offered like --agent-tls-listen.
func startMTLSAgent(t *testing.T, ca *pki.CA, certPEM, keyPEM []byte, h http.Handler) *httptest.Server {
	t.Helper()
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewUnstartedServer(h)
	s.EnableHTTP2 = true
	s.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{pair},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.CertPool(),
	}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

// fakeTLSAgent answers every exec and records the client identities it saw.
type fakeTLSAgent struct {
	*httptest.Server
	mu      sync.Mutex
	peerCNs []string
}

func newFakeTLSAgent(t *testing.T, ca *pki.CA, certPEM, keyPEM []byte) *fakeTLSAgent {
	t.Helper()
	a := &fakeTLSAgent{}
	a.Server = startMTLSAgent(t, ca, certPEM, keyPEM, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.peerCNs = append(a.peerCNs, r.TLS.PeerCertificates[0].Subject.CommonName)
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stdout":"hello\n","stderr":"","exit_code":0}`))
	}))
	return a
}

type mtlsExecFixture struct {
	t   *testing.T
	ca  *pki.CA
	mem *store.MemoryStore
	srv *Server
	mux http.Handler
}

func newMTLSExecFixture(t *testing.T) *mtlsExecFixture {
	t.Helper()
	t.Setenv("ASP_AUTO_PROVISION", "0")
	ca, err := pki.GenerateCA("test", pki.DefaultNodeTTL)
	if err != nil {
		t.Fatal(err)
	}
	mem := store.NewMemoryStore()
	srv := NewServer(mem)
	srv.CA = ca
	srv.Agents = NewAgentDialer(ca, false)
	return &mtlsExecFixture{t: t, ca: ca, mem: mem, srv: srv, mux: testMux(srv)}
}

// sandboxOn registers nodeID at endpoint and returns a sandbox assigned to it.
func (f *mtlsExecFixture) sandboxOn(nodeID, endpoint string) string {
	f.t.Helper()
	if _, err := f.mem.RegisterNode(store.RegisterNodeInput{ID: nodeID, AgentEndpoint: endpoint}); err != nil {
		f.t.Fatal(err)
	}
	sb, err := f.mem.CreateSandbox(store.CreateSandboxInput{TenantID: "t1", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 512, NodeID: nodeID})
	if err != nil {
		f.t.Fatal(err)
	}
	runSandbox(f.t, f.mem, sb.ID)
	return sb.ID
}

func (f *mtlsExecFixture) exec(sandboxID string) *httptest.ResponseRecorder {
	f.t.Helper()
	rr := httptest.NewRecorder()
	f.mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sandboxID+"/exec", bytes.NewBufferString(`{"cmd":["echo","hello"]}`)))
	return rr
}

func TestExecReachesTheAssignedNodeOverMutualTLS(t *testing.T) {
	f := newMTLSExecFixture(t)
	issued, err := f.ca.IssueNodeCert("n1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	agent := newFakeTLSAgent(t, f.ca, issued.CertPEM, issued.KeyPEM)

	rr := f.exec(f.sandboxOn("n1", agent.URL))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "hello") {
		t.Fatalf("exec over mTLS: %d %s", rr.Code, rr.Body.String())
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.peerCNs) != 1 || agent.peerCNs[0] != pki.ControlPlaneCN {
		t.Fatalf("agent saw client identities %v, want [%s]", agent.peerCNs, pki.ControlPlaneCN)
	}
}

// A node whose endpoint points at another node's agent must not get its traffic.
func TestExecRefusesAnAgentWithAnotherNodesCertificate(t *testing.T) {
	f := newMTLSExecFixture(t)
	issued, err := f.ca.IssueNodeCert("n1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	agent := newFakeTLSAgent(t, f.ca, issued.CertPEM, issued.KeyPEM)

	rr := f.exec(f.sandboxOn("n2", agent.URL))
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "certificate") {
		t.Fatalf("want 502 certificate error, got %d %s", rr.Code, rr.Body.String())
	}
}

// Certificates issued before the server-auth profile cannot serve the control plane.
func TestExecRefusesAClientOnlyNodeCertificate(t *testing.T) {
	f := newMTLSExecFixture(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7),
		Subject:      pkix.Name{CommonName: "n1", OrganizationalUnit: []string{pki.OUNodes}},
		DNSNames:     []string{"n1"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, f.ca.Cert, &key.PublicKey, f.ca.Key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	agent := newFakeTLSAgent(t, f.ca,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))

	if rr := f.exec(f.sandboxOn("n1", agent.URL)); rr.Code != http.StatusBadGateway {
		t.Fatalf("want 502 for a client-only certificate, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestExecRefusesPlainHTTPToAnotherHost(t *testing.T) {
	f := newMTLSExecFixture(t)
	rr := f.exec(f.sandboxOn("n1", "http://10.0.0.9:9100"))
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), EnvInsecureAgentHTTP) {
		t.Fatalf("want 502 with the insecure hint, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestExecRefusesARevokedNode(t *testing.T) {
	f := newMTLSExecFixture(t)
	id := f.sandboxOn("n1", "http://127.0.0.1:9")
	if _, err := f.mem.RevokeNode("n1"); err != nil {
		t.Fatal(err)
	}
	if rr := f.exec(id); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "revoked") {
		t.Fatalf("want 409 revoked, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestRegisterRejectsPlainHTTPAgentEndpointOnAnotherHost(t *testing.T) {
	f := newMTLSExecFixture(t)
	register := func(endpoint string) int {
		rr := httptest.NewRecorder()
		f.mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/nodes/register",
			bytes.NewBufferString(`{"id":"n1","agent_endpoint":"`+endpoint+`"}`)))
		return rr.Code
	}
	for endpoint, want := range map[string]int{
		"http://10.0.0.9:9100":    http.StatusBadRequest,
		"ftp://10.0.0.9:9100":     http.StatusBadRequest,
		"http://127.0.0.1:9100":   http.StatusOK,
		"http://localhost:9100":   http.StatusOK,
		"https://10.0.0.9:9443":   http.StatusOK,
		"https://node1.lab:9443/": http.StatusOK,
	} {
		if got := register(endpoint); got != want {
			t.Errorf("register %s: got %d, want %d", endpoint, got, want)
		}
	}
	f.srv.Agents.AllowInsecureHTTP = true
	if got := register("http://10.0.0.9:9100"); got != http.StatusOK {
		t.Fatalf("with %s=1: got %d, want 200", EnvInsecureAgentHTTP, got)
	}
}
