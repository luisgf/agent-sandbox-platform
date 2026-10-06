package attest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
)

// VerifyES256 checks ev's signature with pub, as the control plane does.
func verifyES256(t *testing.T, ev Evidence, pub *ecdsa.PublicKey) bool {
	t.Helper()
	sig, err := base64.RawURLEncoding.DecodeString(ev.Signature)
	if err != nil || len(sig) != 64 {
		t.Fatalf("signature encoding: %v", err)
	}
	canonical, _ := json.Marshal(ev.Statement)
	sum := sha256.Sum256(canonical)
	return ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
}

func TestLoadKeyFileSignsWithTheNodeKey(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for name, encode := range map[string]func() []byte{
		"sec1": func() []byte {
			der, _ := x509.MarshalECPrivateKey(key)
			return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
		},
		"pkcs8": func() []byte {
			der, _ := x509.MarshalPKCS8PrivateKey(key)
			return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		},
	} {
		path := filepath.Join(t.TempDir(), "client.key")
		if err := os.WriteFile(path, encode(), 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := LoadKeyFile(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		ev, err := s.SignBoot("sb-1", "node-a", "sha256:abc", "cloud-hypervisor", 3)
		if err != nil {
			t.Fatal(err)
		}
		if ev.KeyID != s.KeyID() || ev.Statement.TS == "" || ev.Statement.NodeID != "node-a" {
			t.Fatalf("%s: evidence %+v", name, ev)
		}
		if !verifyES256(t, ev, &key.PublicKey) {
			t.Fatalf("%s: signature does not verify with the node key", name)
		}
	}
	if _, err := LoadKeyFile(filepath.Join(t.TempDir(), "missing.key")); err == nil {
		t.Fatal("a missing key file must fail")
	}
}
