package idp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func mustRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// signRawToken signs exactly the claims given, so a test can omit exp.
func signRawToken(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	hb, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid})
	cb, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	unsigned := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestValidateTimeClaims(t *testing.T) {
	key := mustRSAKey(t)
	const iss, kid = "https://idp.example.test", "k"
	keys := map[string]*rsa.PublicKey{kid: &key.PublicKey}
	strict, err := NewValidatorWithPublicKeys(Config{Issuer: iss}, keys)
	if err != nil {
		t.Fatal(err)
	}
	lax, err := NewValidatorWithPublicKeys(Config{Issuer: iss, AllowMissingExp: true}, keys)
	if err != nil {
		t.Fatal(err)
	}

	noExp := signRawToken(t, key, kid, map[string]any{"iss": iss, "sub": "alice"})
	if _, err := strict.Validate(noExp); err == nil {
		t.Fatal("a token without exp must be rejected by default")
	}
	if _, err := lax.Validate(noExp); err != nil {
		t.Fatalf("AllowMissingExp: %v", err)
	}

	now := time.Now().Unix()
	for _, tc := range []struct {
		name   string
		claims map[string]any
		ok     bool
	}{
		{"valid", map[string]any{"exp": now + 3600, "iat": now}, true},
		{"expired", map[string]any{"exp": now - 10}, false},
		{"nbf within clock skew", map[string]any{"exp": now + 3600, "nbf": now + 30}, true},
		{"nbf ahead", map[string]any{"exp": now + 3600, "nbf": now + 600}, false},
		{"iat far in the future", map[string]any{"exp": now + 7200, "iat": now + 3600}, false},
	} {
		tc.claims["iss"] = iss
		tc.claims["sub"] = "alice"
		_, err := strict.Validate(signRawToken(t, key, kid, tc.claims))
		if (err == nil) != tc.ok {
			t.Errorf("%s: ok=%v, err=%v", tc.name, tc.ok, err)
		}
	}
}

func TestConfigFromEnvRequireExp(t *testing.T) {
	if ConfigFromEnv().AllowMissingExp {
		t.Fatal("exp must be required unless ASP_IDP_REQUIRE_EXP=0")
	}
	t.Setenv("ASP_IDP_REQUIRE_EXP", "0")
	if !ConfigFromEnv().AllowMissingExp {
		t.Fatal("ASP_IDP_REQUIRE_EXP=0 must allow tokens without exp")
	}
}

type jwksServer struct {
	mu      sync.Mutex
	fetches int
	keys    map[string]*rsa.PublicKey
	srv     *httptest.Server
}

func newJWKSServer(t *testing.T, keys map[string]*rsa.PublicKey) *jwksServer {
	t.Helper()
	s := &jwksServer{keys: keys}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.fetches++
		doc, err := JWKSJSON(s.keys)
		s.mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *jwksServer) setKeys(keys map[string]*rsa.PublicKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = keys
}

func (s *jwksServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetches
}

func remoteValidator(t *testing.T, js *jwksServer, iss string) *Validator {
	t.Helper()
	v, err := NewValidator(Config{Issuer: iss, JWKSURL: js.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return v
}

// Tokens with made-up kids must not each trigger a JWKS fetch.
func TestUnknownKidRefreshIsRateLimited(t *testing.T) {
	const iss = "https://idp.example.test"
	good, forger := mustRSAKey(t), mustRSAKey(t)
	js := newJWKSServer(t, map[string]*rsa.PublicKey{"k1": &good.PublicKey})
	v := remoteValidator(t, js, iss)
	exp := time.Now().Add(time.Hour)

	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := 0; i < 100; i++ {
		tok, err := SignTestToken(forger, fmt.Sprintf("forged-%d", i), iss, "mallory", "", "", exp)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := v.Validate(tok); err == nil {
				errs <- fmt.Errorf("forged token accepted")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if n := js.count(); n > 2 {
		t.Fatalf("100 unknown kids caused %d JWKS fetches; want at most the initial one plus one", n)
	}
}

func TestRotatedKeyIsPickedUpAfterTheInterval(t *testing.T) {
	const iss = "https://idp.example.test"
	k1, k2 := mustRSAKey(t), mustRSAKey(t)
	js := newJWKSServer(t, map[string]*rsa.PublicKey{"k1": &k1.PublicKey})
	v := remoteValidator(t, js, iss)
	v.minRefresh = 50 * time.Millisecond

	js.setKeys(map[string]*rsa.PublicKey{"k1": &k1.PublicKey, "k2": &k2.PublicKey})
	tok, err := SignTestToken(k2, "k2", iss, "alice", "", "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if _, err := v.Validate(tok); err != nil {
		t.Fatalf("token signed with the rotated-in key: %v", err)
	}
}

// Run drops a key the IdP removed, without waiting for a restart.
func TestRunDropsRemovedKeys(t *testing.T) {
	const iss = "https://idp.example.test"
	k1, k2 := mustRSAKey(t), mustRSAKey(t)
	js := newJWKSServer(t, map[string]*rsa.PublicKey{"k1": &k1.PublicKey})
	v := remoteValidator(t, js, iss)
	tok, err := SignTestToken(k1, "k1", iss, "alice", "", "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Validate(tok); err != nil {
		t.Fatal(err)
	}

	js.setKeys(map[string]*rsa.PublicKey{"k2": &k2.PublicKey})
	v.cacheTTL = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go v.Run(ctx)
	// The fetch is counted before the validator swaps its keys: wait for the swap.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := v.Validate(tok); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("a key removed from the IdP's JWKS still validates after %d fetches", js.count())
}

func TestValidateTenantClaim(t *testing.T) {
	key := mustRSAKey(t)
	const iss, kid = "https://idp.example.test", "k"
	keys := map[string]*rsa.PublicKey{kid: &key.PublicKey}
	exp := time.Now().Add(time.Hour).Unix()
	tok := func(claims map[string]any) string {
		claims["iss"], claims["sub"], claims["exp"] = iss, "alice", exp
		return signRawToken(t, key, kid, claims)
	}
	plain, _ := NewValidatorWithPublicKeys(Config{Issuer: iss}, keys)
	withDefault, _ := NewValidatorWithPublicKeys(Config{Issuer: iss, DefaultTenant: "acme"}, keys)
	custom, _ := NewValidatorWithPublicKeys(Config{Issuer: iss, TenantClaim: "org"}, keys)

	for _, tc := range []struct {
		name   string
		v      *Validator
		claims map[string]any
		want   string
		err    bool
	}{
		{"string claim", plain, map[string]any{"tenant_id": "t1"}, "t1", false},
		{"one-element array", plain, map[string]any{"tenant_id": []string{"t2"}}, "t2", false},
		{"several tenants", plain, map[string]any{"tenant_id": []string{"t1", "t2"}}, "", true},
		{"no claim, no default", plain, map[string]any{}, "", false},
		{"no claim, default", withDefault, map[string]any{}, "acme", false},
		{"claim wins over default", withDefault, map[string]any{"tenant_id": "t3"}, "t3", false},
		{"custom claim name", custom, map[string]any{"org": "o1", "tenant_id": "ignored"}, "o1", false},
	} {
		p, err := tc.v.Validate(tok(tc.claims))
		if (err != nil) != tc.err || p.TenantID != tc.want {
			t.Errorf("%s: tenant=%q err=%v, want %q (error %v)", tc.name, p.TenantID, err, tc.want, tc.err)
		}
	}
}

func TestConfigFromEnvTenant(t *testing.T) {
	t.Setenv("ASP_IDP_TENANT_CLAIM", "org")
	t.Setenv("ASP_IDP_DEFAULT_TENANT", "acme")
	cfg := ConfigFromEnv()
	if cfg.TenantClaim != "org" || cfg.DefaultTenant != "acme" {
		t.Fatalf("cfg=%+v", cfg)
	}
}
