package cpclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// serverCAFile writes the httptest server's certificate, which signs itself.
func serverCAFile(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cp-ca.pem")
	b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// selfSignedPEM returns a throwaway certificate and key.
func selfSignedPEM(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func TestEnrollTrustsTheGivenControlPlaneCA(t *testing.T) {
	var got EnrollRequest
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/nodes/enroll" || r.Header.Get("Authorization") != "Bearer boot" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(EnrollResponse{NodeID: "n1", ClientCertPEM: "c", ClientKeyPEM: "k", CACertPEM: "ca"})
	}))
	defer srv.Close()

	untrusted, err := NewEnrollHTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(srv.URL, untrusted).Enroll(context.Background(), "boot", EnrollRequest{ID: "n1"}); err == nil {
		t.Fatal("enroll must fail when the control plane's certificate is not trusted")
	}

	trusted, err := NewEnrollHTTPClient(serverCAFile(t, srv))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := New(srv.URL, trusted).Enroll(context.Background(), "boot", EnrollRequest{ID: "n1", AgentEndpoint: "https://n1:9443"})
	if err != nil {
		t.Fatalf("enroll with --control-plane-ca: %v", err)
	}
	if resp.NodeID != "n1" || got.AgentEndpoint != "https://n1:9443" {
		t.Fatalf("resp=%+v request=%+v", resp, got)
	}
}

func TestLoadMTLSClientPrefersTheControlPlaneCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	certDir := t.TempDir()
	clientCert, clientKey := selfSignedPEM(t, "n1")
	enrollCA, _ := selfSignedPEM(t, "enrollment-ca") // does not sign the server certificate
	for name, b := range map[string][]byte{"client.crt": clientCert, "client.key": clientKey, "ca.crt": enrollCA} {
		if err := os.WriteFile(filepath.Join(certDir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	c, mtls, err := LoadMTLSClient(certDir, false, "")
	if err != nil || !mtls {
		t.Fatalf("mtls=%v err=%v", mtls, err)
	}
	if _, err := c.Get(srv.URL); err == nil {
		t.Fatal("without --control-plane-ca the enrollment CA must be the only root")
	}

	c, mtls, err = LoadMTLSClient(certDir, false, serverCAFile(t, srv))
	if err != nil || !mtls {
		t.Fatalf("mtls=%v err=%v", mtls, err)
	}
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("with --control-plane-ca: %v", err)
	}
	resp.Body.Close()
}

func TestLoadMTLSClientWithoutCertificates(t *testing.T) {
	c, mtls, err := LoadMTLSClient(t.TempDir(), false, "")
	if err != nil || mtls || c == nil {
		t.Fatalf("no certs, not forced: client=%v mtls=%v err=%v", c, mtls, err)
	}
	if _, _, err := LoadMTLSClient(t.TempDir(), true, ""); err == nil {
		t.Fatal("ASP_MTLS=1 without certificates must fail")
	}
	if _, err := NewEnrollHTTPClient(filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Fatal("a missing --control-plane-ca file must fail")
	}
}
