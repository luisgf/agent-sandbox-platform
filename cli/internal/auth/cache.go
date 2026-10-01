package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultSkew = 30 * time.Second

// DefaultCachePath returns ~/.cache/asp/id_token.json (override ASP_IDP_TOKEN_CACHE).
func DefaultCachePath() string {
	if p := strings.TrimSpace(os.Getenv("ASP_IDP_TOKEN_CACHE")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".cache", "asp", "id_token.json")
}

// LoadCache reads a previously saved Token from path.
func LoadCache(path string) (Token, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Token{}, err
	}
	var tok Token
	if err := json.Unmarshal(b, &tok); err != nil {
		return Token{}, err
	}
	return tok, nil
}

// SaveCache writes tok to path (0600), creating parent dirs.
func SaveCache(path string, tok Token) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// ClearCache removes the cache file if present.
func ClearCache(path string) error {
	if path == "" {
		return nil
	}
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// DefaultSkew is the refresh lead time before expiry.
func DefaultSkew() time.Duration { return defaultSkew }
