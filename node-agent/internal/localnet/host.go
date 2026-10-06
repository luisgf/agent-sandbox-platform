package localnet

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// NodeKeyer is implemented by Host. The reconciler publishes the public key.
type NodeKeyer interface {
	EnsureNodeKey(sandboxID string) (public string, listenPort int, keyPath string, err error)
}

// Host runs ip(8) and wg(8) from PATH. Unit tests put scripts on PATH.
// It never changes the host main default route.
type Host struct {
	KeyDir string

	mu    sync.Mutex
	plans map[string]Plan
}

func NewHost(keyDir string) *Host {
	if strings.TrimSpace(keyDir) == "" {
		keyDir = "/var/lib/asp/local-net"
	}
	return &Host{KeyDir: keyDir, plans: map[string]Plan{}}
}

func (h *Host) keyPath(sandboxID string) string {
	id := strings.TrimSpace(sandboxID)
	id = strings.ReplaceAll(id, string(os.PathSeparator), "_")
	return filepath.Join(h.KeyDir, id+".key")
}

// EnsureNodeKey creates the node private key (mode 0600) if it does not exist.
func (h *Host) EnsureNodeKey(sandboxID string) (string, int, string, error) {
	if h == nil {
		return "", 0, "", fmt.Errorf("localnet host is nil")
	}
	if strings.TrimSpace(sandboxID) == "" {
		return "", 0, "", fmt.Errorf("sandbox id required")
	}
	path := h.keyPath(sandboxID)
	priv, err := readKey(path)
	if err != nil {
		if !os.IsNotExist(err) && !strings.Contains(err.Error(), "empty key") {
			// fall through to regenerate on empty; other read errors regenerate only if not exist
		}
		if os.IsNotExist(err) || strings.Contains(err.Error(), "empty key") {
			var pub string
			priv, pub, err = generateKey()
			if err != nil {
				return "", 0, "", err
			}
			if err := writeKey(path, priv); err != nil {
				return "", 0, "", err
			}
			return pub, ListenPort(sandboxID), path, nil
		}
		return "", 0, "", err
	}
	pub, err := publicFromPrivate(priv)
	if err != nil {
		return "", 0, "", err
	}
	return pub, ListenPort(sandboxID), path, nil
}

func (h *Host) Apply(p Plan) error {
	if h == nil {
		return fmt.Errorf("localnet host is nil")
	}
	if p.SandboxID == "" {
		return fmt.Errorf("sandbox id required")
	}
	if p.Kind != KindPublic && p.UsePublicProxy {
		return fmt.Errorf("refusing public proxy fallback for %s", p.SandboxID)
	}
	if p.Kind == KindPublic {
		h.mu.Lock()
		if h.plans == nil {
			h.plans = map[string]Plan{}
		}
		h.plans[p.SandboxID] = p
		h.mu.Unlock()
		return nil
	}
	if p.TableID < 10000 || p.TableID >= 30000 {
		return fmt.Errorf("refusing routing table %d (host main table is out of range)", p.TableID)
	}
	if p.Kind == KindTunnel {
		if strings.TrimSpace(p.PeerPublic) == "" {
			return fmt.Errorf("tunnel peer public key required")
		}
		pub, _, path, err := h.EnsureNodeKey(p.SandboxID)
		if err != nil {
			return err
		}
		p.KeyPath = path
		_ = pub
	}
	cmds := Argv(p)
	// Reconcile runs every couple of seconds. Deleting a live tunnel device
	// drops the handshake, so the laptop can never finish a TCP exchange.
	// Blackhole/withdraw still deletes. A missing device takes the full recipe.
	if p.Kind == KindTunnel && ifaceUp(p.Iface) {
		kept := make([]Cmd, 0, len(cmds))
		for _, c := range cmds {
			if c.Name == "ip" && len(c.Args) >= 2 && c.Args[0] == "link" && c.Args[1] == "delete" {
				continue
			}
			kept = append(kept, c)
		}
		cmds = kept
	}
	if HijacksHost(cmds) {
		return fmt.Errorf("refusing local-net recipe that hijacks the host default or the public proxy")
	}
	for _, c := range cmds {
		if err := runCmd(c, ignoreMissing(c)); err != nil {
			return fmt.Errorf("local-net %s: %w", p.SandboxID, err)
		}
	}
	if p.Kind == KindTunnel {
		if err := ensureTapWGForward(p.Tap, p.Iface); err != nil {
			return fmt.Errorf("local-net %s: %w", p.SandboxID, err)
		}
	} else if p.Tap != "" && p.Iface != "" {
		removeTapWGForward(p.Tap, p.Iface)
	}
	// Local-net is all-or-nothing, including the blackhole while the tunnel
	// is down. Return in prerouting so 80/443/DNS are not redirected at the
	// node proxy. KindPublic never reaches here.
	if p.BlockNodeProxy {
		if err := ensureLocalNetExempt(p.Tap); err != nil {
			return fmt.Errorf("local-net %s: %w", p.SandboxID, err)
		}
	}
	h.mu.Lock()
	if h.plans == nil {
		h.plans = map[string]Plan{}
	}
	h.plans[p.SandboxID] = p
	h.mu.Unlock()
	return nil
}

func (h *Host) Clear(sandboxID string) error {
	if h == nil || strings.TrimSpace(sandboxID) == "" {
		return nil
	}
	p := Decide(sandboxID, "", true, "withdrawn")
	for _, c := range ClearArgv(p) {
		if HijacksHost([]Cmd{c}) {
			return fmt.Errorf("refusing clear recipe that hijacks the host")
		}
		if err := runCmd(c, true); err != nil {
			return err
		}
	}
	removeTapWGForward(p.Tap, p.Iface)
	removeLocalNetExempt(p.Tap)
	_ = os.Remove(h.keyPath(sandboxID))
	h.mu.Lock()
	delete(h.plans, sandboxID)
	h.mu.Unlock()
	return nil
}

// Healthy reports whether the device of the applied plan still exists: one
// `ip link show` for a tunnel. Other plans have no device to lose.
func (h *Host) Healthy(sandboxID string) bool {
	p, ok := h.Current(sandboxID)
	if !ok {
		return false
	}
	if p.Kind != KindTunnel {
		return true
	}
	return ifaceUp(p.Iface)
}

func (h *Host) Current(sandboxID string) (Plan, bool) {
	if h == nil {
		return Plan{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.plans[sandboxID]
	return p, ok
}

func ignoreMissing(c Cmd) bool {
	if c.Name != "ip" || len(c.Args) < 2 {
		return false
	}
	if c.Args[0] == "link" && c.Args[1] == "delete" {
		return true
	}
	// rule del on withdraw runs even when the return rule was never added.
	return c.Args[0] == "rule" && c.Args[1] == "del"
}

func runCmd(c Cmd, soft bool) error {
	cmd := exec.Command(c.Name, c.Args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	if soft && (strings.Contains(msg, "Cannot find device") ||
		strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "No such device") ||
		strings.Contains(msg, "No such file") ||
		strings.Contains(err.Error(), "Cannot find device")) {
		return nil
	}
	if strings.Contains(msg, "File exists") || strings.Contains(msg, "RTNETLINK answers: File exists") {
		return nil
	}
	if msg == "" {
		msg = err.Error()
	}
	return fmt.Errorf("%s %s: %s", c.Name, strings.Join(c.Args, " "), msg)
}

func ifaceUp(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	cmd := exec.Command("ip", "link", "show", "dev", name)
	return cmd.Run() == nil
}

// ensureTapWGForward lets this sandbox's TAP reach its WireGuard device and
// the reverse path back. Docker's FORWARD policy is drop; these two rules are
// the only exception and they name this session's interfaces. Nothing else
// in FORWARD is changed. Missing iptables (unit tests) is a no-op.
func ensureTapWGForward(tap, wg string) error {
	if _, err := exec.LookPath("iptables"); err != nil {
		return nil
	}
	for _, spec := range [][]string{
		{"-i", tap, "-o", wg, "-j", "ACCEPT"},
		{"-i", wg, "-o", tap, "-j", "ACCEPT"},
	} {
		if iptablesCheck(spec) {
			continue
		}
		if err := iptablesRun(append([]string{"-I", "FORWARD", "1"}, spec...)); err != nil {
			return err
		}
	}
	return nil
}

func removeTapWGForward(tap, wg string) {
	if _, err := exec.LookPath("iptables"); err != nil {
		return
	}
	for _, spec := range [][]string{
		{"-i", tap, "-o", wg, "-j", "ACCEPT"},
		{"-i", wg, "-o", tap, "-j", "ACCEPT"},
	} {
		_ = iptablesRun(append([]string{"-D", "FORWARD"}, spec...))
	}
}

func iptablesCheck(spec []string) bool {
	cmd := exec.Command("iptables", append([]string{"-C", "FORWARD"}, spec...)...)
	return cmd.Run() == nil
}

func iptablesRun(args []string) error {
	cmd := exec.Command("iptables", args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		msg = err.Error()
	}
	return fmt.Errorf("iptables %s: %s", strings.Join(args, " "), msg)
}
