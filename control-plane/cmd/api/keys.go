package main

import (
	"fmt"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/envcfg"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/attest"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/oidc"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/pki"
)

// EnvAllowTmpKeys lets a control plane configured for production start with
// key material in a temporary directory.
const EnvAllowTmpKeys = "ASP_ALLOW_TMP_KEYS"

// configError is a refusal to start because of the configuration (exit 2).
type configError struct{ error }

// prodMode reports whether the control plane is configured like a production
// deployment, and which setting says so.
func prodMode() (bool, string) {
	for _, k := range []string{"ASP_CLIENT_CA", "ASP_TLS_CERT"} {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			return true, k + " is set"
		}
	}
	if databaseURL() != "" {
		return true, EnvDatabaseURL + " is set"
	}
	if envcfg.Truthy("ASP_IDP_REQUIRED") {
		return true, "ASP_IDP_REQUIRED=1"
	}
	return false, ""
}

type keyLocation struct{ env, path string }

// keyLocations is where the control plane keeps its key material, defaults
// applied.
func keyLocations() []keyLocation {
	caCert, caKey := pki.PathsFromEnv()
	return []keyLocation{
		{"ASP_CA_CERT", caCert},
		{"ASP_CA_KEY", caKey},
		{"ASP_OIDC_KEY", oidc.KeyPathFromEnv()},
		{"ASP_ATTEST_KEY", attest.KeyPathFromEnv()},
	}
}

// checkKeyLocations refuses to run a production-looking control plane on key
// material in a temporary directory: a reboot or a tmp cleaner wipes it, every
// node certificate stops verifying and tokens change kid. In a lab it warns.
func checkKeyLocations() error {
	var inTmp []string
	for _, k := range keyLocations() {
		if inTempDir(k.path) {
			inTmp = append(inTmp, k.env+"="+k.path)
		}
	}
	if len(inTmp) == 0 {
		return nil
	}
	prod, why := prodMode()
	switch {
	case !prod:
		slog.Warn("key material in a temporary directory: fine for a lab, lost on reboot", "keys", inTmp)
		return nil
	case envcfg.Truthy(EnvAllowTmpKeys):
		slog.Warn(EnvAllowTmpKeys+"=1: production mode with key material in a temporary directory, lost on reboot", "reason", why, "keys", inTmp)
		return nil
	}
	return configError{fmt.Errorf("production mode (%s) with key material in a temporary directory, which a reboot or tmp cleaner wipes: %s. "+
		"Point these variables at persistent storage such as /var/lib/asp (missing keys are created there), or set %s=1",
		why, strings.Join(inTmp, ", "), EnvAllowTmpKeys)}
}

// tempDirs are directories whose content does not survive a reboot or is
// cleaned periodically.
func tempDirs() []string {
	dirs := []string{"/tmp", "/var/tmp", "/dev/shm", os.TempDir()}
	out := make([]string, 0, 2*len(dirs))
	for _, d := range dirs {
		out = append(out, filepath.Clean(d))
		if r, err := filepath.EvalSymlinks(d); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// inTempDir reports whether path is inside a temporary directory, following
// symlinks of its parent where they exist (macOS: /tmp → /private/tmp).
func inTempDir(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	candidates := []string{abs}
	for dir := filepath.Dir(abs); ; dir = filepath.Dir(dir) {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			rel, _ := filepath.Rel(dir, abs)
			candidates = append(candidates, filepath.Join(r, rel))
			break
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	for _, t := range tempDirs() {
		for _, c := range candidates {
			if rel, err := filepath.Rel(t, c); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}
