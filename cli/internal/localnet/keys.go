// Package localnet generates per-session WireGuard keys for the local agent.
// Private keys are written mode 0600 beside the session file, never inside it.
package localnet

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// KeyPath is the private key file for a session JSON path.
func KeyPath(sessionPath string) string {
	return sessionPath + ".local-net.key"
}

// PlanPath is the non-secret tunnel descriptor (public keys, iface, dial).
func PlanPath(sessionPath string) string {
	return sessionPath + ".local-net.plan"
}

// ConfPath is a wg-quick style snippet. It contains the private key, mode 0600.
func ConfPath(sessionPath string) string {
	return sessionPath + ".local-net.conf"
}

// Material is a freshly generated X25519 keypair (WireGuard encoding).
type Material struct {
	Private string
	Public  string
}

// Generate creates an X25519 keypair using the same raw encoding as WireGuard.
func Generate() (Material, error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return Material{}, err
	}
	return Material{
		Private: base64.StdEncoding.EncodeToString(k.Bytes()),
		Public:  base64.StdEncoding.EncodeToString(k.PublicKey().Bytes()),
	}, nil
}

// WritePrivate stores the private key mode 0600. It does not print it.
func WritePrivate(path, private string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("key path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(strings.TrimSpace(private)+"\n"), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// PublicFromPrivate derives the standard-base64 public key.
func PublicFromPrivate(private string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(private))
	if err != nil {
		return "", fmt.Errorf("private key: %w", err)
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("private key must be 32 bytes")
	}
	k, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(k.PublicKey().Bytes()), nil
}

// ReadPrivate loads a previously stored key.
func ReadPrivate(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("empty local-net key %s", path)
	}
	return s, nil
}

// Plan is safe to log. It must not contain the private key.
type Plan struct {
	Iface           string `json:"iface"`
	ClientPublicKey string `json:"client_public_key"`
	Dial            string `json:"dial,omitempty"`
	Transport       string `json:"transport"`
	WireGuardTools  bool   `json:"wireguard_tools"`
	Userspace       bool   `json:"userspace"`
	Note            string `json:"note"`
}

// WritePlan writes the descriptor mode 0600.
func WritePlan(path string, plan Plan) error {
	b, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// WriteConf writes a wg-quick-shaped file mode 0600. asp does not run wg-quick:
// wg-quick would install 0.0.0.0/0 in the host main table. The applied path is
// `ip link` + `wg set` (see device.go) and only when tools and CAP_NET_ADMIN exist.
func WriteConf(path, private, iface, address, nodePublic, endpoint string) error {
	body := fmt.Sprintf(`# local-net client. Private key. Mode 0600. Do not commit.
# Do not run wg-quick: Table=off is a reminder, not what we execute.
# Host main default route is not modified. AllowedIPs is cryptokey only.
# iface %s
[Interface]
PrivateKey = %s
Address = %s
Table = off

[Peer]
PublicKey = %s
Endpoint = %s
AllowedIPs = 0.0.0.0/0, ::/0
PersistentKeepalive = 25
`, iface, private, address, nodePublic, endpoint)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// WireGuardInstalled reports whether the wg(8) binary is on PATH.
// Absence is not a failure of the control-plane handshake.
func WireGuardInstalled() bool {
	_, err := exec.LookPath("wg")
	return err == nil
}

// Remove deletes key, plan and conf. Missing files are fine.
func Remove(sessionPath string) {
	for _, p := range []string{KeyPath(sessionPath), PlanPath(sessionPath), ConfPath(sessionPath)} {
		_ = os.Remove(p)
	}
}
