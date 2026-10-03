// Package session persists local ASP sandbox sessions for reuse across CLI invocations.
// Each file holds sandbox id and control-plane URL only — never tokens or API keys.
// Named sessions live under a directory (default ~/.cache/asp/sessions/<name>.json).
// ASP_SESSION_FILE / an explicit path still selects one file and ignores the name.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ErrNoSession means the state file is absent.
var ErrNoSession = errors.New("no active asp session")

// DefaultName is used when the caller omits --name.
const DefaultName = "default"

// namePattern is a single path segment. Rejects "..", slashes, and empty.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// State is the on-disk session record (mode 0600).
type State struct {
	Name      string `json:"name,omitempty"`
	SandboxID string `json:"sandbox_id"`
	CPURL     string `json:"cp_url"`
	TenantID  string `json:"tenant_id,omitempty"`
	ImageRef  string `json:"image_ref,omitempty"`
	// Workspace is the host directory requested at start. It is not a mount.
	Workspace string `json:"workspace,omitempty"`
	// LocalNet records that this session opted into the full tunnel.
	// It is not authority and must not carry a WireGuard key or grant.
	LocalNet  bool      `json:"local_net,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ValidateName returns DefaultName when name is empty, or an error if it is
// not a safe single path segment.
func ValidateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return DefaultName, nil
	}
	if !namePattern.MatchString(name) {
		return "", fmt.Errorf("invalid session name %q (use letters, digits, '.', '_' or '-'; max 64)", name)
	}
	return name, nil
}

// DefaultDir returns ~/.cache/asp/sessions, or ASP_SESSION_DIR when set.
// Empty string if the home directory cannot be resolved and no override is set.
func DefaultDir() string {
	if p := strings.TrimSpace(os.Getenv("ASP_SESSION_DIR")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".cache", "asp", "sessions")
}

// NamedPath joins dir and "<name>.json". Empty dir falls back to DefaultDir.
func NamedPath(dir, name string) (string, error) {
	name, err := ValidateName(name)
	if err != nil {
		return "", err
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		dir = DefaultDir()
	}
	if dir == "" {
		return "", fmt.Errorf("cannot resolve session dir (set --session-dir or ASP_SESSION_DIR)")
	}
	return filepath.Join(dir, name+".json"), nil
}

// DefaultPath returns ~/.cache/asp/session.json, or ASP_SESSION_FILE when set.
// Empty string if the home directory cannot be resolved and no override is set.
// Prefer NamedPath. DefaultPath remains the explicit single-file override.
func DefaultPath() string {
	if p := strings.TrimSpace(os.Getenv("ASP_SESSION_FILE")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".cache", "asp", "session.json")
}

// Load reads a session file. Missing file returns ErrNoSession.
func Load(path string) (State, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return State{}, fmt.Errorf("session path is empty")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return State{}, ErrNoSession
		}
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("session file %s: %w", path, err)
	}
	st.SandboxID = strings.TrimSpace(st.SandboxID)
	st.CPURL = strings.TrimRight(strings.TrimSpace(st.CPURL), "/")
	if st.SandboxID == "" {
		return State{}, fmt.Errorf("session file %s has empty sandbox_id", path)
	}
	if st.CPURL == "" {
		return State{}, fmt.Errorf("session file %s has empty cp_url", path)
	}
	return st, nil
}

// Save writes st as JSON with mode 0600 (parent directory 0700).
// Permissions are chmod'd after write so umask cannot leave the file group-readable.
func Save(path string, st State) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("session path is empty")
	}
	if strings.TrimSpace(st.SandboxID) == "" {
		return fmt.Errorf("refusing to save session with empty sandbox_id")
	}
	if strings.TrimSpace(st.CPURL) == "" {
		return fmt.Errorf("refusing to save session with empty cp_url")
	}
	st.CPURL = strings.TrimRight(strings.TrimSpace(st.CPURL), "/")
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Tighten the leaf directory. Ignore failure when it is not ours (e.g. /tmp).
	if err := os.Chmod(dir, 0o700); err != nil {
		if fi, statErr := os.Stat(dir); statErr != nil || !fi.IsDir() {
			return err
		}
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	// O_EXCL is not used: start --force and normal save replace the record.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	if cerr != nil {
		return cerr
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	return nil
}

// Clear removes the state file. Absent file is success.
func Clear(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("session path is empty")
	}
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
