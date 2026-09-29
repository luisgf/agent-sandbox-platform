package pki

import (
	"crypto/x509"
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
