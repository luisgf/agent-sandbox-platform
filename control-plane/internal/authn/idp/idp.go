// Package idp validates corporate IdP (OIDC) Bearer JWTs for the control-plane API (ADR-0007 phase 2).
package idp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	defaultJWKSCacheTTL = 5 * time.Minute
	// defaultMinRefresh is the shortest gap between JWKS fetches triggered by
	// tokens with an unknown kid.
	defaultMinRefresh = 30 * time.Second
	// clockSkew tolerates a token whose nbf is slightly ahead of this clock.
	clockSkew = time.Minute
	// maxIatFuture rejects tokens issued further in the future than this.
	maxIatFuture = 5 * time.Minute
)

// Config controls IdP JWT validation. Required=false (default) keeps lab behavior.
type Config struct {
	Issuer   string
	Audience string
	JWKSURL  string
	Required bool
	// RoleClaim is the JWT claim holding groups/roles (default "groups"). ASP_IDP_ROLE_CLAIM.
	RoleClaim string
	// RoleMap maps claim values → Role when ASP_IDP_ROLE_MAP is set (e.g. "asp-admin:admin").
	RoleMap map[string]Role
	// RolePrefix maps "{prefix}admin|operator|viewer" when RoleMap is empty (default "asp-").
	RolePrefix string
	// DestroyAnyGroup claim value that lets operators destroy any sandbox (default "sandbox:destroy-any").
	DestroyAnyGroup string
	// AllowMissingExp accepts tokens without an exp claim (ASP_IDP_REQUIRE_EXP=0).
	// Off by default: a token without exp would be valid forever.
	AllowMissingExp bool
	// TenantClaim names the claim with the user's tenant (ASP_IDP_TENANT_CLAIM,
	// default "tenant_id"): a string, or an array with exactly one value.
	TenantClaim string
	// DefaultTenant is the tenant of tokens without TenantClaim
	// (ASP_IDP_DEFAULT_TENANT), for single-tenant deployments.
	DefaultTenant string
}

const defaultTenantClaim = "tenant_id"

// Principal is the authenticated human (or service principal) from an IdP JWT.
type Principal struct {
	Sub        string
	Email      string
	Role       Role
	DestroyAny bool // operator may destroy non-owned sandboxes (ADR-0007)
	// TenantID is the tenant the principal acts within: TenantClaim, else
	// DefaultTenant. Empty when neither applies; the API refuses such tokens.
	TenantID string
}

// Validator verifies RS256 IdP JWTs against JWKS (fetched or static).
type Validator struct {
	cfg    Config
	client *http.Client

	mu       sync.RWMutex
	keys     map[string]*rsa.PublicKey // kid -> key; "" for single-key sets
	keysAt   time.Time
	cacheTTL time.Duration // Run refreshes the keys this often
	static   bool          // keys pinned; never refetch
	jwksURL  string

	// refreshMu serialises fetches: concurrent misses wait for the one in
	// flight instead of each fetching. lastRefresh is the last attempt.
	refreshMu   sync.Mutex
	lastRefresh time.Time
	minRefresh  time.Duration
}

// ConfigFromEnv reads ASP_IDP_* variables.
// ASP_IDP_REQUIRED=0|1 (also true/yes). Discovery used when JWKS URL empty and Issuer set.
func ConfigFromEnv() Config {
	cfg := Config{
		Issuer:   strings.TrimSpace(os.Getenv("ASP_IDP_ISSUER")),
		Audience: strings.TrimSpace(os.Getenv("ASP_IDP_AUDIENCE")),
		JWKSURL:  strings.TrimSpace(os.Getenv("ASP_IDP_JWKS_URL")),
		Required: envTruthy("ASP_IDP_REQUIRED"),
		// ASP_IDP_REQUIRE_EXP=0 is the escape hatch for IdPs that omit exp.
		AllowMissingExp: envFalsy("ASP_IDP_REQUIRE_EXP"),
		TenantClaim:     strings.TrimSpace(os.Getenv("ASP_IDP_TENANT_CLAIM")),
		DefaultTenant:   strings.TrimSpace(os.Getenv("ASP_IDP_DEFAULT_TENANT")),
	}
	RoleConfigFromEnv(&cfg)
	return cfg
}

func envFalsy(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "0", "false", "no", "off":
		return true
	}
	return false
}

func envTruthy(key string) bool {
	v := strings.TrimSpace(os.Getenv(key))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

// Enabled reports whether IdP validation can run (issuer configured).
func (c Config) Enabled() bool {
	return strings.TrimSpace(c.Issuer) != ""
}

// NewValidator builds a Validator. JWKSURL may be empty if discovery from Issuer works.
// Call Refresh(ctx) once at startup when using remote JWKS.
func NewValidator(cfg Config) (*Validator, error) {
	cfg.Issuer = strings.TrimRight(strings.TrimSpace(cfg.Issuer), "/")
	cfg.Audience = strings.TrimSpace(cfg.Audience)
	cfg.JWKSURL = strings.TrimSpace(cfg.JWKSURL)
	if cfg.Issuer == "" {
		return nil, errors.New("idp: issuer required")
	}
	if strings.TrimSpace(cfg.RoleClaim) == "" {
		cfg.RoleClaim = defaultRoleClaim
	}
	if strings.TrimSpace(cfg.DestroyAnyGroup) == "" {
		cfg.DestroyAnyGroup = defaultDestroyAnyGroup
	}
	if strings.TrimSpace(cfg.TenantClaim) == "" {
		cfg.TenantClaim = defaultTenantClaim
	}
	if len(cfg.RoleMap) == 0 && strings.TrimSpace(cfg.RolePrefix) == "" {
		cfg.RolePrefix = defaultRolePrefix
	}
	return &Validator{
		cfg:        cfg,
		client:     &http.Client{Timeout: 10 * time.Second},
		keys:       make(map[string]*rsa.PublicKey),
		cacheTTL:   defaultJWKSCacheTTL,
		jwksURL:    cfg.JWKSURL,
		minRefresh: defaultMinRefresh,
	}, nil
}

// NewValidatorWithPublicKeys is for tests (static JWKS, no HTTP).
func NewValidatorWithPublicKeys(cfg Config, pubs map[string]*rsa.PublicKey) (*Validator, error) {
	v, err := NewValidator(cfg)
	if err != nil {
		return nil, err
	}
	v.static = true
	v.keys = make(map[string]*rsa.PublicKey, len(pubs))
	for kid, pub := range pubs {
		if pub == nil {
			continue
		}
		v.keys[kid] = pub
	}
	v.keysAt = time.Now()
	return v, nil
}

// FromEnv constructs a Validator when ASP_IDP_ISSUER is set; otherwise returns (nil, nil).
func FromEnv() (*Validator, Config, error) {
	cfg := ConfigFromEnv()
	if !cfg.Enabled() {
		return nil, cfg, nil
	}
	v, err := NewValidator(cfg)
	if err != nil {
		return nil, cfg, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := v.Refresh(ctx); err != nil {
		// Allow Required=false labs to start even if IdP is briefly unreachable;
		// validation will retry on demand. When Required, fail fast.
		if cfg.Required {
			return nil, cfg, fmt.Errorf("idp jwks refresh: %w", err)
		}
	}
	return v, cfg, nil
}

// Config returns a copy of the validator config.
func (v *Validator) Config() Config {
	if v == nil {
		return Config{}
	}
	return v.cfg
}

// LooksLikeJWT reports whether the bearer secret has three JWT segments.
func LooksLikeJWT(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	parts := strings.Split(token, ".")
	return len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] != ""
}

// Refresh loads JWKS (discovery if needed). Safe to call concurrently: calls
// run one at a time. On failure the keys already loaded are kept.
func (v *Validator) Refresh(ctx context.Context) error {
	if v == nil {
		return errors.New("idp: nil validator")
	}
	if v.static {
		return nil
	}
	v.refreshMu.Lock()
	defer v.refreshMu.Unlock()
	v.lastRefresh = time.Now()
	return v.refresh(ctx)
}

// refreshAfterMiss refreshes because a token named an unknown kid, which may be
// a key the IdP just rotated in. It fetches at most every minRefresh, so tokens
// with made-up kids cannot turn the control plane into a request generator
// against the IdP; misses that arrive during a fetch wait for it.
func (v *Validator) refreshAfterMiss() {
	if v.static {
		return
	}
	v.refreshMu.Lock()
	defer v.refreshMu.Unlock()
	if !v.lastRefresh.IsZero() && time.Since(v.lastRefresh) < v.minRefresh {
		return
	}
	v.lastRefresh = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = v.refresh(ctx)
}

// Run refreshes the keys every cache TTL until ctx is done, so a key the IdP
// removes stops validating without a restart and without waiting for a miss.
func (v *Validator) Run(ctx context.Context) {
	if v == nil || v.static || v.cacheTTL <= 0 {
		return
	}
	t := time.NewTicker(v.cacheTTL)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := v.Refresh(rctx); err != nil {
				slog.Warn("idp jwks refresh failed; keeping the keys already loaded", "error", err)
			}
			cancel()
		}
	}
}

// refresh does the fetch. Caller holds refreshMu.
func (v *Validator) refresh(ctx context.Context) error {
	url := v.jwksURL
	if url == "" {
		disc, err := v.discoverJWKSURL(ctx)
		if err != nil {
			return err
		}
		url = disc
		v.mu.Lock()
		v.jwksURL = disc
		v.mu.Unlock()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("jwks http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	keys, err := parseJWKS(raw)
	if err != nil {
		return err
	}
	v.mu.Lock()
	v.keys = keys
	v.keysAt = time.Now()
	v.mu.Unlock()
	return nil
}

func (v *Validator) discoverJWKSURL(ctx context.Context) (string, error) {
	discURL := v.cfg.Issuer + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oidc discovery http %d", resp.StatusCode)
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
		Issuer  string `json:"issuer"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return "", err
	}
	if doc.JWKSURI == "" {
		return "", errors.New("oidc discovery missing jwks_uri")
	}
	return doc.JWKSURI, nil
}

// Validate verifies signature (RS256), iss, aud (if configured), exp; returns Principal.
func (v *Validator) Validate(token string) (Principal, error) {
	if v == nil {
		return Principal{}, errors.New("idp: validator not configured")
	}
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return Principal{}, errors.New("malformed jwt")
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Principal{}, fmt.Errorf("jwt header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(hb, &header); err != nil {
		return Principal{}, fmt.Errorf("jwt header json: %w", err)
	}
	if header.Alg != "RS256" {
		return Principal{}, fmt.Errorf("unsupported alg %q", header.Alg)
	}

	pub, err := v.lookupKey(header.Kid)
	if err != nil {
		v.refreshAfterMiss()
		pub, err = v.lookupKey(header.Kid)
		if err != nil {
			return Principal{}, err
		}
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Principal{}, fmt.Errorf("jwt signature: %w", err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return Principal{}, errors.New("invalid jwt signature")
	}

	cb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Principal{}, fmt.Errorf("jwt claims: %w", err)
	}
	var claims claimsJSON
	if err := json.Unmarshal(cb, &claims); err != nil {
		return Principal{}, fmt.Errorf("jwt claims json: %w", err)
	}
	if claims.Sub == "" {
		return Principal{}, errors.New("jwt missing sub")
	}
	iss := strings.TrimRight(claims.Iss, "/")
	if iss != v.cfg.Issuer {
		return Principal{}, fmt.Errorf("jwt iss mismatch: got %q want %q", claims.Iss, v.cfg.Issuer)
	}
	if v.cfg.Audience != "" {
		if !audienceMatch(claims.Aud, v.cfg.Audience) {
			return Principal{}, fmt.Errorf("jwt aud mismatch: want %q", v.cfg.Audience)
		}
	}
	now := time.Now().UTC()
	switch {
	case claims.Exp <= 0 && !v.cfg.AllowMissingExp:
		return Principal{}, errors.New("jwt missing exp")
	case claims.Exp > 0 && now.Unix() > claims.Exp:
		return Principal{}, errors.New("token expired")
	}
	if claims.Nbf > 0 && now.Add(clockSkew).Unix() < claims.Nbf {
		return Principal{}, errors.New("token not yet valid")
	}
	if claims.Iat > 0 && claims.Iat > now.Add(maxIatFuture).Unix() {
		return Principal{}, errors.New("token issued in the future")
	}

	email := strings.TrimSpace(claims.Email)
	if email == "" {
		email = strings.TrimSpace(claims.PreferredUsername)
	}
	roleValues := extractRoleClaim(cb, v.cfg.RoleClaim, claims)
	role, destroyAny := v.cfg.MapRoles(roleValues)
	tenant, err := extractTenantClaim(cb, v.cfg.TenantClaim)
	if err != nil {
		return Principal{}, err
	}
	if tenant == "" {
		tenant = strings.TrimSpace(v.cfg.DefaultTenant)
	}
	return Principal{Sub: claims.Sub, Email: email, Role: role, DestroyAny: destroyAny, TenantID: tenant}, nil
}

// extractTenantClaim reads the tenant claim: a string, or an array with one
// value. Several values are refused rather than guessed.
func extractTenantClaim(raw []byte, claim string) (string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", nil
	}
	rb, ok := m[claim]
	if !ok {
		return "", nil
	}
	var values stringSliceFlex
	if err := values.UnmarshalJSON(rb); err != nil {
		return "", fmt.Errorf("jwt %s claim: %w", claim, err)
	}
	switch len(values) {
	case 0:
		return "", nil
	case 1:
		return strings.TrimSpace(values[0]), nil
	}
	return "", fmt.Errorf("jwt %s claim names %d tenants; want one", claim, len(values))
}

func extractRoleClaim(raw []byte, claim string, c claimsJSON) []string {
	claim = strings.TrimSpace(claim)
	if claim == "" {
		claim = defaultRoleClaim
	}
	switch strings.ToLower(claim) {
	case "groups":
		return []string(c.Groups)
	case "roles":
		return []string(c.Roles)
	}
	// Custom claim name: pull from raw JSON object.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	rb, ok := m[claim]
	if !ok {
		return nil
	}
	var flex stringSliceFlex
	if err := flex.UnmarshalJSON(rb); err != nil {
		return nil
	}
	return []string(flex)
}

func (v *Validator) lookupKey(kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if len(v.keys) == 0 {
		return nil, errors.New("idp: no jwks keys loaded")
	}
	if kid != "" {
		if pub, ok := v.keys[kid]; ok {
			return pub, nil
		}
	}
	// single key or kid omitted: use sole entry
	if kid == "" && len(v.keys) == 1 {
		for _, pub := range v.keys {
			return pub, nil
		}
	}
	// also try empty-kid bucket
	if pub, ok := v.keys[""]; ok && kid == "" {
		return pub, nil
	}
	if kid != "" {
		return nil, fmt.Errorf("idp: no jwk for kid %q", kid)
	}
	return nil, errors.New("idp: ambiguous jwks without kid")
}

type claimsJSON struct {
	Iss               string          `json:"iss"`
	Sub               string          `json:"sub"`
	Exp               int64           `json:"exp"`
	Nbf               int64           `json:"nbf"`
	Iat               int64           `json:"iat"`
	Email             string          `json:"email"`
	PreferredUsername string          `json:"preferred_username"`
	Aud               audFlex         `json:"aud"`
	Groups            stringSliceFlex `json:"groups"`
	Roles             stringSliceFlex `json:"roles"`
}

// audFlex accepts JWT aud as string or []string.
type audFlex []string

func (a *audFlex) UnmarshalJSON(b []byte) error {
	b = bytesTrim(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*a = []string{s}
		return nil
	}
	var arr []string
	if err := json.Unmarshal(b, &arr); err != nil {
		return err
	}
	*a = arr
	return nil
}

func bytesTrim(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

func audienceMatch(got audFlex, want string) bool {
	for _, a := range got {
		if a == want {
			return true
		}
	}
	return false
}

func parseJWKS(raw []byte) (map[string]*rsa.PublicKey, error) {
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
			Alg string `json:"alg"`
			Use string `json:"use"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	out := make(map[string]*rsa.PublicKey)
	for _, k := range doc.Keys {
		if k.Kty != "" && k.Kty != "RSA" {
			continue
		}
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		nb, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil || len(nb) == 0 {
			continue
		}
		eb, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil || len(eb) == 0 {
			continue
		}
		pub := &rsa.PublicKey{
			N: new(big.Int).SetBytes(nb),
			E: int(new(big.Int).SetBytes(eb).Int64()),
		}
		out[k.Kid] = pub
	}
	if len(out) == 0 {
		return nil, errors.New("jwks: no usable RSA keys")
	}
	return out, nil
}

// SignTestToken builds an RS256 JWT for tests (same shape as corporate IdP tokens).
func SignTestToken(key *rsa.PrivateKey, kid, iss, sub, aud, email string, exp time.Time) (string, error) {
	return SignTestTokenClaims(key, kid, iss, sub, aud, email, exp, nil)
}

// SignTestTokenClaims is SignTestToken plus optional extra claims (e.g. groups/roles).
func SignTestTokenClaims(key *rsa.PrivateKey, kid, iss, sub, aud, email string, exp time.Time, extra map[string]any) (string, error) {
	if key == nil {
		return "", errors.New("nil key")
	}
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if kid != "" {
		header["kid"] = kid
	}
	claims := map[string]any{
		"iss": strings.TrimRight(iss, "/"),
		"sub": sub,
		"exp": exp.Unix(),
		"iat": time.Now().UTC().Unix(),
	}
	if aud != "" {
		claims["aud"] = aud
	}
	if email != "" {
		claims["email"] = email
	}
	for k, v := range extra {
		if k == "" || v == nil {
			continue
		}
		claims[k] = v
	}
	hb, _ := json.Marshal(header)
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// JWKSJSON builds a JWKS document from RSA public keys (tests / static lab).
func JWKSJSON(pubs map[string]*rsa.PublicKey) ([]byte, error) {
	type jwk struct {
		Kty string `json:"kty"`
		Use string `json:"use"`
		Alg string `json:"alg"`
		Kid string `json:"kid,omitempty"`
		N   string `json:"n"`
		E   string `json:"e"`
	}
	keys := make([]jwk, 0, len(pubs))
	for kid, pub := range pubs {
		if pub == nil {
			continue
		}
		keys = append(keys, jwk{
			Kty: "RSA",
			Use: "sig",
			Alg: "RS256",
			Kid: kid,
			N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		})
	}
	return json.Marshal(map[string]any{"keys": keys})
}
