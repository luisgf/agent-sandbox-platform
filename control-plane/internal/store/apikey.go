package store

import (
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
