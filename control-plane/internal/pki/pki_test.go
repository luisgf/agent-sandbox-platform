package pki

import (
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGenerateIssueVerify(t *testing.T) {
	ca, err := GenerateCA("test-ca", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := ca.IssueNodeClient("node-abc", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if issued.Fingerprint == "" || len(issued.CertPEM) == 0 || len(issued.KeyPEM) == 0 {
		t.Fatalf("incomplete issue result: %+v", issued)
	}
	cert, err := ca.VerifyClientCert(issued.CertPEM)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if cert.Subject.CommonName != "node-abc" {
		t.Fatalf("cn=%q", cert.Subject.CommonName)
	}
	fp, err := FingerprintPEM(issued.CertPEM)
	if err != nil || fp != issued.Fingerprint {
		t.Fatalf("fingerprint mismatch: %q vs %q err=%v", fp, issued.Fingerprint, err)
	}
	// Wrong CA must fail.
	other, err := GenerateCA("other", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.VerifyClientCert(issued.CertPEM); err == nil {
		t.Fatal("expected verify failure against wrong CA")
	}
}

func TestLoadOrCreateDevCAPersists(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	ca1, err := LoadOrCreateDevCA(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	ca2, err := LoadOrCreateDevCA(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if Fingerprint(ca1.Cert) != Fingerprint(ca2.Cert) {
		t.Fatal("expected same CA fingerprint after reload")
	}
	issued, err := ca2.IssueNodeClient("n1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca1.VerifyClientCert(issued.CertPEM); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("ca key should be private, mode=%v", info.Mode())
	}
}

func TestIssueRequiresNodeID(t *testing.T) {
	ca, err := GenerateCA("ca", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.IssueNodeClient("  ", time.Hour); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := ParseCertPEM([]byte("not-pem")); err == nil {
		t.Fatal("expected error")
	}
	ca, err := GenerateCA("ca", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Key PEM as cert must fail.
	if _, err := ParseCertPEM(ca.KeyPEM); err == nil {
		t.Fatal("expected error")
	}
	_ = x509.Certificate{}
}

func TestIssueNodeCertIsClientAndServerForTheNodeID(t *testing.T) {
	ca, err := GenerateCA("test-ca", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := ca.IssueNodeCert("node-abc", []string{"10.0.0.5", "node1.lab", "node-abc"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ParseCertPEM(issued.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != "node-abc" || len(cert.Subject.OrganizationalUnit) != 1 || cert.Subject.OrganizationalUnit[0] != OUNodes {
		t.Fatalf("subject=%v", cert.Subject)
	}
	if len(cert.DNSNames) != 2 || cert.DNSNames[0] != "node-abc" || cert.DNSNames[1] != "node1.lab" {
		t.Fatalf("dns sans=%v", cert.DNSNames)
	}
	if len(cert.IPAddresses) != 1 || cert.IPAddresses[0].String() != "10.0.0.5" {
		t.Fatalf("ip sans=%v", cert.IPAddresses)
	}
	for _, opts := range []x509.VerifyOptions{
		{Roots: ca.CertPool(), DNSName: "node-abc", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}},
		{Roots: ca.CertPool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}},
	} {
		if _, err := cert.Verify(opts); err != nil {
			t.Fatalf("verify %+v: %v", opts.KeyUsages, err)
		}
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: ca.CertPool(), DNSName: "other-node", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Fatal("certificate must not verify for another node id")
	}
}

func TestIssueControlPlaneClientIsClientOnly(t *testing.T) {
	ca, err := GenerateCA("test-ca", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := ca.IssueControlPlaneClient(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.VerifyClientCert(issued.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != ControlPlaneCN || cert.Subject.OrganizationalUnit[0] != OUControlPlane {
		t.Fatalf("subject=%v", cert.Subject)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: ca.CertPool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Fatal("control-plane client certificate must not be usable as a server certificate")
	}
}

func TestValidNodeID(t *testing.T) {
	for _, id := range []string{"dev-node", "node1.lab", "n_1", "10.0.0.5", "Node-A", "9b2c1f3e-1d2a-4c3b-8e7f-0a1b2c3d4e5f"} {
		if err := ValidNodeID(id); err != nil {
			t.Errorf("ValidNodeID(%q) = %v, want nil", id, err)
		}
	}
	long := make([]byte, 254)
	for i := range long {
		long[i] = 'a'
	}
	for _, id := range []string{"", "-a", "a-", "a..b", ".a", "a.", "a b", "nodé", "a/b", ControlPlaneCN, "ASP-Control-Plane", string(long)} {
		if err := ValidNodeID(id); err == nil {
			t.Errorf("ValidNodeID(%q) = nil, want error", id)
		}
	}
	ca, err := GenerateCA("test-ca", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.IssueNodeCert("a b", nil, time.Hour); !errors.Is(err, ErrInvalidNodeID) {
		t.Fatalf("IssueNodeCert with invalid id: %v", err)
	}
}

func TestEndpointHosts(t *testing.T) {
	got := EndpointHosts("https://node1.lab:9443", "http://0.0.0.0:9100", "http://127.0.0.1:9100", "https://node1.lab:9443/x", "", "::not a url")
	if len(got) != 2 || got[0] != "node1.lab" || got[1] != "127.0.0.1" {
		t.Fatalf("EndpointHosts=%v", got)
	}
}
