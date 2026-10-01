package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadSecretsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lab.txt")
	content := `# comment
CLIENT_ID=asp-api
CLIENT_SECRET=sec
USER=asp-lab
PASSWORD=pw
ISSUER=https://auth.example/realms/asp

TOKEN_URL=
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSecretsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.ClientID != "asp-api" || s.Username != "asp-lab" || s.Issuer == "" {
		t.Fatalf("%+v", s)
	}
	u, err := s.ResolveTokenURL()
	if err != nil || !strings.HasSuffix(u, "/protocol/openid-connect/token") {
		t.Fatalf("url=%q err=%v", u, err)
	}
}

func TestFetchPasswordGrant(t *testing.T) {
	var gotGrant, gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotGrant = r.Form.Get("grant_type")
		gotUser = r.Form.Get("username")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "tok-abc",
			"token_type":   "Bearer",
			"expires_in":   300,
		})
	}))
	defer srv.Close()

	tok, err := FetchAccessToken(context.Background(), FetchOptions{
		Secrets: Secrets{
			ClientID: "c", ClientSecret: "s",
			Username: "u", Password: "p",
			TokenURL: srv.URL,
		},
		GrantType: GrantPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "tok-abc" || gotGrant != "password" || gotUser != "u" {
		t.Fatalf("tok=%+v grant=%q user=%q", tok, gotGrant, gotUser)
	}
	if !tok.Valid(0) {
		t.Fatal("expected valid")
	}
}

func TestFetchClientCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != GrantClientCredentials {
			t.Errorf("grant=%q", r.Form.Get("grant_type"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "cc-tok",
			"expires_in":   60,
		})
	}))
	defer srv.Close()
	tok, err := FetchAccessToken(context.Background(), FetchOptions{
		Secrets:   Secrets{ClientID: "c", ClientSecret: "s", TokenURL: srv.URL},
		GrantType: GrantClientCredentials,
	})
	if err != nil || tok.AccessToken != "cc-tok" {
		t.Fatalf("%v %+v", err, tok)
	}
}

func TestCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tok.json")
	tok := Token{AccessToken: "x", ExpiresIn: 120, ObtainedAt: time.Now()}
	if err := SaveCache(path, tok); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCache(path)
	if err != nil || got.AccessToken != "x" {
		t.Fatalf("%v %+v", err, got)
	}
	if !got.Valid(DefaultSkew()) {
		t.Fatal("expected valid cached token")
	}
}

func TestResolveBearerPriority(t *testing.T) {
	t.Setenv("ASP_ID_TOKEN", "")
	t.Setenv("ASP_IDP_ACCESS_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_IDP_GRANT_TYPE", "")
	t.Setenv("ASP_IDP_CLIENT_ID", "")
	t.Setenv("ASP_IDP_TOKEN_URL", "")

	// explicit wins
	res, err := ResolveBearer(context.Background(), ResolveInput{ExplicitToken: "exp"})
	if err != nil || res.Source != "id-token" || res.Bearer != "exp" {
		t.Fatalf("%v %+v", err, res)
	}

	// cache
	cache := filepath.Join(t.TempDir(), "c.json")
	_ = SaveCache(cache, Token{AccessToken: "cached", ExpiresIn: 600, ObtainedAt: time.Now()})
	res, err = ResolveBearer(context.Background(), ResolveInput{CachePath: cache, SecretsPath: filepath.Join(t.TempDir(), "missing")})
	if err != nil || res.Source != "cache" || res.Bearer != "cached" {
		t.Fatalf("%v %+v", err, res)
	}

	// fetch
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fetched", "expires_in": 60})
	}))
	defer srv.Close()
	secPath := filepath.Join(t.TempDir(), "sec.txt")
	_ = os.WriteFile(secPath, []byte("CLIENT_ID=c\nCLIENT_SECRET=s\nUSER=u\nPASSWORD=p\nTOKEN_URL="+srv.URL+"\n"), 0o600)
	cache2 := filepath.Join(t.TempDir(), "c2.json")
	res, err = ResolveBearer(context.Background(), ResolveInput{
		CachePath: cache2, SecretsPath: secPath, ForceFetch: true,
	})
	if err != nil || res.Source != "fetch" || res.Bearer != "fetched" {
		t.Fatalf("%v %+v", err, res)
	}

	// api-key fallback
	res, err = ResolveBearer(context.Background(), ResolveInput{
		APIKey: "key", SecretsPath: filepath.Join(t.TempDir(), "nope"), CachePath: filepath.Join(t.TempDir(), "empty.json"),
	})
	if err != nil || res.Source != "api-key" || res.Bearer != "key" {
		t.Fatalf("%v %+v", err, res)
	}

	// required without creds
	t.Setenv("ASP_IDP_REQUIRED", "1")
	_, err = ResolveBearer(context.Background(), ResolveInput{
		SecretsPath: filepath.Join(t.TempDir(), "nope"),
		CachePath:   filepath.Join(t.TempDir(), "empty.json"),
	})
	if err == nil {
		t.Fatal("expected error when required")
	}
}

func TestTokenExpired(t *testing.T) {
	tok := Token{AccessToken: "x", ExpiresIn: 10, ObtainedAt: time.Now().Add(-time.Minute)}
	if tok.Valid(DefaultSkew()) {
		t.Fatal("should be expired")
	}
}
