package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestTLSCertNames(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "cp.example.com"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"cp.example.com", "api.example.com"}, IPAddresses: []net.IP{net.ParseIP("203.0.113.9")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tls.crt")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	names, err := tlsCertNames(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cp.example.com", "api.example.com", "203.0.113.9"} {
		if !slices.Contains(names, want) {
			t.Errorf("%q missing from %v", want, names)
		}
	}
	if _, err := tlsCertNames(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing file was accepted")
	}
	junk := filepath.Join(t.TempDir(), "junk")
	if err := os.WriteFile(junk, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tlsCertNames(junk); err == nil {
		t.Error("a file with no certificate was accepted")
	}
}
