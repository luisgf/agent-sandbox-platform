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
	"strings"
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
		ev, err := s.SignBoot("sb-1", "node-a", "cloud-hypervisor", 3, Measurement{ImageDigest: "sha256:abc"})
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

// The measurements are part of what is signed, and a statement without them
// signs exactly what it always did.
func TestMeasurementsAreSigned(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSigner(key)
	ev, err := s.SignBoot("sb-1", "node-a", "cloud-hypervisor", 3, Measurement{
		ImageDigest: "sha256:" + strings.Repeat("a", 64), KernelDigest: "sha256:" + strings.Repeat("b", 64),
		VMMVersion: "cloud-hypervisor v43.0", Boot: "resume",
	})
	if err != nil {
		t.Fatal(err)
	}
	st := ev.Statement
	if st.ImageDigest != "sha256:"+strings.Repeat("a", 64) || st.KernelDigest != "sha256:"+strings.Repeat("b", 64) ||
		st.VMMVersion != "cloud-hypervisor v43.0" || st.Boot != "resume" {
		t.Fatalf("statement %+v", st)
	}
	if !verifyES256(t, ev, &key.PublicKey) {
		t.Fatal("a statement with measurements does not verify")
	}
	// Changing a measurement after signing breaks the signature.
	ev.Statement.KernelDigest = "sha256:" + strings.Repeat("c", 64)
	if verifyES256(t, ev, &key.PublicKey) {
		t.Fatal("the kernel digest is not covered by the signature")
	}

	bare, err := s.SignBoot("sb-2", "node-a", "cloud-hypervisor", 4, Measurement{})
	if err != nil {
		t.Fatal(err)
	}
	canonical, _ := json.Marshal(bare.Statement)
	for _, field := range []string{"kernel_digest", "vmm_version", "boot"} {
		if strings.Contains(string(canonical), field) {
			t.Errorf("an unmeasured statement carries %s: %s", field, canonical)
		}
	}
}
