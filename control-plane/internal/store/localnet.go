package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash/fnv"
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

func fnv32a(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// LocalNetListenPort is the UDP port the node device binds. Stable per sandbox.
func LocalNetListenPort(id string) int {
	return 47000 + int(fnv32a("udp"+LocalNetShortID(id))%8000)
}

// LocalNetTableID is a policy-routing table that is never the main table.
func LocalNetTableID(id string) int {
	return 10000 + int(fnv32a(LocalNetShortID(id))%20000)
}

// LocalNetTunnel returns the node and client interface CIDRs inside 10.188.0.0/16.
// The client address is the second usable host of a /30. Neither is installed on
// the host main default route.
func LocalNetTunnel(id string) (nodeCIDR, clientCIDR string) {
	slot := fnv32a("tun"+LocalNetShortID(id)) % 16384
	base := slot * 4
	a := (base >> 8) & 0xff
	b := base & 0xff
	nodeCIDR = fmt.Sprintf("10.188.%d.%d/30", a, b+1)
	clientCIDR = fmt.Sprintf("10.188.%d.%d/30", a, b+2)
	return nodeCIDR, clientCIDR
}

// LocalNetIface is the per-sandbox WireGuard device name (15 chars max).
func LocalNetIface(id string) string { return "wg-asp-" + LocalNetShortID(id) }

// LocalNetTap is the TAP whose ingress selects this sandbox's routing table.
func LocalNetTap(id string) string { return "asp-" + LocalNetShortID(id) }
