// Package localnet decides per-sandbox egress for the on-demand full tunnel
// (ADR-0010). When local_net is on, the public proxy path is not installed.
// Disconnect is a blackhole, never a silent fallback.
package localnet

import "strings"

// Kind is the egress plan for one sandbox.
type Kind string

const (
	// KindPublic is ADR-0002: node proxy, DNS sink, nft asp_egress.
	KindPublic Kind = "public"
	// KindBlackhole sinks 0.0.0.0/0 (and ::/0) for this session only.
	KindBlackhole Kind = "blackhole"
	// KindTunnel sends that default through the local-agent tunnel.
	KindTunnel Kind = "tunnel"
)

// Plan is what the node should install. It is not a kernel guarantee:
// CI records the plan. Host wireguard tools are required for a real device.
type Plan struct {
	SandboxID string
	OwnerSub  string
	Kind      Kind
	Iface     string
	Table     string
	// UsePublicProxy is true only for KindPublic.
	UsePublicProxy bool
	// BlockNodeProxy rejects this TAP talking to :8888 / the DNS sink.
	BlockNodeProxy bool
}

// Decide returns the egress plan. local_net false (or state off) keeps the
// public path. Any other state with the flag on refuses the public proxy.
func Decide(sandboxID, ownerSub string, localNet bool, state string) Plan {
	short := sandboxID
	if len(short) > 8 {
		short = short[:8]
	}
	if short == "" {
		short = "sandbox"
	}
	state = strings.TrimSpace(state)
	if !localNet || state == "off" {
		return Plan{
			SandboxID:      sandboxID,
			OwnerSub:       ownerSub,
			Kind:           KindPublic,
			UsePublicProxy: true,
		}
	}
	p := Plan{
		SandboxID:      sandboxID,
		OwnerSub:       ownerSub,
		Iface:          "wg-asp-" + short,
		Table:          "aspln-" + short,
		UsePublicProxy: false,
		BlockNodeProxy: true,
	}
	if state == "up" {
		p.Kind = KindTunnel
		return p
	}
	// pending, withdrawn, empty, unknown: blackhole. Never KindPublic.
	p.Kind = KindBlackhole
	return p
}

// Commands is the host recipe for a non-public plan. It never redirects to
// the node proxy (:8888) or asp_egress. KindPublic returns nil: that path
// is the existing nft/proxy setup, untouched.
// These commands are not executed unless a future host applier is enabled;
// tests assert the recipe itself has no public fallback.
func Commands(p Plan) []string {
	switch p.Kind {
	case KindPublic:
		return nil
	case KindTunnel:
		return []string{
			"ip route replace default dev " + p.Iface + " table " + p.Table,
			"ip route replace ::/0 dev " + p.Iface + " table " + p.Table,
			"ip rule add from all iif " + p.Iface + " lookup " + p.Table,
		}
	default:
		return []string{
			"ip route replace blackhole 0.0.0.0/0 table " + p.Table,
			"ip route replace blackhole ::/0 table " + p.Table,
		}
	}
}

// UsesPublicProxy reports whether this plan would send the sandbox default
// through the node forward proxy. False for every local_net plan.
func (p Plan) UsesPublicProxy() bool { return p.UsePublicProxy }
