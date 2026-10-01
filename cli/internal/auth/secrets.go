// Package auth obtains and caches IdP access tokens for the asp CLI.
package auth

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Secrets holds Keycloak/OIDC client credentials loaded from a host file or env.
// Field names match ~/.secrets/asp-keycloak-lab.txt (CLIENT_ID, …) plus optional TOKEN_URL.
type Secrets struct {
	ClientID     string
	ClientSecret string
	Username     string
	Password     string
	Issuer       string
	TokenURL     string
	JWKS         string
}

// LoadSecretsFile parses KEY=VALUE lines (comments and blanks ignored).
func LoadSecretsFile(path string) (Secrets, error) {
	f, err := os.Open(path)
	if err != nil {
		return Secrets{}, err
	}
	defer f.Close()
	kv := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if err := sc.Err(); err != nil {
		return Secrets{}, err
	}
	return secretsFromMap(kv), nil
}

func secretsFromMap(kv map[string]string) Secrets {
	get := func(keys ...string) string {
		for _, k := range keys {
			if v := strings.TrimSpace(kv[k]); v != "" {
				return v
			}
		}
		return ""
	}
	return Secrets{
		ClientID:     get("CLIENT_ID", "ASP_IDP_CLIENT_ID"),
		ClientSecret: get("CLIENT_SECRET", "ASP_IDP_CLIENT_SECRET"),
		Username:     get("USER", "USERNAME", "ASP_IDP_USERNAME"),
		Password:     get("PASSWORD", "ASP_IDP_PASSWORD"),
		Issuer:       get("ISSUER", "ASP_IDP_ISSUER"),
		TokenURL:     get("TOKEN_URL", "ASP_IDP_TOKEN_URL"),
		JWKS:         get("JWKS", "JWKS_URL", "ASP_IDP_JWKS_URL"),
	}
}

// Merge overlays non-empty fields from o onto s.
func (s Secrets) Merge(o Secrets) Secrets {
	if o.ClientID != "" {
		s.ClientID = o.ClientID
	}
	if o.ClientSecret != "" {
		s.ClientSecret = o.ClientSecret
	}
	if o.Username != "" {
		s.Username = o.Username
	}
	if o.Password != "" {
		s.Password = o.Password
	}
	if o.Issuer != "" {
		s.Issuer = o.Issuer
	}
	if o.TokenURL != "" {
		s.TokenURL = o.TokenURL
	}
	if o.JWKS != "" {
		s.JWKS = o.JWKS
	}
	return s
}

// ResolveTokenURL returns an explicit token URL or derives it from Issuer.
func (s Secrets) ResolveTokenURL() (string, error) {
	if u := strings.TrimSpace(s.TokenURL); u != "" {
		return u, nil
	}
	iss := strings.TrimRight(strings.TrimSpace(s.Issuer), "/")
	if iss == "" {
		return "", fmt.Errorf("missing ASP_IDP_TOKEN_URL / TOKEN_URL (or ISSUER to derive it)")
	}
	return iss + "/protocol/openid-connect/token", nil
}

// DefaultSecretsPath is the lab host file for Keycloak client credentials.
func DefaultSecretsPath() string {
	if p := strings.TrimSpace(os.Getenv("ASP_IDP_SECRETS_FILE")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return home + "/.secrets/asp-keycloak-lab.txt"
}

// SecretsFromEnv reads credential overrides from the process environment.
func SecretsFromEnv() Secrets {
	return Secrets{
		ClientID:     strings.TrimSpace(os.Getenv("ASP_IDP_CLIENT_ID")),
		ClientSecret: strings.TrimSpace(os.Getenv("ASP_IDP_CLIENT_SECRET")),
		Username:     strings.TrimSpace(firstEnv("ASP_IDP_USERNAME", "ASP_IDP_USER")),
		Password:     strings.TrimSpace(os.Getenv("ASP_IDP_PASSWORD")),
		Issuer:       strings.TrimSpace(os.Getenv("ASP_IDP_ISSUER")),
		TokenURL:     strings.TrimSpace(os.Getenv("ASP_IDP_TOKEN_URL")),
		JWKS:         strings.TrimSpace(os.Getenv("ASP_IDP_JWKS_URL")),
	}
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
