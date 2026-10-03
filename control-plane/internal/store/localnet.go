package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Local-net states (ADR-0010). off is the public egress path. Anything else
// with local_net=true must not fall back to the node proxy.
const (
	LocalNetOff       = "off"
	LocalNetPending   = "pending"
	LocalNetUp        = "up"
	LocalNetWithdrawn = "withdrawn"
	// LocalNetGrantTTL is the clear-grant lifetime. The clear grant is returned
	// once and only its sha256 is stored.
	LocalNetGrantTTL = 10 * time.Minute
)

func localNetFromInput(in CreateSandboxInput) (bool, string) {
	if in.LocalNet != nil && *in.LocalNet {
		return true, LocalNetPending
	}
	return false, LocalNetOff
}

// withdrawLocalNetFields sinks a live tunnel. It does not clear LocalNet:
// the flag stays so a later reconcile cannot treat the sandbox as public egress.
func withdrawLocalNetFields(sb *Sandbox) {
	if sb.LocalNetState == "" {
		sb.LocalNetState = LocalNetOff
	}
	if !sb.LocalNet {
		sb.LocalNetState = LocalNetOff
		sb.LocalNetClientPublic = ""
		sb.LocalNetGrantHash = ""
		sb.LocalNetGrantExpiresAt = nil
		return
	}
	sb.LocalNetState = LocalNetWithdrawn
	sb.LocalNetClientPublic = ""
	sb.LocalNetGrantHash = ""
	sb.LocalNetGrantExpiresAt = nil
}

func localNetTerminal(state SandboxState) bool {
	switch state {
	case SandboxStopped, SandboxStopping, SandboxFailed:
		return true
	default:
		return false
	}
}

func newLocalNetGrant() (clear string, hash string, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", "", err
	}
	clear = hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(clear))
	return clear, hex.EncodeToString(sum[:]), nil
}

func hashLocalNetGrant(clear string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(clear)))
	return hex.EncodeToString(sum[:])
}

// ValidateWGPublicKey accepts a standard-base64 WireGuard/X25519 public key.
func ValidateWGPublicKey(s string) error {
	s = strings.TrimSpace(s)
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != 32 {
		return fmt.Errorf("%w: client_public_key must be a 32-byte WireGuard public key (standard base64)", ErrInvalidInput)
	}
	return nil
}
