// Package tap creates and deletes host TAP devices for microVM networking.
package tap

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// DefaultHostCIDR is the host-side address Create applies when HostCIDR is
// empty. The reconciler gives each TAP its own /30 instead (see Slot): with
// one shared prefix, the host routes every guest's replies to a single TAP.
const DefaultHostCIDR = "10.200.0.1/24"

// Runner executes host commands (injectable for tests).
type Runner interface {
	Run(name string, args ...string) error
}

// ExecRunner uses os/exec.
type ExecRunner struct{}

func (ExecRunner) Run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w (%s)", name, args, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RecordingRunner records calls and returns InjectedErr (tests).
type RecordingRunner struct {
	mu          sync.Mutex // the reconciler runs several workers
	Calls       []string
	InjectedErr error
}

func (r *RecordingRunner) Run(name string, args ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Calls = append(r.Calls, name+" "+strings.Join(args, " "))
	return r.InjectedErr
}

// Manager creates/deletes TAP devices via ip(8).
type Manager struct {
	Runner   Runner
	HostCIDR string // default DefaultHostCIDR
	Logger   *slog.Logger
	// SoftFail logs errors instead of returning them. Only for dry-run: a real
	// VM must not boot without its TAP, or with someone else's.
	SoftFail bool
	// SysClassNet is where an existing device is detected (default /sys/class/net).
	SysClassNet string
}

// ErrDeviceExists means the TAP name is taken: another sandbox with the same
// short id, or a leftover the reaper could not remove.
var ErrDeviceExists = errors.New("network device already exists")

func (m *Manager) runner() Runner {
	if m.Runner != nil {
		return m.Runner
	}
	return ExecRunner{}
}

func (m *Manager) cidr() string {
	if m.HostCIDR != "" {
		return m.HostCIDR
	}
	return DefaultHostCIDR
}

func (m *Manager) sysClassNet() string {
	if m.SysClassNet != "" {
		return m.SysClassNet
	}
	return "/sys/class/net"
}

func (m *Manager) log() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

// DevicePrefix starts every sandbox TAP name (DeviceName).
const DevicePrefix = "asp-"

// DeviceName returns the TAP name for a sandbox (asp-{shortID}).
func DeviceName(sandboxID string) string {
	id := sandboxID
	if len(id) > 8 {
		id = id[:8]
	}
	return DevicePrefix + id
}

// Devices lists the TUN/TAP devices under sysClassNet (/sys/class/net) whose
// name starts with DevicePrefix. Only TUN/TAP devices have tun_flags, so other
// kinds of device that share the prefix are left out.
func Devices(sysClassNet string) ([]string, error) {
	entries, err := os.ReadDir(sysClassNet)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, DevicePrefix) {
			continue
		}
		if _, err := os.Stat(filepath.Join(sysClassNet, name, "tun_flags")); err != nil {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// Create brings up a TAP with the manager's host address.
func (m *Manager) Create(name string) error {
	return m.CreateWithCIDR(name, "")
}

// CreateWithCIDR brings up a TAP with hostCIDR on it (empty = HostCIDR).
func (m *Manager) CreateWithCIDR(name, hostCIDR string) error {
	return m.CreateOwned(name, hostCIDR, nil)
}

// CreateOwned brings up a TAP that the user with id *owner can open without any
// privilege: a VMM running as that user attaches to it (TUNSETIFF checks the
// owner of a persistent device). A nil owner leaves the device to root, as
// CreateWithCIDR does.
func (m *Manager) CreateOwned(name, hostCIDR string, owner *uint32) error {
	if hostCIDR == "" {
		hostCIDR = m.cidr()
	}
	if name == "" {
		return fmt.Errorf("tap name required")
	}
	// Never take over a device that is already there: ip tuntap add on an
	// existing TAP fails, but a soft failure would boot the VM on it.
	if _, err := os.Lstat(filepath.Join(m.sysClassNet(), name)); err == nil {
		err := fmt.Errorf("%w: %s", ErrDeviceExists, name)
		if m.SoftFail {
			m.log().Warn("tap create skipped (soft)", "tap", name, "error", err)
			return nil
		}
		return err
	}
	r := m.runner()
	add := []string{"tuntap", "add", "dev", name, "mode", "tap"}
	if owner != nil {
		add = append(add, "user", strconv.FormatUint(uint64(*owner), 10))
	}
	steps := []struct {
		bin  string
		args []string
	}{
		{"ip", add},
		{"ip", []string{"link", "set", name, "up"}},
		{"ip", []string{"addr", "add", hostCIDR, "dev", name}},
	}
	for i, step := range steps {
		if err := r.Run(step.bin, step.args...); err != nil {
			if m.SoftFail {
				m.log().Warn("tap create step failed (soft)", "tap", name, "cmd", step.bin, "args", step.args, "error", err)
				return nil
			}
			if i > 0 {
				// The device exists but is not usable: do not leave it behind.
				_ = r.Run("ip", "link", "delete", name)
			}
			return err
		}
	}
	m.log().Info("tap created", "tap", name, "cidr", hostCIDR)
	return nil
}

// Delete removes a TAP device (idempotent-ish: soft-fail on missing).
func (m *Manager) Delete(name string) error {
	if name == "" {
		return nil
	}
	err := m.runner().Run("ip", "link", "delete", name)
	if err != nil {
		if m.SoftFail {
			m.log().Warn("tap delete failed (soft)", "tap", name, "error", err)
			return nil
		}
		// Treat "Cannot find device" as success for stop paths.
		if strings.Contains(err.Error(), "Cannot find device") ||
			strings.Contains(err.Error(), "does not exist") {
			return nil
		}
		return err
	}
	m.log().Info("tap deleted", "tap", name)
	return nil
}
