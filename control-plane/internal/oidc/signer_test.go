package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestMintAndVerifyAgainstJWKS(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSignerFromKey(key, "http://issuer.test")
	token, claims, err := s.Mint("tenant-a", "sb-1", "https://api.example.com", "n1")
	if err != nil {
		t.Fatal(err)
	}
	if claims.Subject != "sandbox/sb-1" || claims.TenantID != "tenant-a" || claims.Audience != "https://api.example.com" {
		t.Fatalf("claims=%+v", claims)
	}
	jwks, err := s.JWKS()
	if err != nil {
		t.Fatal(err)
	}
	got, err := VerifyAgainstJWKS(token, jwks)
	if err != nil {
		t.Fatal(err)
	}
	if got.SandboxID != "sb-1" || got.Nonce != "n1" {
		t.Fatalf("verified=%+v", got)
	}
	if _, err := s.VerifyRS256(token); err != nil {
		t.Fatal(err)
	}
}

func TestMintRequiresAud(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSignerFromKey(key, "http://issuer.test")
	if _, _, err := s.Mint("t", "s", "", ""); err == nil {
		t.Fatal("expected error")
	}
}

func TestJWKSOneOrTwoKeys(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSignerFromKey(key, "http://issuer.test")
	jwks, err := s.JWKS()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Keys []map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(jwks, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Keys) != 1 {
		t.Fatalf("want 1 key, got %d", len(doc.Keys))
	}
	if doc.Keys[0]["kid"] != s.KID() {
		t.Fatalf("kid mismatch")
	}

	prev, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s.WithPreviousKey(prev)
	jwks, err = s.JWKS()
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(jwks, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Keys) != 2 {
		t.Fatalf("want 2 keys during rotation, got %d", len(doc.Keys))
	}
	// Mint still uses current kid
	token, _, err := s.Mint("t", "s", "aud", "")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var header map[string]string
	_ = json.Unmarshal(hb, &header)
	if header["kid"] != s.KID() {
		t.Fatalf("mint kid=%s want %s", header["kid"], s.KID())
	}
	if _, err := VerifyAgainstJWKS(token, jwks); err != nil {
		t.Fatal(err)
	}
}
