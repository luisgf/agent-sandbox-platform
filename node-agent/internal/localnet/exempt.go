package localnet

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// nftEgressTable is the static guest redirect table (scripts/nftables-egress-redirect.sh).
// A local-net session must not hit its prerouting redirects: that would send
// the session at the node proxy, which is the silent fallback ADR-0010 forbids.
const nftEgressTable = "asp_egress"

var nftHandleRe = regexp.MustCompile(`# handle (\d+)`)

// ExemptSpec is the prerouting nat returns for one TAP. They are inserted at
// the head of the chain so they match before the subnet-wide redirect of
// TCP 80/443 and DNS. The static ruleset cannot name the TAP; the node-agent
// installs these while the session is local-net and deletes them on clear.
func ExemptSpec(tap string) []Cmd {
	tap = strings.TrimSpace(tap)
	if tap == "" {
		return nil
	}
	return []Cmd{
		{Name: "nft", Args: []string{
			"insert", "rule", "ip", nftEgressTable, "prerouting",
			"iifname", tap,
			"tcp", "dport", "{ 80, 443 }",
			"return",
			"comment", exemptComment(tap, "http"),
		}},
		{Name: "nft", Args: []string{
			"insert", "rule", "ip", nftEgressTable, "prerouting",
			"iifname", tap,
			"udp", "dport", "53",
			"return",
			"comment", exemptComment(tap, "udp53"),
		}},
		{Name: "nft", Args: []string{
			"insert", "rule", "ip", nftEgressTable, "prerouting",
			"iifname", tap,
			"tcp", "dport", "53",
			"return",
			"comment", exemptComment(tap, "tcp53"),
		}},
	}
}

func exemptComment(tap, kind string) string {
	return "asp_ln_exempt_" + tap + "_" + kind
}

// ensureLocalNetExempt inserts the return rules if they are not already there.
// Missing nft (unit tests, hosts without nftables) is a no-op. A present nft
// that cannot see the chain is an error: applying local-net without the
// exception would fall through to the node proxy.
func ensureLocalNetExempt(tap string) error {
	tap = strings.TrimSpace(tap)
	if tap == "" {
		return nil
	}
	if _, err := exec.LookPath("nft"); err != nil {
		return nil
	}
	out, err := exec.Command("nft", "list", "chain", "ip", nftEgressTable, "prerouting").CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("nft list prerouting: %s", msg)
	}
	listed := string(out)
	for _, c := range ExemptSpec(tap) {
		comment := flagValue(c.Args, "comment")
		if comment != "" && strings.Contains(listed, comment) {
			continue
		}
		if err := runCmd(c, false); err != nil {
			return err
		}
	}
	return nil
}

// removeLocalNetExempt deletes every return rule installed for this TAP.
// Missing nft or a missing chain is a no-op so clear still succeeds.
func removeLocalNetExempt(tap string) {
	tap = strings.TrimSpace(tap)
	if tap == "" {
		return
	}
	if _, err := exec.LookPath("nft"); err != nil {
		return
	}
	out, err := exec.Command("nft", "-a", "list", "chain", "ip", nftEgressTable, "prerouting").CombinedOutput()
	if err != nil {
		return
	}
	prefix := "asp_ln_exempt_" + tap + "_"
	var handles []string
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, prefix) {
			continue
		}
		m := nftHandleRe.FindStringSubmatch(line)
		if m != nil {
			handles = append(handles, m[1])
		}
	}
	for _, h := range handles {
		_ = runCmd(Cmd{Name: "nft", Args: []string{
			"delete", "rule", "ip", nftEgressTable, "prerouting", "handle", h,
		}}, true)
	}
}
