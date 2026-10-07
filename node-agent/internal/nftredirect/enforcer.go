// Package nftredirect applies guest TCP HTTP(S) + DNS redirect/drop via nftables
// (Fase 2e anti-bypass). SoftFail continues without root/nft; Enforce fails hard.
package nftredirect

import (
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// script is the nftables script this binary applies. It is embedded so that the
// node-agent needs no file on the host and the version that runs is the version
// that was tested with it; ScriptPath or ASP_NFT_SCRIPT name an operator's own.
//
//go:embed nftables-egress-redirect.sh
var script string

// Mode controls failure behavior when rules cannot be applied.
type Mode string

const (
	// ModeSoft logs and continues when root/nft/CAP_NET_ADMIN are missing.
	ModeSoft Mode = "soft"
	// ModeEnforce returns an error when rules cannot be applied.
	ModeEnforce Mode = "enforce"
)

// Config for the redirect enforcer.
type Config struct {
	GuestSubnet string // e.g. 10.200.0.0/16
	ProxyIP     string // host TAP IP (informational for script)
	ProxyPort   int    // e.g. 8888
	DNSSinkIP   string // default = ProxyIP
	DNSSinkPort int    // e.g. 5353 (node-agent --egress-dns-sink)
	HTTPPorts   string // comma-separated, default "80,443"
	DNSAction   string // "redirect" | "drop"
	Table       string // default asp_egress
	ScriptPath  string // an operator's own copy of the script; default: the one built into this binary
	Mode        Mode   // soft | enforce
	Logger      *slog.Logger
}

// SoftFail is true when Mode is soft (back-compat helper).
func (c Config) SoftFail() bool {
	return c.Mode != ModeEnforce
}

// Apply runs the nftables script (apply). Soft mode returns nil on permission errors.
func Apply(cfg Config) error {
	_, err := ApplyChecked(cfg)
	return err
}

// ApplyChecked is Apply that also says whether the rules are in place. In soft
// mode a node that cannot apply them (no root, no nft) goes on without, and a
// caller that reports enforcement must not count that as applied.
func ApplyChecked(cfg Config) (applied bool, err error) {
	cfg = normalize(cfg)
	args := []string{
		"apply",
		"--mode", string(cfg.Mode),
		"--guest-subnet", cfg.GuestSubnet,
		"--proxy-ip", cfg.ProxyIP,
		"--proxy-port", strconv.Itoa(cfg.ProxyPort),
		"--dns-sink-ip", cfg.DNSSinkIP,
		"--dns-sink-port", strconv.Itoa(cfg.DNSSinkPort),
		"--http-ports", cfg.HTTPPorts,
		"--dns-action", cfg.DNSAction,
		"--table", cfg.Table,
	}
	out, via, err := run(cfg, args...)
	if err != nil {
		msg := strings.TrimSpace(out)
		if msg == "" {
			msg = err.Error()
		}
		if cfg.SoftFail() {
			cfg.Logger.Warn("egress-nft-redirect SoftFail (need root/nft?): the guests' egress is NOT enforced", "error", msg, "script", via, "mode", cfg.Mode)
			return false, nil
		}
		return false, fmt.Errorf("nft redirect enforce: %s", msg)
	}
	applied = strings.Contains(out, "applied table")
	if !applied {
		// The script exits 0 in soft mode when it cannot apply the rules.
		cfg.Logger.Warn("egress-nft-redirect SoftFail: the guests' egress is NOT enforced", "output", strings.TrimSpace(out), "script", via, "mode", cfg.Mode)
		return false, nil
	}
	cfg.Logger.Info("egress-nft-redirect applied",
		"script", via,
		"subnet", cfg.GuestSubnet,
		"proxy_port", cfg.ProxyPort,
		"dns_sink_port", cfg.DNSSinkPort,
		"dns_action", cfg.DNSAction,
		"http_ports", cfg.HTTPPorts,
		"table", cfg.Table,
		"mode", cfg.Mode,
		"output", strings.TrimSpace(out),
	)
	return true, nil
}

// Flush removes the namespaced table (idempotent).
func Flush(cfg Config) error {
	cfg = normalize(cfg)
	out, _, err := run(cfg, "flush", "--mode", string(cfg.Mode), "--table", cfg.Table)
	if err != nil {
		msg := strings.TrimSpace(out)
		if msg == "" {
			msg = err.Error()
		}
		if cfg.SoftFail() {
			cfg.Logger.Warn("egress-nft-flush SoftFail", "error", msg)
			return nil
		}
		return fmt.Errorf("nft flush enforce: %s", msg)
	}
	cfg.Logger.Info("egress-nft-redirect flushed", "table", cfg.Table, "output", strings.TrimSpace(out))
	return nil
}

// DryRun returns the nftables ruleset text the script would apply (no root needed).
func DryRun(cfg Config) (string, error) {
	cfg = normalize(cfg)
	out, _, err := run(cfg,
		"dry-run",
		"--mode", string(cfg.Mode),
		"--guest-subnet", cfg.GuestSubnet,
		"--proxy-ip", cfg.ProxyIP,
		"--proxy-port", strconv.Itoa(cfg.ProxyPort),
		"--dns-sink-ip", cfg.DNSSinkIP,
		"--dns-sink-port", strconv.Itoa(cfg.DNSSinkPort),
		"--http-ports", cfg.HTTPPorts,
		"--dns-action", cfg.DNSAction,
		"--table", cfg.Table,
	)
	return out, err
}

// run executes the script with args and returns its combined output and where it
// ran from: the operator's own copy (cfg.ScriptPath, else ASP_NFT_SCRIPT), else
// the one embedded in this binary, fed to bash on stdin. An operator's path that
// does not exist is an error: a node asked to use a script must not quietly run
// another.
func run(cfg Config, args ...string) (out, via string, err error) {
	path := cfg.ScriptPath
	if path == "" {
		path = os.Getenv("ASP_NFT_SCRIPT")
	}
	var cmd *exec.Cmd
	if path != "" {
		if st, serr := os.Stat(path); serr != nil || st.IsDir() {
			return "", path, fmt.Errorf("nftables script not found: %s", path)
		}
		cmd, via = exec.Command(path, args...), path
	} else {
		cmd, via = exec.Command("bash", append([]string{"-s", "--"}, args...)...), "embedded"
		cmd.Stdin = strings.NewReader(script)
	}
	b, err := cmd.CombinedOutput()
	return string(b), via, err
}

func normalize(cfg Config) Config {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeSoft
		if strings.EqualFold(os.Getenv("ASP_NFT_EGRESS_MODE"), "enforce") {
			cfg.Mode = ModeEnforce
		}
	}
	cfg.Mode = Mode(strings.ToLower(string(cfg.Mode)))
	if cfg.GuestSubnet == "" {
		cfg.GuestSubnet = getenv("ASP_GUEST_SUBNET", "10.200.0.0/16")
	}
	if cfg.ProxyIP == "" {
		cfg.ProxyIP = getenv("ASP_EGRESS_PROXY_IP", "10.200.0.1")
	}
	if cfg.ProxyPort <= 0 {
		cfg.ProxyPort = 8888
		if p := strings.TrimSpace(os.Getenv("ASP_EGRESS_PROXY_PORT")); p != "" {
			if n, err := strconv.Atoi(p); err == nil {
				cfg.ProxyPort = n
			}
		}
	}
	if cfg.DNSSinkPort <= 0 {
		cfg.DNSSinkPort = 5353
		if p := strings.TrimSpace(os.Getenv("ASP_EGRESS_DNS_SINK_PORT")); p != "" {
			if n, err := strconv.Atoi(p); err == nil {
				cfg.DNSSinkPort = n
			}
		}
	}
	if cfg.DNSSinkIP == "" {
		cfg.DNSSinkIP = getenv("ASP_EGRESS_DNS_SINK_IP", cfg.ProxyIP)
	}
	if cfg.HTTPPorts == "" {
		cfg.HTTPPorts = getenv("ASP_NFT_HTTP_PORTS", "80,443")
	}
	if cfg.DNSAction == "" {
		cfg.DNSAction = getenv("ASP_NFT_DNS_ACTION", "redirect")
	}
	cfg.DNSAction = strings.ToLower(cfg.DNSAction)
	if cfg.Table == "" {
		cfg.Table = getenv("ASP_NFT_TABLE", "asp_egress")
	}
	return cfg
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
