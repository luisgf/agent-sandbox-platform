package execproxy

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// DefaultTokenFile is where the node-agent and a same-host control plane share
// the secret that guards the local API.
const DefaultTokenFile = "/var/lib/asp/agent.token"

// The local API (exec in any sandbox, the egress policy check, SSH-agent
// approvals) listens on loopback, which is not a credential: any local user,
// any process in a container with host networking, and anything that can make
// the egress proxy open a connection to 127.0.0.1 can reach it. It takes a
// bearer token instead, a secret in a file that only root and the control
// plane's user can read.
//
// A control plane on another host does not use it: it reaches the agent over
// mutual TLS (--agent-tls-listen), whose handler has no token.

// LoadOrCreateToken returns the secret in the file at path. A file that does
// not exist is created, 0600, with 32 random bytes in hex, so an agent that
// starts first makes the secret the control plane then reads. An existing file
// must hold at least 16 characters and not be readable by group or others
// unless the operator made it so deliberately (a 0640 file for the control
// plane's group): permissions are reported, not changed.
func LoadOrCreateToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		tok := strings.TrimSpace(string(b))
		if len(tok) < 16 {
			return "", fmt.Errorf("agent token file %s holds %d characters: too short to be a secret (delete it to have a new one made)", path, len(tok))
		}
		return tok, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("agent token file: %w", err)
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw[:])
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("agent token file: %w", err)
	}
	// O_EXCL: two agents starting together do not each write their own.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return LoadOrCreateToken(path)
		}
		return "", fmt.Errorf("agent token file: %w", err)
	}
	if _, err := f.WriteString(tok + "\n"); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return tok, nil
}

// RequireToken wraps h so that every route but GET /healthz needs
// "Authorization: Bearer <token>". The token is compared in constant time and
// is taken from the header only, never from the URL.
func RequireToken(token string, h http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			h.ServeHTTP(w, r)
			return
		}
		got := ""
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			got = strings.TrimSpace(auth[len("Bearer "):])
		}
		if got == "" || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="asp-node-agent"`)
			writeErr(w, http.StatusUnauthorized, "node agent token required: send the secret of the agent's token file as a bearer token")
			return
		}
		h.ServeHTTP(w, r)
	})
}
