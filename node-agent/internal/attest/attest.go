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

// BootStatement mirrors control-plane attest.BootStatement.
type BootStatement struct {
	SandboxID   string `json:"sandbox_id"`
	ImageDigest string `json:"image_digest"`
	VMMProfile  string `json:"vmm_profile"`
	CID         uint32 `json:"cid"`
	NodeID      string `json:"node_id"`
	TS          string `json:"ts"`
}

// Evidence is the signed bundle POSTed to the control plane.
type Evidence struct {
	Statement    BootStatement `json:"statement"`
	Signature    string        `json:"signature"`
	Alg          string        `json:"alg"`
	KeyID        string        `json:"key_id,omitempty"`
	PublicKeyPEM string        `json:"public_key_pem,omitempty"`
}

// Signer signs boot statements with ASP_ATTEST_KEY (ECDSA P-256).
type Signer struct {
	key *ecdsa.PrivateKey
	kid string
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
		return &Signer{key: key, kid: kidOf(&key.PublicKey)}, nil
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
	return &Signer{key: key, kid: kidOf(&key.PublicKey)}, nil
}

func (s *Signer) Sign(stmt BootStatement) (Evidence, error) {
	if stmt.TS == "" {
		stmt.TS = time.Now().UTC().Format(time.RFC3339)
	}
	canonical, err := json.Marshal(stmt)
	if err != nil {
		return Evidence{}, err
	}
	sum := sha256.Sum256(canonical)
	r, ss, err := ecdsa.Sign(rand.Reader, s.key, sum[:])
	if err != nil {
		return Evidence{}, err
	}
	sig := append(pad32(r.Bytes()), pad32(ss.Bytes())...)
	pubDER, _ := x509.MarshalPKIXPublicKey(&s.key.PublicKey)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	return Evidence{
		Statement:    stmt,
		Signature:    base64.RawURLEncoding.EncodeToString(sig),
		Alg:          "ES256",
		KeyID:        s.kid,
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

// SignNow is a convenience used by the reconciler.
func SignNow(sandboxID, nodeID, imageDigest, vmmProfile string, cid uint32) (Evidence, error) {
	s, err := LoadOrCreate()
	if err != nil {
		return Evidence{}, err
	}
	return s.Sign(BootStatement{
		SandboxID:   sandboxID,
		ImageDigest: imageDigest,
		VMMProfile:  vmmProfile,
		CID:         cid,
		NodeID:      nodeID,
		TS:          time.Now().UTC().Format(time.RFC3339),
	})
}
