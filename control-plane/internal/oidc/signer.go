// Package oidc mints short-lived attested sandbox tokens and serves JWKS.
package oidc

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
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
	DefaultIssuer = "http://127.0.0.1:8080"
	DefaultTTL    = 5 * time.Minute
)

// Signer holds the RSA private key used to mint sandbox OIDC tokens.
// Optional previous key (ASP_OIDC_KEY_PREV) remains in JWKS during rotation.
type Signer struct {
	mu       sync.RWMutex
	key      *rsa.PrivateKey
	kid      string
	prevKey  *rsa.PrivateKey // optional; verify-only during rotation
	prevKID  string
	Issuer   string
	TTL      time.Duration
	KeyPath  string
	PrevPath string
}

// TokenRequest is the mint payload from a node (sandbox_id required; tenant from store).
type TokenRequest struct {
	SandboxID string `json:"sandbox_id"`
	Audience  string `json:"aud"`
	Nonce     string `json:"nonce,omitempty"`
}

// Claims embedded in the JWT (subset exposed for tests).
type Claims struct {
	Issuer          string         `json:"iss"`
	Subject         string         `json:"sub"`
	Audience        string         `json:"aud"`
	Expiry          int64          `json:"exp"`
	IssuedAt        int64          `json:"iat"`
	JTI             string         `json:"jti"`
	TenantID        string         `json:"tenant_id"`
	SandboxID       string         `json:"sandbox_id"`
	Nonce           string         `json:"nonce,omitempty"`
	XAspAttestation map[string]any `json:"x_asp_attestation,omitempty"`
}

// LoadOrCreate loads PEM from ASP_OIDC_KEY (path) or creates a 2048-bit RSA key.
func LoadOrCreate(issuer string) (*Signer, error) {
	if issuer == "" {
		issuer = strings.TrimSpace(os.Getenv("ASP_OIDC_ISSUER"))
	}
	if issuer == "" {
		issuer = DefaultIssuer
	}
	s := &Signer{Issuer: strings.TrimRight(issuer, "/"), TTL: DefaultTTL}

	path := strings.TrimSpace(os.Getenv("ASP_OIDC_KEY"))
	if path == "" {
		path = filepath.Join(os.TempDir(), "asp-oidc-key.pem")
	}
	s.KeyPath = path

	if data, err := os.ReadFile(path); err == nil {
		key, err := parseRSAPrivateKey(data)
		if err != nil {
			return nil, fmt.Errorf("parse ASP_OIDC_KEY: %w", err)
		}
		s.key = key
		s.kid = keyID(key)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, err
		}
		pemBytes, err := marshalRSAPrivateKey(key)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
			return nil, err
		}
		s.key = key
		s.kid = keyID(key)
	}

	prevPath := strings.TrimSpace(os.Getenv("ASP_OIDC_KEY_PREV"))
	s.PrevPath = prevPath
	if prevPath != "" {
		data, err := os.ReadFile(prevPath)
		if err != nil {
			return nil, fmt.Errorf("read ASP_OIDC_KEY_PREV: %w", err)
		}
		pkey, err := parseRSAPrivateKey(data)
		if err != nil {
			return nil, fmt.Errorf("parse ASP_OIDC_KEY_PREV: %w", err)
		}
		s.prevKey = pkey
		s.prevKID = keyID(pkey)
	}
	return s, nil
}

// NewSignerFromKey is for tests.
func NewSignerFromKey(key *rsa.PrivateKey, issuer string) *Signer {
	if issuer == "" {
		issuer = DefaultIssuer
	}
	return &Signer{
		key:    key,
		kid:    keyID(key),
		Issuer: strings.TrimRight(issuer, "/"),
		TTL:    DefaultTTL,
	}
}

// WithPreviousKey attaches a previous signing key for JWKS overlap (tests / rotation).
func (s *Signer) WithPreviousKey(prev *rsa.PrivateKey) *Signer {
	if s == nil || prev == nil {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prevKey = prev
	s.prevKID = keyID(prev)
	return s
}

func (s *Signer) KID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.kid
}

func (s *Signer) PublicKey() *rsa.PublicKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return &s.key.PublicKey
}

// Mint creates a signed JWT. sub = "sandbox/{sandboxID}"; tenant/sandbox claims are authoritative.
//
// ADR-0007 phase 5 (not yet): accept owner_sub and emit user_sub / act claims so
// workload tokens carry the human chain. Guest must never supply user_sub.
func (s *Signer) Mint(tenantID, sandboxID, aud, nonce string) (string, Claims, error) {
	if strings.TrimSpace(sandboxID) == "" {
		return "", Claims{}, errors.New("sandbox_id required")
	}
	if strings.TrimSpace(aud) == "" {
		return "", Claims{}, errors.New("aud required")
	}
	if strings.TrimSpace(tenantID) == "" {
		return "", Claims{}, errors.New("tenant_id required")
	}
	now := time.Now().UTC()
	ttl := s.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	claims := Claims{
		Issuer:    s.Issuer,
		Subject:   "sandbox/" + sandboxID,
		Audience:  aud,
		Expiry:    now.Add(ttl).Unix(),
		IssuedAt:  now.Unix(),
		JTI:       newJTI(),
		TenantID:  tenantID,
		SandboxID: sandboxID,
		Nonce:     nonce,
	}
	token, err := s.sign(claims)
	return token, claims, err
}

// MintWithAttestation is Mint plus optional x_asp_attestation claim when evidence is fresh.
func (s *Signer) MintWithAttestation(tenantID, sandboxID, aud, nonce string, attestation map[string]any) (string, Claims, error) {
	token, claims, err := s.Mint(tenantID, sandboxID, aud, nonce)
	if err != nil {
		return "", Claims{}, err
	}
	if len(attestation) == 0 {
		return token, claims, nil
	}
	claims.XAspAttestation = attestation
	token, err = s.sign(claims)
	return token, claims, err
}

func (s *Signer) sign(claims Claims) (string, error) {
	s.mu.RLock()
	key := s.key
	kid := s.kid
	s.mu.RUnlock()

	header := map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid}
	hb, _ := json.Marshal(header)
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	unsigned := b64url(hb) + "." + b64url(cb)
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + b64url(sig), nil
}

// JWKS returns a JSON Web Key Set for the current (+ optional previous) public keys.
func (s *Signer) JWKS() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := []map[string]string{jwkEntry(&s.key.PublicKey, s.kid)}
	if s.prevKey != nil && s.prevKID != "" && s.prevKID != s.kid {
		keys = append(keys, jwkEntry(&s.prevKey.PublicKey, s.prevKID))
	}
	doc := map[string]any{"keys": keys}
	return json.Marshal(doc)
}

func jwkEntry(pub *rsa.PublicKey, kid string) map[string]string {
	return map[string]string{
		"kty": "RSA",
		"use": "sig",
		"alg": "RS256",
		"kid": kid,
		"n":   b64url(pub.N.Bytes()),
		"e":   b64url(big.NewInt(int64(pub.E)).Bytes()),
	}
}

// OpenIDConfiguration returns the discovery document.
func (s *Signer) OpenIDConfiguration() ([]byte, error) {
	doc := map[string]any{
		"issuer":                                s.Issuer,
		"jwks_uri":                              s.Issuer + "/oidc/jwks.json",
		"token_endpoint":                        s.Issuer + "/v1/internal/oidc/token",
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"response_types_supported":              []string{"id_token"},
		"subject_types_supported":               []string{"public"},
	}
	return json.Marshal(doc)
}

// VerifyRS256 verifies a token minted by this signer (tests / local check).
func (s *Signer) VerifyRS256(token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("malformed jwt")
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, err
	}
	var header map[string]string
	if err := json.Unmarshal(hb, &header); err != nil {
		return Claims{}, err
	}
	if header["alg"] != "RS256" {
		return Claims{}, fmt.Errorf("unexpected alg %q", header["alg"])
	}
	cb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, err
	}
	unsigned := parts[0] + "." + parts[1]
	sum := sha256.Sum256([]byte(unsigned))
	s.mu.RLock()
	pubs := []*rsa.PublicKey{&s.key.PublicKey}
	if s.prevKey != nil {
		pubs = append(pubs, &s.prevKey.PublicKey)
	}
	s.mu.RUnlock()
	var verr error
	for _, pub := range pubs {
		verr = rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig)
		if verr == nil {
			break
		}
	}
	if verr != nil {
		return Claims{}, verr
	}
	var claims Claims
	if err := json.Unmarshal(cb, &claims); err != nil {
		return Claims{}, err
	}
	if claims.Expiry > 0 && time.Now().UTC().Unix() > claims.Expiry {
		return Claims{}, errors.New("token expired")
	}
	return claims, nil
}

// VerifyAgainstJWKS verifies token using a JWKS document (tests).
func VerifyAgainstJWKS(token string, jwksJSON []byte) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("malformed jwt")
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, err
	}
	var header map[string]string
	if err := json.Unmarshal(hb, &header); err != nil {
		return Claims{}, err
	}
	var jwks struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
			Alg string `json:"alg"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(jwksJSON, &jwks); err != nil {
		return Claims{}, err
	}
	var pub *rsa.PublicKey
	for _, k := range jwks.Keys {
		if k.Kid != "" && header["kid"] != "" && k.Kid != header["kid"] {
			continue
		}
		nb, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eb, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		pub = &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(new(big.Int).SetBytes(eb).Int64())}
		break
	}
	if pub == nil {
		return Claims{}, errors.New("no matching jwk")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return Claims{}, err
	}
	cb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, err
	}
	var claims Claims
	if err := json.Unmarshal(cb, &claims); err != nil {
		return Claims{}, err
	}
	return claims, nil
}

func b64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func keyID(key *rsa.PrivateKey) string {
	sum := sha256.Sum256(x509.MarshalPKCS1PublicKey(&key.PublicKey))
	return base64.RawURLEncoding.EncodeToString(sum[:8])
}

func newJTI() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x", b[:])
}

func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA private key")
	}
	return rk, nil
}

func marshalRSAPrivateKey(key *rsa.PrivateKey) ([]byte, error) {
	b := x509.MarshalPKCS1PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: b}), nil
}
