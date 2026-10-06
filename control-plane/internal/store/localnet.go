package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/netip"
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

// LocalNetShortID is the 8-char suffix used for iface, table and tunnel addresses.
// Keep in sync with node-agent/internal/localnet and cli/internal/localnet.
func LocalNetShortID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > 8 {
		id = id[:8]
	}
	if id == "" {
		id = "sandbox"
	}
	return id
}

// LocalNetTunnel is what the node allocates for a session and publishes with
// its public key: the UDP port of its WireGuard device and the two ends of a
// /30 in 10.188.0.0/16. The grant hands them to the laptop.
type LocalNetTunnel struct {
	ListenPort int
	NodeAddr   string // CIDR, e.g. 10.188.4.1/30
	ClientAddr string // CIDR in the same /30
}

// Validate checks the port and that both addresses are hosts of one /30 in
// 10.188.0.0/16.
func (t LocalNetTunnel) Validate() error {
	if t.ListenPort < 1 || t.ListenPort > 65535 {
		return fmt.Errorf("%w: listen_port must be 1-65535", ErrInvalidInput)
	}
	pool := netip.MustParsePrefix("10.188.0.0/16")
	node, err := netip.ParsePrefix(strings.TrimSpace(t.NodeAddr))
	if err != nil || node.Bits() != 30 || !pool.Contains(node.Addr()) {
		return fmt.Errorf("%w: node_tunnel_addr must be a /30 address in 10.188.0.0/16", ErrInvalidInput)
	}
	client, err := netip.ParsePrefix(strings.TrimSpace(t.ClientAddr))
	if err != nil || client.Bits() != 30 || client.Masked() != node.Masked() || client.Addr() == node.Addr() {
		return fmt.Errorf("%w: client_tunnel_addr must be the other host of the node's /30", ErrInvalidInput)
	}
	return nil
}

// LocalNetIface is the per-sandbox WireGuard device name (15 chars max).
func LocalNetIface(id string) string { return "wg-asp-" + LocalNetShortID(id) }

// LocalNetTap is the TAP whose ingress selects this sandbox's routing table.
func LocalNetTap(id string) string { return "asp-" + LocalNetShortID(id) }
