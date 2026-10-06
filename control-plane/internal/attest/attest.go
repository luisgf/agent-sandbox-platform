// Package attest provides practical MVP remote attestation (software-signed boot
// statements). Hardware TPM/SEV is a future plug-in via the Attestor interface.
package attest

import (
	"context"
	"crypto"
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
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	AlgES256      = "ES256"
	DefaultMaxAge = 10 * time.Minute
)

// KeyPathFromEnv is where the attestation key lives: ASP_ATTEST_KEY, else a
// lab key in the temporary directory, the same place the node-agent puts its
// own, so a single-host lab shares one key.
func KeyPathFromEnv() string {
	if path := strings.TrimSpace(os.Getenv("ASP_ATTEST_KEY")); path != "" {
		return path
	}
	return filepath.Join(os.TempDir(), "asp-attest-key.pem")
}

// BootStatement is the canonical sandbox boot evidence payload.
type BootStatement struct {
	SandboxID   string `json:"sandbox_id"`
	ImageDigest string `json:"image_digest"`
	VMMProfile  string `json:"vmm_profile"`
	CID         uint32 `json:"cid"`
	NodeID      string `json:"node_id"`
	TS          string `json:"ts"` // RFC3339 UTC
}

// Evidence is a signed attestation bundle stored by the control plane.
type Evidence struct {
	Statement    BootStatement `json:"statement"`
	Signature    string        `json:"signature"` // base64url DER ECDSA
	Alg          string        `json:"alg"`
	KeyID        string        `json:"key_id,omitempty"`
	PublicKeyPEM string        `json:"public_key_pem,omitempty"`
	ReceivedAt   time.Time     `json:"received_at,omitempty"`
}

// Attestor is the plug-in interface. SoftwareAttestor is the MVP; TPM/SEV
// implementations can satisfy the same contract later.
type Attestor interface {
	Name() string
	Attest(ctx context.Context, stmt BootStatement) (Evidence, error)
	Verify(ctx context.Context, ev Evidence) error
}

// SoftwareAttestor signs/verifies boot statements with ECDSA P-256 keys.
//
// It verifies against configured keys only: the public key of ASP_ATTEST_KEY
// (or ASP_ATTEST_PUB, which replaces it), the keys in ASP_ATTEST_TRUSTED_PUBS,
// and, per request, the key of the node certificate the request was
// authenticated with (VerifyWithNodeKey). The public key inside the evidence
// is never trusted: anyone can sign with a fresh key and attach it.
type SoftwareAttestor struct {
	mu      sync.RWMutex
	key     *ecdsa.PrivateKey
	pub     *ecdsa.PublicKey
	kid     string
	trusted []*ecdsa.PublicKey
	MaxAge  time.Duration
}

// LoadOrCreate loads ASP_ATTEST_KEY PEM (or creates a lab key there), then the
// optional ASP_ATTEST_PUB and ASP_ATTEST_TRUSTED_PUBS verification keys.
func LoadOrCreate() (*SoftwareAttestor, error) {
	path := KeyPathFromEnv()
	a := &SoftwareAttestor{MaxAge: DefaultMaxAge}
	if raw := strings.TrimSpace(os.Getenv("ASP_ATTEST_MAX_AGE")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			a.MaxAge = d
		}
	}
	key, err := loadOrCreateKey(path)
	if err != nil {
		return nil, err
	}
	a.key = key
	a.pub = &key.PublicKey
	a.kid = keyID(a.pub)

	// Optional verify-only public key, replacing the signing key's.
	if pubPath := strings.TrimSpace(os.Getenv("ASP_ATTEST_PUB")); pubPath != "" {
		pubPEM, err := os.ReadFile(pubPath)
		if err != nil {
			return nil, fmt.Errorf("read ASP_ATTEST_PUB: %w", err)
		}
		pub, err := parseECPublicKey(pubPEM)
		if err != nil {
			return nil, fmt.Errorf("parse ASP_ATTEST_PUB: %w", err)
		}
		a.pub = pub
		a.kid = keyID(pub)
	}
	if bundle := strings.TrimSpace(os.Getenv("ASP_ATTEST_TRUSTED_PUBS")); bundle != "" {
		keys, err := LoadPublicKeys(bundle)
		if err != nil {
			return nil, fmt.Errorf("ASP_ATTEST_TRUSTED_PUBS: %w", err)
		}
		a.trusted = keys
	}
	return a, nil
}

func loadOrCreateKey(path string) (*ecdsa.PrivateKey, error) {
	if data, err := os.ReadFile(path); err == nil {
		key, err := parseECPrivateKey(data)
		if err != nil {
			return nil, fmt.Errorf("parse ASP_ATTEST_KEY: %w", err)
		}
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	pemBytes, err := marshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// LoadPublicKeys reads a PEM bundle of ECDSA P-256 verification keys: PUBLIC
// KEY blocks and/or CERTIFICATE blocks (their subject key).
func LoadPublicKeys(path string) ([]*ecdsa.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var keys []*ecdsa.PublicKey
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		var k any
		switch block.Type {
		case "PUBLIC KEY":
			k, err = x509.ParsePKIXPublicKey(block.Bytes)
		case "CERTIFICATE":
			var cert *x509.Certificate
			cert, err = x509.ParseCertificate(block.Bytes)
			if err == nil {
				k = cert.PublicKey
			}
		default:
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s block: %w", block.Type, err)
		}
		pub, ok := k.(*ecdsa.PublicKey)
		if !ok || pub.Curve != elliptic.P256() {
			return nil, fmt.Errorf("%s block: not an ECDSA P-256 key", block.Type)
		}
		keys = append(keys, pub)
	}
	if len(keys) == 0 {
		return nil, errors.New("no PUBLIC KEY or CERTIFICATE block")
	}
	return keys, nil
}

// AddTrustedKey adds a verification key (tests and callers that load keys
// themselves).
func (a *SoftwareAttestor) AddTrustedKey(pub *ecdsa.PublicKey) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.trusted = append(a.trusted, pub)
}

// NewSoftwareAttestorFromKey is for tests.
func NewSoftwareAttestorFromKey(key *ecdsa.PrivateKey) *SoftwareAttestor {
	return &SoftwareAttestor{
		key:    key,
		pub:    &key.PublicKey,
		kid:    keyID(&key.PublicKey),
		MaxAge: DefaultMaxAge,
	}
}

func (a *SoftwareAttestor) Name() string { return "software" }

func (a *SoftwareAttestor) Attest(_ context.Context, stmt BootStatement) (Evidence, error) {
	if err := validateStatement(stmt); err != nil {
		return Evidence{}, err
	}
	a.mu.RLock()
	key := a.key
	kid := a.kid
	a.mu.RUnlock()
	if key == nil {
		return Evidence{}, errors.New("attest key not loaded")
	}
	canonical, err := CanonicalJSON(stmt)
	if err != nil {
		return Evidence{}, err
	}
	sum := sha256.Sum256(canonical)
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		return Evidence{}, err
	}
	// Fixed 64-byte r||s for P-256
	sig := append(pad32(r.Bytes()), pad32(s.Bytes())...)
	pubPEM, _ := marshalECPublicKey(&key.PublicKey)
	return Evidence{
		Statement:    stmt,
		Signature:    base64.RawURLEncoding.EncodeToString(sig),
		Alg:          AlgES256,
		KeyID:        kid,
		PublicKeyPEM: string(pubPEM),
	}, nil
}

// Verify checks ev against the configured keys only.
func (a *SoftwareAttestor) Verify(ctx context.Context, ev Evidence) error {
	return a.VerifyWithNodeKey(ctx, ev, nil)
}

// VerifyWithNodeKey is Verify that also accepts a signature by nodeKey: the
// public key of the node certificate the request was authenticated with over
// mTLS. The caller must have checked that the certificate names
// ev.Statement.NodeID.
func (a *SoftwareAttestor) VerifyWithNodeKey(_ context.Context, ev Evidence, nodeKey *ecdsa.PublicKey) error {
	if err := validateStatement(ev.Statement); err != nil {
		return err
	}
	if ev.Alg != "" && ev.Alg != AlgES256 {
		return fmt.Errorf("unsupported alg %q", ev.Alg)
	}
	a.mu.RLock()
	pubs := append([]*ecdsa.PublicKey(nil), a.trusted...)
	if a.pub != nil {
		pubs = append(pubs, a.pub)
	}
	a.mu.RUnlock()
	if nodeKey != nil {
		pubs = append(pubs, nodeKey)
	}
	if len(pubs) == 0 {
		return errors.New("no trusted attestation key: set ASP_ATTEST_KEY, ASP_ATTEST_PUB or ASP_ATTEST_TRUSTED_PUBS")
	}
	sig, err := base64.RawURLEncoding.DecodeString(ev.Signature)
	if err != nil || len(sig) != 64 {
		// also try StdEncoding
		sig, err = base64.StdEncoding.DecodeString(ev.Signature)
		if err != nil || len(sig) != 64 {
			return errors.New("invalid signature encoding")
		}
	}
	canonical, err := CanonicalJSON(ev.Statement)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(canonical)
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	ok := false
	for _, pub := range pubs {
		if ecdsa.Verify(pub, sum[:], r, s) {
			ok = true
			break
		}
	}
	if !ok {
		kid := ev.KeyID
		if len(kid) > 64 {
			kid = kid[:64]
		}
		return fmt.Errorf("attestation signature does not verify with any trusted key (evidence key_id %q): sign with ASP_ATTEST_KEY, a key in ASP_ATTEST_TRUSTED_PUBS, or the node certificate over mTLS", kid)
	}
	return a.checkFreshness(ev.Statement)
}

func (a *SoftwareAttestor) checkFreshness(stmt BootStatement) error {
	ts, err := time.Parse(time.RFC3339, stmt.TS)
	if err != nil {
		return fmt.Errorf("invalid ts: %w", err)
	}
	maxAge := a.MaxAge
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}
	age := time.Since(ts.UTC())
	if age < -time.Minute {
		return errors.New("attestation timestamp in the future")
	}
	if age > maxAge {
		return fmt.Errorf("attestation stale (age %s > %s)", age.Truncate(time.Second), maxAge)
	}
	return nil
}

// IsFresh reports whether stored evidence is still within MaxAge (for OIDC claim).
func (a *SoftwareAttestor) IsFresh(ev Evidence, now time.Time) bool {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	ts, err := time.Parse(time.RFC3339, ev.Statement.TS)
	if err != nil {
		return false
	}
	maxAge := a.MaxAge
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}
	age := now.Sub(ts.UTC())
	return age >= -time.Minute && age <= maxAge
}

// CanonicalJSON produces stable JSON for signing (struct field order).
func CanonicalJSON(stmt BootStatement) ([]byte, error) {
	return json.Marshal(stmt)
}

func validateStatement(stmt BootStatement) error {
	if strings.TrimSpace(stmt.SandboxID) == "" {
		return errors.New("sandbox_id required")
	}
	if strings.TrimSpace(stmt.NodeID) == "" {
		return errors.New("node_id required")
	}
	if strings.TrimSpace(stmt.TS) == "" {
		return errors.New("ts required")
	}
	if _, err := time.Parse(time.RFC3339, stmt.TS); err != nil {
		return fmt.Errorf("ts: %w", err)
	}
	return nil
}

func pad32(b []byte) []byte {
	if len(b) >= 32 {
		return b[len(b)-32:]
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func keyID(pub *ecdsa.PublicKey) string {
	b, _ := x509.MarshalPKIXPublicKey(pub)
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(sum[:8])
}

func parseECPrivateKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an ECDSA private key")
	}
	return ek, nil
}

func parseECPublicKey(pemBytes []byte) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("not an ECDSA public key")
	}
	return pub, nil
}

func marshalECPrivateKey(key *ecdsa.PrivateKey) ([]byte, error) {
	b, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: b}), nil
}

func marshalECPublicKey(pub *ecdsa.PublicKey) ([]byte, error) {
	b, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: b}), nil
}

// PublicKeyPEM returns the verifier public key PEM.
func (a *SoftwareAttestor) PublicKeyPEM() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.pub == nil {
		return ""
	}
	b, _ := marshalECPublicKey(a.pub)
	return string(b)
}

func (a *SoftwareAttestor) KID() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.kid
}

// Ensure crypto.Hash is referenced for future SHA use.
var _ = crypto.SHA256
