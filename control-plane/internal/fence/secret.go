package fence

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A fence credential is stored either as the secret itself or as a reference
// the control plane resolves when it fences, so an operator who does not want a
// BMC password in the database keeps it in the environment or in a file:
//
//	env:NAME        the value of the control plane's environment variable NAME
//	file:/abs/path  the contents of the file, without surrounding whitespace
//
// Anything else is the secret.
const (
	refEnv  = "env:"
	refFile = "file:"
)

// ValidateSecretRef checks the form of a credential given to the API; it does
// not look at the environment or the file, which may appear later.
func ValidateSecretRef(s string) error {
	switch {
	case strings.HasPrefix(s, refEnv):
		if strings.TrimSpace(strings.TrimPrefix(s, refEnv)) == "" {
			return fmt.Errorf("env: reference without a variable name")
		}
	case strings.HasPrefix(s, refFile):
		if p := strings.TrimPrefix(s, refFile); !filepath.IsAbs(p) {
			return fmt.Errorf("file: reference must be an absolute path")
		}
	}
	if len(s) > 4096 {
		return fmt.Errorf("credential too long")
	}
	return nil
}

// ResolveSecret returns the credential a reference names, or s itself when it
// is not a reference.
func ResolveSecret(s string) (string, error) {
	switch {
	case strings.HasPrefix(s, refEnv):
		name := strings.TrimSpace(strings.TrimPrefix(s, refEnv))
		v, ok := os.LookupEnv(name)
		if !ok || strings.TrimSpace(v) == "" {
			return "", fmt.Errorf("fence credential: environment variable %s is not set", name)
		}
		return strings.TrimSpace(v), nil
	case strings.HasPrefix(s, refFile):
		p := strings.TrimPrefix(s, refFile)
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("fence credential: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return s, nil
}
