// Package nftredirect applies guest TCP HTTP(S) + DNS redirect/drop via nftables
// (Fase 2e anti-bypass). SoftFail continues without root/nft; Enforce fails hard.
package nftredirect

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

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
	ScriptPath  string // path to scripts/nftables-egress-redirect.sh
	Mode        Mode   // soft | enforce
	Logger      *slog.Logger
}

// SoftFail is true when Mode is soft (back-compat helper).
func (c Config) SoftFail() bool {
	return c.Mode != ModeEnforce
}

// Apply runs the nftables script (apply). Soft mode returns nil on permission errors.
func Apply(cfg Config) error {
	cfg = normalize(cfg)
	script, err := locateScript(cfg)
	if err != nil {
		if cfg.SoftFail() {
			cfg.Logger.Warn("egress-nft-redirect SoftFail", "error", err)
			return nil
		}
		return err
	}

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
	cmd := exec.Command(script, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		if cfg.SoftFail() {
			cfg.Logger.Warn("egress-nft-redirect SoftFail (need root/nft?)", "error", msg, "script", script, "mode", cfg.Mode)
			return nil
		}
		return fmt.Errorf("nft redirect enforce: %s", msg)
	}
	cfg.Logger.Info("egress-nft-redirect applied",
		"script", script,
		"subnet", cfg.GuestSubnet,
		"proxy_port", cfg.ProxyPort,
		"dns_sink_port", cfg.DNSSinkPort,
		"dns_action", cfg.DNSAction,
		"http_ports", cfg.HTTPPorts,
		"table", cfg.Table,
		"mode", cfg.Mode,
		"output", strings.TrimSpace(string(out)),
	)
	return nil
}

// Flush removes the namespaced table (idempotent).
func Flush(cfg Config) error {
	cfg = normalize(cfg)
	script, err := locateScript(cfg)
	if err != nil {
		if cfg.SoftFail() {
			cfg.Logger.Warn("egress-nft-flush SoftFail", "error", err)
			return nil
		}
		return err
	}
	cmd := exec.Command(script, "flush", "--mode", string(cfg.Mode), "--table", cfg.Table)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		if cfg.SoftFail() {
			cfg.Logger.Warn("egress-nft-flush SoftFail", "error", msg)
			return nil
		}
		return fmt.Errorf("nft flush enforce: %s", msg)
	}
	cfg.Logger.Info("egress-nft-redirect flushed", "table", cfg.Table, "output", strings.TrimSpace(string(out)))
	return nil
}

// DryRun returns the nftables ruleset text the script would apply (no root needed).
func DryRun(cfg Config) (string, error) {
	cfg = normalize(cfg)
	script, err := locateScript(cfg)
	if err != nil {
		return "", err
	}
	args := []string{
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
	}
	cmd := exec.Command(script, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
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

func locateScript(cfg Config) (string, error) {
	if cfg.ScriptPath != "" {
		if st, err := os.Stat(cfg.ScriptPath); err == nil && !st.IsDir() {
			return cfg.ScriptPath, nil
		}
		return "", fmt.Errorf("nftables script not found: %s", cfg.ScriptPath)
	}
	if s := getenv("ASP_NFT_SCRIPT", ""); s != "" {
		if st, err := os.Stat(s); err == nil && !st.IsDir() {
			return s, nil
		}
	}
	candidates := []string{
		"scripts/nftables-egress-redirect.sh",
		"/usr/local/share/asp/scripts/nftables-egress-redirect.sh",
	}
	if root := os.Getenv("ASP_ROOT"); root != "" {
		candidates = append(candidates, filepath.Join(root, "scripts/nftables-egress-redirect.sh"))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("nftables script not found")
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
