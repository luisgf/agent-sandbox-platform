package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// envAllowTmpKeys lets a node agent configured for production keep key
// material in a temporary directory.
const envAllowTmpKeys = "ASP_ALLOW_TMP_KEYS"

// nodeProdMode reports whether the node agent runs like a production node,
// and which setting says so: not --dry-run, and the control plane reaches it
// over mTLS or it holds an enrolled certificate.
func nodeProdMode(cfg config) (bool, string) {
	switch {
	case cfg.DryRun:
		return false, ""
	case cfg.AgentTLSListen != "":
		return true, "--agent-tls-listen is set"
	case cfg.MTLS:
		return true, "--mtls is set"
	}
	if _, err := os.Stat(filepath.Join(cfg.CertDir, "client.crt")); err == nil {
		return true, "--cert-dir holds a node certificate"
	}
	return false, ""
}

type keyLocation struct{ name, path string }

// nodeKeyLocations is where the node agent keeps key material it uses. The
// attestation key only counts when ASP_ATTEST_KEY is set, or in a lab: a
// production node signs attestations with its certificate key and skips an
// unset ASP_ATTEST_KEY (see attestSigners).
func nodeKeyLocations(cfg config, prod bool) []keyLocation {
	locs := []keyLocation{{"--cert-dir (ASP_CERT_DIR)", cfg.CertDir}}
	if p := strings.TrimSpace(os.Getenv("ASP_ATTEST_KEY")); p != "" {
		locs = append(locs, keyLocation{"ASP_ATTEST_KEY", p})
	} else if !prod {
		locs = append(locs, keyLocation{"ASP_ATTEST_KEY", filepath.Join(os.TempDir(), "asp-attest-key.pem")})
	}
	if cfg.EgressMITM {
		p := strings.TrimSpace(cfg.EgressMITMCA)
		if p == "" {
			p = filepath.Join(os.TempDir(), "asp-egress-mitm-ca.pem")
		}
		locs = append(locs, keyLocation{"--egress-mitm-ca (ASP_EGRESS_MITM_CA)", p})
	}
	return locs
}

// checkNodeKeyLocations refuses to run a production node on key material in a
// temporary directory: a reboot or a tmp cleaner wipes the node certificate
// (the control plane does not let the bootstrap token re-enroll a live node)
// and the MITM CA guests trust. In a lab it warns.
func checkNodeKeyLocations(cfg config) error {
	prod, why := nodeProdMode(cfg)
	var inTmp []string
	for _, k := range nodeKeyLocations(cfg, prod) {
		if inTempDir(k.path) {
			inTmp = append(inTmp, k.name+"="+k.path)
		}
	}
	if len(inTmp) == 0 {
		return nil
	}
	switch {
	case !prod:
		slog.Warn("key material in a temporary directory: fine for a lab, lost on reboot", "keys", inTmp)
		return nil
	case envTruthy(envAllowTmpKeys):
		slog.Warn(envAllowTmpKeys+"=1: production node with key material in a temporary directory, lost on reboot", "reason", why, "keys", inTmp)
		return nil
	}
	return fmt.Errorf("production node (%s) with key material in a temporary directory, which a reboot or tmp cleaner wipes: %s. "+
		"Use persistent storage such as /var/lib/asp, or set %s=1",
		why, strings.Join(inTmp, ", "), envAllowTmpKeys)
}

func envTruthy(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
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
// symlinks of its nearest existing parent (macOS: /tmp → /private/tmp).
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
