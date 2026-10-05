package execproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testCA mirrors the control plane's enrollment CA (the node-agent module cannot
// import control-plane/internal/pki).
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns PEM cert and key for a leaf signed by the CA.
func (ca *testCA) issue(t *testing.T, cn, ou string, ekus []x509.ExtKeyUsage, dns []string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn, OrganizationalUnit: []string{ou}},
		DNSNames:     dns,
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  ekus,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func writeFiles(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

var (
	nodeEKUs   = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}
	clientEKUs = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
)

func TestRemoteListenerAcceptsOnlyTheControlPlane(t *testing.T) {
	ca := newTestCA(t)
	nodeCert, nodeKey := ca.issue(t, "n1", "nodes", nodeEKUs, []string{"n1"})
	dir := writeFiles(t, map[string][]byte{"client.crt": nodeCert, "client.key": nodeKey, "ca.crt": ca.pem})
	tlsCfg, err := MTLSConfig(filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key"), filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	srv, ln, err := ListenAndServeTLS("127.0.0.1:0", (&Server{}).RemoteHandler(), tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	base := "https://" + ln.Addr().String()

	clientWith := func(certPEM, keyPEM []byte) *http.Client {
		cfg := &tls.Config{RootCAs: x509.NewCertPool(), ServerName: "n1", MinVersion: tls.VersionTLS12}
		cfg.RootCAs.AddCert(ca.cert)
		if certPEM != nil {
			pair, err := tls.X509KeyPair(certPEM, keyPEM)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Certificates = []tls.Certificate{pair}
		}
		return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
	}

	cpCert, cpKey := ca.issue(t, ControlPlaneCN, OUControlPlane, clientEKUs, nil)
	cp := clientWith(cpCert, cpKey)
	resp, err := cp.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("control plane: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("control plane healthz: %d", resp.StatusCode)
	}
	// Operator routes are not served to the network.
	resp, err = cp.Post(base+"/v1/internal/ssh-agent/approve", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("approve over the remote listener: want 404, got %d", resp.StatusCode)
	}

	otherNodeCert, otherNodeKey := ca.issue(t, "n2", "nodes", nodeEKUs, []string{"n2"})
	if _, err := clientWith(otherNodeCert, otherNodeKey).Get(base + "/healthz"); err == nil {
		t.Fatal("another node's certificate must not reach the exec API")
	}
	if _, err := clientWith(nil, nil).Get(base + "/healthz"); err == nil {
		t.Fatal("a client without certificate must be refused")
	}
	otherCA := newTestCA(t)
	foreignCert, foreignKey := otherCA.issue(t, ControlPlaneCN, OUControlPlane, clientEKUs, nil)
	if _, err := clientWith(foreignCert, foreignKey).Get(base + "/healthz"); err == nil {
		t.Fatal("a control-plane name from another CA must be refused")
	}
}

func TestMTLSConfigRejectsAClientOnlyNodeCertificate(t *testing.T) {
	ca := newTestCA(t)
	cert, key := ca.issue(t, "n1", "nodes", clientEKUs, []string{"n1"})
	dir := writeFiles(t, map[string][]byte{"client.crt": cert, "client.key": key, "ca.crt": ca.pem})
	_, err := MTLSConfig(filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key"), filepath.Join(dir, "ca.crt"))
	if !errors.Is(err, ErrNoServerAuth) {
		t.Fatalf("want ErrNoServerAuth, got %v", err)
	}
}
