package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Grant types supported for lab / agent automation.
const (
	GrantPassword          = "password"
	GrantClientCredentials = "client_credentials"
)

// Token is an OAuth2 access token response (subset).
type Token struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type"`
	ExpiresIn    int       `json:"expires_in"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	ObtainedAt   time.Time `json:"obtained_at"`
}

// ExpiresAt returns when the access token should be considered expired.
func (t Token) ExpiresAt() time.Time {
	if t.ObtainedAt.IsZero() || t.ExpiresIn <= 0 {
		return time.Time{}
	}
	return t.ObtainedAt.Add(time.Duration(t.ExpiresIn) * time.Second)
}

// Valid reports whether the access token is still usable with skew.
func (t Token) Valid(skew time.Duration) bool {
	if strings.TrimSpace(t.AccessToken) == "" {
		return false
	}
	exp := t.ExpiresAt()
	if exp.IsZero() {
		return true // unknown expiry: treat as usable until 401
	}
	return time.Now().Add(skew).Before(exp)
}

// FetchOptions configures a token request.
type FetchOptions struct {
	Secrets    Secrets
	GrantType  string // password | client_credentials; empty = auto
	HTTPClient *http.Client
}

// FetchAccessToken performs a password or client_credentials grant against the IdP.
func FetchAccessToken(ctx context.Context, opt FetchOptions) (Token, error) {
	s := opt.Secrets
	tokenURL, err := s.ResolveTokenURL()
	if err != nil {
		return Token{}, err
	}
	if s.ClientID == "" {
		return Token{}, fmt.Errorf("missing CLIENT_ID / ASP_IDP_CLIENT_ID")
	}
	grant := strings.TrimSpace(opt.GrantType)
	if grant == "" {
		grant = chooseGrant(s)
	}
	form := url.Values{}
	form.Set("client_id", s.ClientID)
	if s.ClientSecret != "" {
		form.Set("client_secret", s.ClientSecret)
	}
	switch grant {
	case GrantPassword:
		if s.Username == "" || s.Password == "" {
			return Token{}, fmt.Errorf("password grant requires USER/PASSWORD (or ASP_IDP_USERNAME/ASP_IDP_PASSWORD)")
		}
		form.Set("grant_type", GrantPassword)
		form.Set("username", s.Username)
		form.Set("password", s.Password)
	case GrantClientCredentials:
		form.Set("grant_type", GrantClientCredentials)
	default:
		return Token{}, fmt.Errorf("unsupported grant_type %q (use %s or %s)", grant, GrantPassword, GrantClientCredentials)
	}

	hc := opt.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return Token{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Token{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 200 {
			msg = msg[:200] + "…"
		}
		return Token{}, fmt.Errorf("token endpoint HTTP %d: %s", resp.StatusCode, msg)
	}
	var tok Token
	if err := json.Unmarshal(raw, &tok); err != nil {
		return Token{}, fmt.Errorf("decode token response: %w", err)
	}
	if strings.TrimSpace(tok.AccessToken) == "" {
		return Token{}, fmt.Errorf("token response missing access_token")
	}
	tok.ObtainedAt = time.Now()
	return tok, nil
}

func chooseGrant(s Secrets) string {
	if g := strings.TrimSpace(os.Getenv("ASP_IDP_GRANT_TYPE")); g != "" {
		return g
	}
	if s.Username != "" && s.Password != "" {
		return GrantPassword
	}
	return GrantClientCredentials
}
