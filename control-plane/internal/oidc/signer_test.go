package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"reflect"
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

func TestMintIdentityUserSubAndAct(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSignerFromKey(key, "http://issuer.test")

	token, claims, err := s.MintIdentity("tenant-a", "sb-1", "https://api.example.com", "n1", "user:alice", "")
	if err != nil {
		t.Fatal(err)
	}
	if claims.UserSub != "user:alice" {
		t.Fatalf("user_sub=%q", claims.UserSub)
	}
	if claims.Act == nil || claims.Act.Sub != "user:alice" {
		t.Fatalf("act=%v", claims.Act)
	}
	got, err := s.VerifyRS256(token)
	if err != nil {
		t.Fatal(err)
	}
	if got.UserSub != "user:alice" || got.Act == nil || got.Act.Sub != "user:alice" {
		t.Fatalf("verified=%+v", got)
	}
}

func TestMintIdentityActorSubOverridesAct(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSignerFromKey(key, "http://issuer.test")
	_, claims, err := s.MintIdentity("t", "s", "aud", "", "user:owner", "user:actor")
	if err != nil {
		t.Fatal(err)
	}
	if claims.UserSub != "user:owner" {
		t.Fatalf("user_sub=%q", claims.UserSub)
	}
	if claims.Act == nil || claims.Act.Sub != "user:actor" {
		t.Fatalf("act=%v want actor", claims.Act)
	}
}

func TestMintWithoutOwnerOmitsUserSub(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSignerFromKey(key, "http://issuer.test")
	token, claims, err := s.Mint("t", "s", "aud", "")
	if err != nil {
		t.Fatal(err)
	}
	if claims.UserSub != "" || claims.Act != nil {
		t.Fatalf("lab mint must omit human claims: %+v", claims)
	}
	// Ensure JSON payload does not include empty user_sub/act keys.
	parts := strings.Split(token, ".")
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var raw map[string]any
	if err := json.Unmarshal(cb, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["user_sub"]; ok {
		t.Fatalf("unexpected user_sub in payload: %v", raw)
	}
	if _, ok := raw["act"]; ok {
		t.Fatalf("unexpected act in payload: %v", raw)
	}
}

func TestMintWithAttestationCarriesUserSub(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSignerFromKey(key, "http://issuer.test")
	att := map[string]any{"fresh": true}
	token, claims, err := s.MintWithAttestation("t", "s", "aud", "", "user:bob", att)
	if err != nil {
		t.Fatal(err)
	}
	if claims.UserSub != "user:bob" || claims.Act == nil || claims.Act.Sub != "user:bob" {
		t.Fatalf("claims=%+v", claims)
	}
	if claims.XAspAttestation == nil {
		t.Fatal("missing attestation")
	}
	got, err := s.VerifyRS256(token)
	if err != nil {
		t.Fatal(err)
	}
	if got.UserSub != "user:bob" {
		t.Fatalf("verified user_sub=%q", got.UserSub)
	}
}

// Minting with attestation signs once, with the attestation in the claims.
func TestMintWithAttestationSignsOnce(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSignerFromKey(key, "")
	signs := 0
	signHook = func() { signs++ }
	defer func() { signHook = nil }()
	tok, claims, err := s.MintWithAttestation("t1", "sb1", "aud", "", "alice", map[string]any{"node_id": "n1"})
	if err != nil {
		t.Fatal(err)
	}
	if signs != 1 {
		t.Fatalf("MintWithAttestation signed %d times, want 1", signs)
	}
	got, err := s.VerifyRS256(tok)
	if err != nil || got.XAspAttestation["node_id"] != "n1" || claims.UserSub != "alice" {
		t.Fatalf("token claims %+v err=%v", got, err)
	}
}

// ADR-0020: groups travel with the owner, verbatim, and only when there are any.
func TestMintForCarriesOwnerGroups(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSignerFromKey(key, "http://issuer.test")
	id := Identity{UserSub: "user:alice", Groups: []string{"asp-operator", "SRE", " db-admins "}}
	token, claims, err := s.MintFor("tenant-a", "sb-1", "https://api.example.com", "", id, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"asp-operator", "SRE", "db-admins"}
	if !reflect.DeepEqual(claims.Groups, want) {
		t.Fatalf("groups=%v want %v", claims.Groups, want)
	}
	got, err := s.VerifyRS256(token)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Groups, want) || got.UserSub != "user:alice" {
		t.Fatalf("jwt groups=%v user_sub=%q", got.Groups, got.UserSub)
	}
	// The wire form is a JSON array under "groups": what a consumer's groups_claim reads.
	payload, _ := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}
	if arr, ok := raw["groups"].([]any); !ok || len(arr) != 3 || arr[1] != "SRE" {
		t.Fatalf("wire groups=%v", raw["groups"])
	}
}

func TestMintForOmitsGroupsWithoutOwnerOrWithoutGroups(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSignerFromKey(key, "http://issuer.test")
	cases := map[string]Identity{
		"no owner, groups given anyway": {Groups: []string{"sre"}},
		"owner, no groups":              {UserSub: "user:alice"},
		"owner, only blank groups":      {UserSub: "user:alice", Groups: []string{"", "  "}},
	}
	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			token, claims, err := s.MintFor("tenant-a", "sb-1", "https://api.example.com", "", id, nil)
			if err != nil {
				t.Fatal(err)
			}
			if claims.Groups != nil {
				t.Fatalf("groups=%v want none", claims.Groups)
			}
			payload, _ := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
			var raw map[string]any
			_ = json.Unmarshal(payload, &raw)
			if _, present := raw["groups"]; present {
				t.Fatalf("the claim must be absent, not empty: %s", payload)
			}
			if id.UserSub == "" {
				if _, present := raw["user_sub"]; present {
					t.Fatalf("no owner, no user_sub: %s", payload)
				}
			}
		})
	}
}
