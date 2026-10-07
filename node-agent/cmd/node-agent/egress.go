package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

// optBool is a bool flag that remembers whether anyone set it, so that "not set"
// can mean something other than false.
type optBool struct{ set, value bool }

func (b *optBool) String() string {
	if b == nil || !b.set {
		return "auto"
	}
	return strconv.FormatBool(b.value)
}

func (b *optBool) Set(v string) error {
	parsed, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return err
	}
	b.set, b.value = true, parsed
	return nil
}

// IsBoolFlag lets the flag be given bare: --egress-nft-redirect means true.
func (b *optBool) IsBoolFlag() bool { return true }

// envOptBool reads the first of keys that is set. 1/true/yes/on and 0/false/no/off
// are understood; anything else counts as not set.
func envOptBool(keys ...string) optBool {
	for _, k := range keys {
		switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
		case "1", "true", "yes", "on":
			return optBool{set: true, value: true}
		case "0", "false", "no", "off":
			return optBool{set: true, value: false}
		}
	}
	return optBool{}
}

const (
	nftModeEnforce = "enforce"
	nftModeSoft    = "soft"

	// defaultDNSSink is where the DNS sink listens when the nft redirect sends
	// guest DNS to it and the operator did not say where.
	defaultDNSSink = ":5353"
)

// resolveEgress fills in what the egress flags leave open, and says what it did.
//
// Deny by default has to hold on a node that was given the proxy and nothing else:
//   - with an egress proxy and real VMs the nft redirect is on, so HTTP_PROXY is
//     not something a guest can ignore; --egress-nft-redirect=false turns it off,
//     loudly;
//   - the redirect's mode is enforce: a node that cannot apply the rules does not
//     start. soft is for --dry-run (no VMs, no root) and for an operator who asks.
//
// It returns an error for a combination that cannot work.
func resolveEgress(cfg *config) error {
	if cfg.egressNFTRedirect.set {
		cfg.EgressNFTRedirect = cfg.egressNFTRedirect.value
	} else {
		cfg.EgressNFTRedirect = cfg.EgressProxyListen != "" && !cfg.DryRun
	}

	mode := strings.ToLower(strings.TrimSpace(cfg.NFTEgressMode))
	switch mode {
	case "":
		mode = nftModeEnforce
		if cfg.DryRun {
			mode = nftModeSoft
		}
	case nftModeSoft, nftModeEnforce:
	default:
		return fmt.Errorf("--nft-egress-mode takes %s or %s, not %q", nftModeSoft, nftModeEnforce, cfg.NFTEgressMode)
	}
	cfg.NFTEgressMode = mode

	if cfg.EgressNFTRedirect && cfg.EgressProxyListen == "" {
		return errors.New("--egress-nft-redirect sends guest traffic to the egress proxy, and --egress-proxy-listen is not set")
	}
	if cfg.EgressNFTRedirect && strings.EqualFold(cfg.NFTDNSAction, "redirect") && cfg.EgressDNSSink == "" {
		// Redirecting DNS to a port nobody listens on would leave every guest
		// without name resolution.
		cfg.EgressDNSSink = defaultDNSSink
		slog.Info("the nft redirect sends guest DNS to the sink: starting it", "egress_dns_sink", cfg.EgressDNSSink)
	}

	if cfg.DryRun {
		return nil
	}
	switch {
	case cfg.EgressNFTRedirect && mode == nftModeSoft:
		slog.Warn("--nft-egress-mode=soft: if the nftables rules cannot be applied (no root, no nft) this node starts anyway and its guests are NOT forced through the egress proxy")
	case cfg.EgressProxyListen != "" && !cfg.EgressNFTRedirect:
		slog.Warn("the egress proxy is set but the nft redirect is off: HTTP_PROXY is voluntary, and a guest can reach the network directly wherever the host forwards. Remove --egress-nft-redirect=false to enforce it")
	case cfg.TapAuto && cfg.EgressProxyListen == "":
		slog.Warn("no egress proxy (--egress-proxy-listen): guests are not subject to their tenant's egress policy, and `asp node list` shows this node as not enforcing")
	}
	return nil
}

// listenPort is the port of a listen address (":8888", "0.0.0.0:8888"), or 0.
func listenPort(addr string) int {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return 0
	}
	p, err := strconv.Atoi(addr[i+1:])
	if err != nil || p <= 0 {
		return 0
	}
	return p
}

// guestProxyPort is where guests find the egress proxy on their gateway: the port
// of --egress-proxy-listen, or 0 when the node has no proxy.
func guestProxyPort(cfg config) int {
	if cfg.EgressProxyListen == "" {
		return 0
	}
	return listenPort(cfg.EgressProxyListen)
}

// guestDNS says whether a guest should be given its gateway as resolver: when the
// DNS sink is running and the guest's queries reach it, through the nft redirect of
// port 53 or because the sink listens on 53 itself. Otherwise nothing answers there.
func guestDNS(cfg config) bool {
	if cfg.EgressDNSSink == "" {
		return false
	}
	if listenPort(cfg.EgressDNSSink) == 53 {
		return true
	}
	return cfg.EgressNFTRedirect && strings.EqualFold(cfg.NFTDNSAction, "redirect")
}

// egressEnforced is what the node tells the control plane about its guests'
// egress: true only when the proxy listens and the nft rules that force every
// guest through it are in place in enforce mode. A rule set that soft mode could
// not apply is not enforcement.
func egressEnforced(cfg config, rulesApplied bool) bool {
	return cfg.EgressProxyListen != "" && rulesApplied && cfg.NFTEgressMode == nftModeEnforce
}
