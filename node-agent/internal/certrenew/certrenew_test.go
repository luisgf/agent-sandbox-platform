package certrenew

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/cpclient"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/execproxy"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
	pool *x509.CertPool
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-24 * time.Hour), NotAfter: time.Now().Add(1000 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pool: pool}
}

// issue signs a leaf usable as client and server certificate.
func (ca *testCA) issue(t *testing.T, cn, ou string, notBefore, notAfter time.Time) (certPEM, keyPEM []byte, serial *big.Int) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ = rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: cn, OrganizationalUnit: []string{ou}},
		DNSNames: []string{cn}, NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), serial
}

// fakeCP is a control plane that requires the node's client certificate and
// records the serial of every client certificate it sees.
type fakeCP struct {
	t       *testing.T
	ca      *testCA
	srv     *httptest.Server
	mu      sync.Mutex
	seen    []*big.Int
	refuse  bool
	rotated []*big.Int
}

func newFakeCP(t *testing.T, ca *testCA) *fakeCP {
	f := &fakeCP{t: t, ca: ca}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/nodes/{id}/rotate-cert", func(w http.ResponseWriter, r *http.Request) {
		peer := f.record(r)
		f.mu.Lock()
		refuse := f.refuse
		f.mu.Unlock()
		if refuse || peer.Subject.CommonName != r.PathValue("id") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		c, k, serial := ca.issue(t, "node-a", "nodes", time.Now().Add(-time.Minute), time.Now().Add(24*time.Hour))
		f.mu.Lock()
		f.rotated = append(f.rotated, serial)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{
			"node_id": "node-a", "client_cert_pem": string(c), "client_key_pem": string(k),
			"ca_cert_pem": string(ca.pem), "cert_fingerprint": serial.String(),
		})
	})
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		_, _ = w.Write([]byte("ok"))
	})
	f.srv = httptest.NewUnstartedServer(mux)
	f.srv.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: ca.pool}
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCP) record(r *http.Request) *x509.Certificate {
	peer := r.TLS.PeerCertificates[0]
	f.mu.Lock()
	f.seen = append(f.seen, peer.SerialNumber)
	f.mu.Unlock()
	return peer
}

func (f *fakeCP) lastSeen() *big.Int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[len(f.seen)-1]
}

func (f *fakeCP) caFile(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "cp-ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// setup writes a node certificate valid from notBefore to notAfter into a
// cert-dir and returns the node's mTLS client.
func setup(t *testing.T, ca *testCA, f *fakeCP, notBefore, notAfter time.Time) (*http.Client, *cpclient.NodeCert, string, *big.Int) {
	t.Helper()
	certPEM, keyPEM, serial := ca.issue(t, "node-a", "nodes", notBefore, notAfter)
	dir := t.TempDir()
	if err := cpclient.WriteCerts(dir, string(certPEM), string(keyPEM), string(ca.pem)); err != nil {
		t.Fatal(err)
	}
	client, nc, err := cpclient.LoadMTLSClientCert(dir, true, f.caFile(t))
	if err != nil {
		t.Fatal(err)
	}
	return client, nc, dir, serial
}

func ping(t *testing.T, c *http.Client, base string) {
	t.Helper()
	resp, err := c.Get(base + "/ping")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func onDiskSerial(t *testing.T, dir string) *big.Int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "client.crt"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert.SerialNumber
}

// servedSerial dials the node's control-plane TLS listener as the control
// plane and returns the serial of the certificate the node presents.
func servedSerial(t *testing.T, ca *testCA, addr string) *big.Int {
	t.Helper()
	cpCert, cpKey, _ := ca.issue(t, execproxy.ControlPlaneCN, execproxy.OUControlPlane, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
	pair, err := tls.X509KeyPair(cpCert, cpKey)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: ca.pool, ServerName: "node-a", Certificates: []tls.Certificate{pair}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0].SerialNumber
}

func TestRenewalRotatesAndPresentsTheNewCertificate(t *testing.T) {
	ca := newTestCA(t)
	f := newFakeCP(t, ca)
	now := time.Now()
	// Four hours of life, one left: less than a third, so renewal is due.
	client, nc, dir, oldSerial := setup(t, ca, f, now.Add(-3*time.Hour), now.Add(time.Hour))

	tlsCfg, err := execproxy.MTLSConfigFor(nc.Current, filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				_ = c.(*tls.Conn).Handshake()
				_ = c.Close()
			}(c)
		}
	}()

	ping(t, client, f.srv.URL)
	if f.lastSeen().Cmp(oldSerial) != 0 || servedSerial(t, ca, ln.Addr().String()).Cmp(oldSerial) != 0 {
		t.Fatal("before renewal the node should present its original certificate")
	}

	r := &Renewer{CP: cpclient.New(f.srv.URL, client), Cert: nc, NodeID: "node-a", OnRotate: client.CloseIdleConnections}
	rotated, err := r.Check(context.Background())
	if err != nil || !rotated {
		t.Fatalf("Check: rotated=%v err=%v", rotated, err)
	}
	newSerial := f.rotated[0]
	if onDiskSerial(t, dir).Cmp(newSerial) != 0 || nc.Leaf().SerialNumber.Cmp(newSerial) != 0 {
		t.Fatal("the renewed certificate is neither on disk nor loaded")
	}
	ping(t, client, f.srv.URL)
	if f.lastSeen().Cmp(newSerial) != 0 {
		t.Fatalf("the control-plane client still presents serial %v, want %v", f.lastSeen(), newSerial)
	}
	if got := servedSerial(t, ca, ln.Addr().String()); got.Cmp(newSerial) != 0 {
		t.Fatalf("the TLS listener still serves serial %v, want %v", got, newSerial)
	}

	// A fresh certificate is left alone.
	if rotated, err := r.Check(context.Background()); err != nil || rotated {
		t.Fatalf("fresh certificate: rotated=%v err=%v", rotated, err)
	}
	if len(f.rotated) != 1 {
		t.Fatalf("rotate-cert called %d times", len(f.rotated))
	}
}

func TestRenewalFailureKeepsTheCertificate(t *testing.T) {
	ca := newTestCA(t)
	f := newFakeCP(t, ca)
	f.refuse = true
	now := time.Now()
	client, nc, dir, oldSerial := setup(t, ca, f, now.Add(-3*time.Hour), now.Add(time.Hour))
	r := &Renewer{CP: cpclient.New(f.srv.URL, client), Cert: nc, NodeID: "node-a"}
	if rotated, err := r.Check(context.Background()); err == nil || rotated {
		t.Fatalf("refused renewal: rotated=%v err=%v", rotated, err)
	}
	if onDiskSerial(t, dir).Cmp(oldSerial) != 0 || nc.Leaf().SerialNumber.Cmp(oldSerial) != 0 {
		t.Fatal("a failed renewal must keep the current certificate")
	}
}

func TestDue(t *testing.T) {
	now := time.Now()
	leaf := &x509.Certificate{NotBefore: now.Add(-200 * 24 * time.Hour), NotAfter: now.Add(165 * 24 * time.Hour)}
	if Due(leaf, now) {
		t.Fatal("165 of 365 days left is not due")
	}
	if !Due(leaf, now.Add(50*24*time.Hour)) {
		t.Fatal("115 of 365 days left is due")
	}
}
