// Package tap creates and deletes host TAP devices for microVM networking.
package tap

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// DefaultHostCIDR is the host-side address applied to each TAP when none is set.
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
	Calls       []string
	InjectedErr error
}

func (r *RecordingRunner) Run(name string, args ...string) error {
	r.Calls = append(r.Calls, name+" "+strings.Join(args, " "))
	return r.InjectedErr
}

// Manager creates/deletes TAP devices via ip(8).
type Manager struct {
	Runner   Runner
	HostCIDR string // default DefaultHostCIDR
	Logger   *slog.Logger
	// SoftFail logs permission/capability errors instead of failing Start.
	SoftFail bool
}

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

func (m *Manager) log() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

// DeviceName returns the TAP name for a sandbox (asp-{shortID}).
func DeviceName(sandboxID string) string {
	id := sandboxID
	if len(id) > 8 {
		id = id[:8]
	}
	return "asp-" + id
}

// Create brings up a TAP with optional host address.
func (m *Manager) Create(name string) error {
	if name == "" {
		return fmt.Errorf("tap name required")
	}
	r := m.runner()
	steps := []struct {
		bin  string
		args []string
	}{
		{"ip", []string{"tuntap", "add", "dev", name, "mode", "tap"}},
		{"ip", []string{"link", "set", name, "up"}},
		{"ip", []string{"addr", "add", m.cidr(), "dev", name}},
	}
	for _, step := range steps {
		if err := r.Run(step.bin, step.args...); err != nil {
			if m.SoftFail {
				m.log().Warn("tap create step failed (soft)", "tap", name, "cmd", step.bin, "args", step.args, "error", err)
				return nil
			}
			return err
		}
	}
	m.log().Info("tap created", "tap", name, "cidr", m.cidr())
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
