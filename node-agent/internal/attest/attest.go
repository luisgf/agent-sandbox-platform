// Package attest signs sandbox boot statements for control-plane remote attestation.
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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BootStatement mirrors control-plane attest.BootStatement. The order of the
// fields is the order they are signed in: both sides must keep it.
type BootStatement struct {
	SandboxID string `json:"sandbox_id"`
	// ImageDigest is "sha256:<hex>" of the base image the sandbox's disk was
	// copied from, as hashed on the node. Empty when it was not measured.
	ImageDigest string `json:"image_digest"`
	VMMProfile  string `json:"vmm_profile"`
	CID         uint32 `json:"cid"`
	NodeID      string `json:"node_id"`
	TS          string `json:"ts"`
	// KernelDigest is "sha256:<hex>" of the kernel the VM loaded.
	KernelDigest string `json:"kernel_digest,omitempty"`
	// VMMVersion is what the hypervisor binary reports as its version.
	VMMVersion string `json:"vmm_version,omitempty"`
	// Boot is "new" (a disk copied from the base image for this boot) or
	// "resume" (a retained disk, which has diverged from the base image it was
	// copied from).
	Boot string `json:"boot,omitempty"`
}

// Measurement is what a node measured of the boot a statement is about. A field
// that was not measured is empty.
type Measurement struct {
	ImageDigest  string
	KernelDigest string
	VMMVersion   string
	Boot         string
}

// Evidence is the signed bundle POSTed to the control plane.
type Evidence struct {
	Statement    BootStatement `json:"statement"`
	Signature    string        `json:"signature"`
	Alg          string        `json:"alg"`
	KeyID        string        `json:"key_id,omitempty"`
	PublicKeyPEM string        `json:"public_key_pem,omitempty"`
}

// Signer signs boot statements with an ECDSA P-256 key: the node
// certificate's key over mTLS (the control plane verifies it with the
// certificate the request comes with), else ASP_ATTEST_KEY, whose public key
// the control plane must trust.
type Signer struct {
	key func() *ecdsa.PrivateKey
}

// NewSigner signs with key.
func NewSigner(key *ecdsa.PrivateKey) *Signer {
	return &Signer{key: func() *ecdsa.PrivateKey { return key }}
}

// NewKeySigner signs with the key that key returns at signing time, such as
// the node certificate's key, which renewal replaces.
func NewKeySigner(key func() *ecdsa.PrivateKey) *Signer {
	return &Signer{key: key}
}

// LoadKeyFile reads an ECDSA private key PEM (SEC 1 or PKCS #8), such as the
// node certificate key in cert-dir/client.key.
func LoadKeyFile(path string) (*Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := parseKey(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return NewSigner(key), nil
}

// KeyID identifies the signing key (truncated SHA-256 of its SPKI).
func (s *Signer) KeyID() string {
	if k := s.key(); k != nil {
		return kidOf(&k.PublicKey)
	}
	return ""
}

func LoadOrCreate() (*Signer, error) {
	path := strings.TrimSpace(os.Getenv("ASP_ATTEST_KEY"))
	if path == "" {
		path = filepath.Join(os.TempDir(), "asp-attest-key.pem")
	}
	if data, err := os.ReadFile(path); err == nil {
		key, err := parseKey(data)
		if err != nil {
			return nil, err
		}
		return NewSigner(key), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	b, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: b})
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		return nil, err
	}
	return NewSigner(key), nil
}

func (s *Signer) Sign(stmt BootStatement) (Evidence, error) {
	if stmt.TS == "" {
		stmt.TS = time.Now().UTC().Format(time.RFC3339)
	}
	key := s.key()
	if key == nil {
		return Evidence{}, errors.New("no ECDSA signing key")
	}
	canonical, err := json.Marshal(stmt)
	if err != nil {
		return Evidence{}, err
	}
	sum := sha256.Sum256(canonical)
	r, ss, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		return Evidence{}, err
	}
	sig := append(pad32(r.Bytes()), pad32(ss.Bytes())...)
	pubDER, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	return Evidence{
		Statement:    stmt,
		Signature:    base64.RawURLEncoding.EncodeToString(sig),
		Alg:          "ES256",
		KeyID:        kidOf(&key.PublicKey),
		PublicKeyPEM: string(pubPEM),
	}, nil
}

func pad32(b []byte) []byte {
	if len(b) >= 32 {
		return b[len(b)-32:]
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func kidOf(pub *ecdsa.PublicKey) string {
	b, _ := x509.MarshalPKIXPublicKey(pub)
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(sum[:8])
}

func parseKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM")
	}
	if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not ecdsa")
	}
	return ek, nil
}

// SignNow signs with ASP_ATTEST_KEY; the reconciler uses it without a Signer.
func SignNow(sandboxID, nodeID, vmmProfile string, cid uint32, m Measurement) (Evidence, error) {
	s, err := LoadOrCreate()
	if err != nil {
		return Evidence{}, err
	}
	return s.SignBoot(sandboxID, nodeID, vmmProfile, cid, m)
}

// SignBoot signs a boot statement stamped now.
func (s *Signer) SignBoot(sandboxID, nodeID, vmmProfile string, cid uint32, m Measurement) (Evidence, error) {
	return s.Sign(BootStatement{
		SandboxID:    sandboxID,
		ImageDigest:  m.ImageDigest,
		VMMProfile:   vmmProfile,
		CID:          cid,
		NodeID:       nodeID,
		TS:           time.Now().UTC().Format(time.RFC3339),
		KernelDigest: m.KernelDigest,
		VMMVersion:   m.VMMVersion,
		Boot:         m.Boot,
	})
}
