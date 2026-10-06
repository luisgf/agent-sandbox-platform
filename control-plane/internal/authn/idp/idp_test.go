package idp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestValidateHappyPathAndRejects(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const iss = "https://idp.example.test"
	const aud = "asp-api"
	const kid = "test-kid"

	v, err := NewValidatorWithPublicKeys(Config{Issuer: iss, Audience: aud}, map[string]*rsa.PublicKey{kid: &key.PublicKey})
	if err != nil {
		t.Fatal(err)
	}

	tok, err := SignTestToken(key, kid, iss, "user-alice", aud, "alice@ex.com", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	p, err := v.Validate(tok)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if p.Sub != "user-alice" || p.Email != "alice@ex.com" {
		t.Fatalf("principal=%+v", p)
	}

	// bad signature
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	bad, _ := SignTestToken(other, kid, iss, "user-alice", aud, "", time.Now().Add(time.Hour))
	if _, err := v.Validate(bad); err == nil {
		t.Fatal("want signature error")
	}

	// bad iss
	badIss, _ := SignTestToken(key, kid, "https://evil", "user-alice", aud, "", time.Now().Add(time.Hour))
	if _, err := v.Validate(badIss); err == nil {
		t.Fatal("want iss error")
	}

	// bad aud
	badAud, _ := SignTestToken(key, kid, iss, "user-alice", "other-aud", "", time.Now().Add(time.Hour))
	if _, err := v.Validate(badAud); err == nil {
		t.Fatal("want aud error")
	}

	// expired
	exp, _ := SignTestToken(key, kid, iss, "user-alice", aud, "", time.Now().Add(-time.Minute))
	if _, err := v.Validate(exp); err == nil {
		t.Fatal("want exp error")
	}
}

func TestJWKSFetchAndDiscovery(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const kid = "k1"
	jwks, err := JWKSJSON(map[string]*rsa.PublicKey{kid: &key.PublicKey})
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwks)
	})
	var iss string
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   iss,
			"jwks_uri": iss + "/jwks",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	iss = srv.URL

	v, err := NewValidator(Config{Issuer: iss, Audience: "asp", JWKSURL: ""})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	tok, err := SignTestToken(key, kid, iss, "bob", "asp", "bob@ex.com", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	p, err := v.Validate(tok)
	if err != nil || p.Sub != "bob" {
		t.Fatalf("got %+v err=%v", p, err)
	}
}

func TestLooksLikeJWT(t *testing.T) {
	if !LooksLikeJWT("a.b.c") {
		t.Fatal("want true")
	}
	if LooksLikeJWT("asp_lab_key_no_dots") {
		t.Fatal("want false")
	}
	if LooksLikeJWT("") || LooksLikeJWT("a.b") {
		t.Fatal("want false")
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("ASP_IDP_ISSUER", "https://login.example")
	t.Setenv("ASP_IDP_AUDIENCE", "asp-api")
	t.Setenv("ASP_IDP_JWKS_URL", "https://login.example/jwks")
	t.Setenv("ASP_IDP_REQUIRED", "1")
	cfg := ConfigFromEnv()
	if !cfg.Enabled() || !cfg.Required || cfg.Audience != "asp-api" {
		t.Fatalf("%+v", cfg)
	}
	t.Setenv("ASP_IDP_REQUIRED", "0")
	cfg = ConfigFromEnv()
	if cfg.Required {
		t.Fatal("want required false")
	}
}

func TestMapGrantsPrefixAndMap(t *testing.T) {
	cfg := Config{RoleClaim: "groups", RolePrefix: "asp-", DestroyAnyGroup: "sandbox:destroy-any", ExecAnyGroup: "sandbox:exec-any"}
	if g := cfg.MapGrants([]string{"asp-viewer", "asp-operator"}); g.Role != RoleOperator || g.DestroyAny || g.ExecAny {
		t.Fatalf("got %+v", g)
	}
	if g := cfg.MapGrants([]string{"asp-admin", "sandbox:destroy-any"}); g.Role != RoleAdmin || !g.DestroyAny {
		t.Fatalf("got %+v", g)
	}
	if g := cfg.MapGrants([]string{"asp-user"}); g.Role != RoleUser {
		t.Fatalf("asp-user got %+v", g)
	}
	// viewer and user are orthogonal: both together are operator.
	if g := cfg.MapGrants([]string{"asp-user", "asp-viewer"}); g.Role != RoleOperator {
		t.Fatalf("viewer+user got %+v", g)
	}
	if g := cfg.MapGrants([]string{"asp-operator", "SANDBOX:EXEC-ANY"}); g.Role != RoleOperator || !g.ExecAny {
		t.Fatalf("exec-any got %+v", g)
	}
	if g := cfg.MapGrants([]string{"asp-root", "other"}); g.Role != RoleNone {
		t.Fatalf("unknown groups got %+v", g)
	}
	cfg.RoleMap = map[string]Role{"Corp.Admin": RoleAdmin, "Corp.Ops": RoleOperator}
	cfg.RolePrefix = ""
	if g := cfg.MapGrants([]string{"Corp.Ops"}); g.Role != RoleOperator {
		t.Fatalf("map got %+v", g)
	}
}

func TestExecAnyGroupFromEnv(t *testing.T) {
	t.Setenv("ASP_IDP_EXEC_ANY_GROUP", "")
	cfg := Config{}
	RoleConfigFromEnv(&cfg)
	if cfg.ExecAnyGroup != "sandbox:exec-any" {
		t.Fatalf("default exec-any group %q", cfg.ExecAnyGroup)
	}
	t.Setenv("ASP_IDP_EXEC_ANY_GROUP", "corp-break-glass")
	RoleConfigFromEnv(&cfg)
	if cfg.ExecAnyGroup != "corp-break-glass" || !cfg.MapGrants([]string{"asp-operator", "corp-break-glass"}).ExecAny {
		t.Fatalf("custom exec-any group %+v", cfg)
	}
}

func TestValidateExtractsRoleFromGroups(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const iss = "https://idp.example.test"
	const aud = "asp-api"
	const kid = "test-kid"
	v, err := NewValidatorWithPublicKeys(Config{Issuer: iss, Audience: aud, RoleClaim: "groups", RolePrefix: "asp-"}, map[string]*rsa.PublicKey{kid: &key.PublicKey})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := SignTestTokenClaims(key, kid, iss, "user-alice", aud, "a@ex.com", time.Now().Add(time.Hour), map[string]any{
		"groups": []string{"asp-admin", "other"},
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := v.Validate(tok)
	if err != nil {
		t.Fatal(err)
	}
	if p.Role != RoleAdmin {
		t.Fatalf("role=%q", p.Role)
	}
}

func TestRoleConfigFromEnv(t *testing.T) {
	t.Setenv("ASP_IDP_ROLE_CLAIM", "roles")
	t.Setenv("ASP_IDP_ROLE_MAP", "g-admin:admin,g-op:operator")
	t.Setenv("ASP_IDP_ROLE_PREFIX", "")
	t.Setenv("ASP_IDP_DESTROY_ANY_GROUP", "destroy-world")
	cfg := ConfigFromEnv()
	if cfg.RoleClaim != "roles" || cfg.RoleMap["g-admin"] != RoleAdmin {
		t.Fatalf("%+v", cfg)
	}
	if cfg.DestroyAnyGroup != "destroy-world" {
		t.Fatalf("destroy=%q", cfg.DestroyAnyGroup)
	}
}
