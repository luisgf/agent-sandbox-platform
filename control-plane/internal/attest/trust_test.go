package attest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func signedBy(t *testing.T, key *ecdsa.PrivateKey) Evidence {
	t.Helper()
	ev, err := NewSoftwareAttestorFromKey(key).Attest(context.Background(), BootStatement{
		SandboxID: "sb-1", NodeID: "node-a", ImageDigest: "sha256:abc", VMMProfile: "cloud-hypervisor", CID: 3,
		TS: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// Anyone can sign with a fresh key and attach it: that key proves nothing.
func TestVerifyIgnoresTheKeyInTheEvidence(t *testing.T) {
	a := NewSoftwareAttestorFromKey(newKey(t))
	forged := signedBy(t, newKey(t))
	if forged.PublicKeyPEM == "" {
		t.Fatal("the forged bundle should carry its key")
	}
	err := a.Verify(context.Background(), forged)
	if err == nil {
		t.Fatal("evidence signed by an unknown key verified with its own public_key_pem")
	}
	if !strings.Contains(err.Error(), forged.KeyID) {
		t.Fatalf("the error should name the evidence key id: %v", err)
	}
}

func TestVerifyWithTrustedKeysAndNodeKey(t *testing.T) {
	trustedPub, trustedCert, node := newKey(t), newKey(t), newKey(t)
	dir := t.TempDir()
	der, err := x509.MarshalPKIXPublicKey(&trustedPub.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "node-b"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &trustedCert.PublicKey, trustedCert)
	if err != nil {
		t.Fatal(err)
	}
	bundle := append(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})...)
	path := filepath.Join(dir, "trusted.pem")
	if err := os.WriteFile(path, bundle, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASP_ATTEST_KEY", filepath.Join(dir, "attest.pem"))
	t.Setenv("ASP_ATTEST_TRUSTED_PUBS", path)
	a, err := LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, key := range map[string]*ecdsa.PrivateKey{"public key block": trustedPub, "certificate block": trustedCert} {
		if err := a.Verify(ctx, signedBy(t, key)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	ev := signedBy(t, node)
	if err := a.Verify(ctx, ev); err == nil {
		t.Fatal("a node key the control plane was not given must not verify")
	}
	if err := a.VerifyWithNodeKey(ctx, ev, &node.PublicKey); err != nil {
		t.Fatalf("signed by the key of the node's certificate: %v", err)
	}
}

func TestLoadPublicKeysRejectsBadBundles(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.pem")
	_ = os.WriteFile(empty, []byte("not pem"), 0o644)
	if _, err := LoadPublicKeys(empty); err == nil {
		t.Fatal("a file without keys must be refused")
	}
	if _, err := LoadPublicKeys(filepath.Join(dir, "missing.pem")); err == nil {
		t.Fatal("a missing file must be refused")
	}
}

// ASP_ATTEST_PUB used to apply only when the signing key was created, not
// when it already existed.
func TestAttestPubAppliesToAnExistingKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "attest.pem")
	pemBytes, err := marshalECPrivateKey(newKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	verifier := newKey(t)
	pubPEM, err := marshalECPublicKey(&verifier.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pubPath := filepath.Join(dir, "attest.pub")
	if err := os.WriteFile(pubPath, pubPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASP_ATTEST_KEY", keyPath)
	t.Setenv("ASP_ATTEST_PUB", pubPath)
	a, err := LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Verify(context.Background(), signedBy(t, verifier)); err != nil {
		t.Fatalf("ASP_ATTEST_PUB ignored for an existing key: %v", err)
	}
}
