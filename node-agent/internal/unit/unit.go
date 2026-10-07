// Package unit runs a process as a transient systemd service of its own. The
// process gets a cgroup (shown by systemd-cgls, bounded by the limits of its
// Spec), its output goes to the journal, and it is not killed when the process
// that started it exits. The node-agent uses it to give every microVM its own
// resource limits instead of leaving it a child in the agent's cgroup.
package unit

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// Spec describes the service a process runs in.
type Spec struct {
	// Name is the unit name without the ".service" suffix.
	Name        string
	Slice       string
	Description string
	// MemoryMax is a hard limit in bytes. 0 sets none.
	MemoryMax int64
	// CPUQuota is a percentage of one CPU (100 is one full CPU). 0 sets none.
	CPUQuota int
	// TasksMax bounds processes and threads. 0 sets none.
	TasksMax int
	// Properties are more systemd properties, as "Key=value".
	Properties []string
}

// Args is the systemd-run command line that runs name args... as a service of
// its own and waits for it to end: systemd-run exits with the service, but the
// service does not end with systemd-run.
func (s Spec) Args(name string, args ...string) []string {
	a := []string{"--quiet", "--collect", "--wait", "--unit=" + s.Name}
	if s.Slice != "" {
		a = append(a, "--slice="+s.Slice)
	}
	if s.Description != "" {
		a = append(a, "--description="+s.Description)
	}
	prop := func(k, v string) { a = append(a, "--property="+k+"="+v) }
	if s.MemoryMax > 0 {
		prop("MemoryMax", strconv.FormatInt(s.MemoryMax, 10))
	}
	if s.CPUQuota > 0 {
		prop("CPUQuota", strconv.Itoa(s.CPUQuota)+"%")
	}
	if s.TasksMax > 0 {
		prop("TasksMax", strconv.Itoa(s.TasksMax))
	}
	for _, p := range s.Properties {
		a = append(a, "--property="+p)
	}
	a = append(a, "--", name)
	return append(a, args...)
}

// Launcher starts processes as transient units.
type Launcher struct {
	// SystemdRun and Systemctl are the executables. Empty means the one on PATH.
	SystemdRun string
	Systemctl  string
	// Command builds a command; tests substitute their own. Nil is exec.Command.
	Command func(name string, args ...string) *exec.Cmd
}

func (l Launcher) command(name string, args ...string) *exec.Cmd {
	if l.Command != nil {
		return l.Command(name, args...)
	}
	return exec.Command(name, args...)
}

func (l Launcher) systemdRun() string {
	if l.SystemdRun != "" {
		return l.SystemdRun
	}
	return "systemd-run"
}

func (l Launcher) systemctl() string {
	if l.Systemctl != "" {
		return l.Systemctl
	}
	return "systemctl"
}

// Start runs name args... in the unit spec describes. The returned Process waits
// for the unit to end and can kill it.
func (l Launcher) Start(spec Spec, name string, args ...string) (*Process, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return nil, errors.New("unit: a name is required")
	}
	cmd := l.command(l.systemdRun(), spec.Args(name, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("unit: start %s: %w", l.systemdRun(), err)
	}
	return &Process{cmd: cmd, l: l, unit: spec.Name + ".service", stderr: &stderr}, nil
}

// Process is a service started by Launcher.Start.
type Process struct {
	cmd    *exec.Cmd
	l      Launcher
	unit   string
	stderr *bytes.Buffer
}

// Unit is the full name of the service, with its suffix.
func (p *Process) Unit() string { return p.unit }

// Wait blocks until the service ends. It returns systemd-run's error, which
// carries the service's exit status, with what systemd-run printed.
func (p *Process) Wait() error {
	err := p.cmd.Wait()
	if err != nil {
		if msg := strings.TrimSpace(p.stderr.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
	}
	return err
}

// Kill sends SIGKILL to every process of the unit. A unit that is already gone
// is not an error.
func (p *Process) Kill() error {
	out, err := p.l.command(p.l.systemctl(), "kill", "--signal=SIGKILL", "--kill-whom=all", p.unit).CombinedOutput()
	if err != nil && !gone(string(out)) {
		return fmt.Errorf("unit: kill %s: %w: %s", p.unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// gone reports whether systemctl said the unit does not exist (anymore).
func gone(output string) bool {
	return strings.Contains(output, "not loaded") || strings.Contains(output, "not found") || strings.Contains(output, "No such unit")
}

// Pid is the main process of the unit, or 0 when it is not known (not started
// yet, or already gone).
func (p *Process) Pid() int {
	out, err := p.l.command(p.l.systemctl(), "show", "--property=MainPID", "--value", p.unit).Output()
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	if pid < 0 {
		return 0
	}
	return pid
}

// SelfUnit is the systemd service this process runs in ("asp-node-agent.service"),
// or "" when it is not one (started from a shell, or no cgroup2).
func SelfUnit() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	return selfUnitFrom(string(b))
}

// selfUnitFrom reads a service name out of /proc/self/cgroup content: the last
// element of the cgroup2 path, when it ends in ".service".
func selfUnitFrom(cgroups string) string {
	for _, line := range strings.Split(cgroups, "\n") {
		path, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}
		name := path[strings.LastIndex(path, "/")+1:]
		if strings.HasSuffix(name, ".service") && !strings.ContainsAny(name, " \t") {
			return name
		}
	}
	return ""
}

// Available reports why transient units cannot be used on this host, or nil.
func Available() error {
	if runtime.GOOS != "linux" {
		return errors.New("transient units need Linux")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return errors.New("systemd is not the init system (no /run/systemd/system)")
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return errors.New("systemd-run not found")
	}
	if os.Geteuid() != 0 {
		return errors.New("creating units needs root")
	}
	return nil
}
