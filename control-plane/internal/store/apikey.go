package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// normalizeScope validates an API key scope; empty means tenant.
func normalizeScope(scope string) (string, error) {
	switch strings.TrimSpace(scope) {
	case "", APIKeyScopeTenant:
		return APIKeyScopeTenant, nil
	case APIKeyScopePlatform:
		return APIKeyScopePlatform, nil
	}
	return "", fmt.Errorf("%w: api key scope %q (want tenant or platform)", ErrInvalidInput, scope)
}

// HashAPIKeySecret returns the sha256 hex digest of a raw API key secret.
func HashAPIKeySecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// KeyPrefix returns a stable non-secret prefix for display (up to 8 runes).
func KeyPrefix(secret string) string {
	s := strings.TrimSpace(secret)
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

// GenerateAPIKeySecret returns a new random API key secret: "asp_" and 160
// bits in hex. The first eight characters are the display prefix, which is
// unique per key, so a clash is retried with a fresh secret.
func GenerateAPIKeySecret() (string, error) {
	var b [20]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "asp_" + hex.EncodeToString(b[:]), nil
}
