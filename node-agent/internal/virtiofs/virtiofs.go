package virtiofs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/hostproc"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/unit"
)

// ErrNotFound means the virtiofsd binary is not on PATH (or the configured
// path does not exist). The reconciler fails sandbox start only when a
// workspace was requested.
var ErrNotFound = errors.New("virtiofsd not found")

// Config is one virtiofsd process for a single sandbox share.
type Config struct {
	// Binary is the virtiofsd executable. Empty means "virtiofsd" on PATH.
	// The expected CLI is the Rust virtiofsd (virtio-fs / QEMU 8+), not the
	// removed C helper that took `-o source=`.
	Binary string
	// SocketPath is the per-sandbox UNIX socket Cloud Hypervisor connects to.
	SocketPath string
	// SharedDir is the host directory exported under the virtiofs tag.
	SharedDir string
	// Sandbox is virtiofsd's --sandbox mode: "chroot" confines the daemon to
	// SharedDir, so a link inside it cannot lead out; "namespace" does the same
	// with namespaces; "none" (the default here) does not.
	Sandbox string
	// Launcher, when set, runs the daemon as the transient systemd service Unit
	// describes (its own cgroup and limits) instead of as a child of this process.
	Launcher *unit.Launcher
	Unit     unit.Spec
}

// Sandbox modes of virtiofsd.
const (
	SandboxNone      = "none"
	SandboxChroot    = "chroot"
	SandboxNamespace = "namespace"
)

// ValidSandbox reports whether mode is one virtiofsd takes ("" means none).
func ValidSandbox(mode string) bool {
	switch mode {
	case "", SandboxNone, SandboxChroot, SandboxNamespace:
		return true
	}
	return false
}

// PIDFileSuffix is appended to the socket path for the pid file virtiofsd
// writes and locks next to its socket (virtiofs-{id}.sock.pid). virtiofsd does
// not remove it when it exits.
const PIDFileSuffix = ".pid"

// DaemonArgs is the argv virtiofsd gets after the binary name. sandbox is its
// --sandbox mode ("" means none: it needs nothing from the node-agent's
// process, and the share is still only sharedDir). --cache never keeps the
// guest view coherent with host writes without a DAX window.
func DaemonArgs(socketPath, sharedDir, sandbox string) []string {
	if sandbox == "" {
		sandbox = SandboxNone
	}
	return []string{
		"--socket-path", socketPath,
		"--shared-dir", sharedDir,
		"--cache", "never",
		"--sandbox", sandbox,
	}
}

// SocketPathOf returns the socket a virtiofsd started with DaemonArgs serves,
// from its argv. The node-agent reaper uses it to find daemons a previous
// agent process left running.
func SocketPathOf(argv []string) (string, bool) {
	return hostproc.FlagValue(argv, "--socket-path")
}

// childProc is a daemon that is a child of this process.
type childProc struct{ cmd *exec.Cmd }

func (c childProc) Wait() error { return c.cmd.Wait() }

func (c childProc) Kill() error {
	if c.cmd.Process == nil {
		return nil
	}
	return c.cmd.Process.Kill()
}

// Start launches virtiofsd and waits until the socket exists.
// The returned function kills the daemon and removes the socket, and the pid
// file once the daemon has exited. It is safe to call more than once.
func Start(ctx context.Context, cfg Config) (func(), error) {
	if cfg.SocketPath == "" {
		return nil, fmt.Errorf("virtiofs: socket path required")
	}
	if cfg.SharedDir == "" {
		return nil, fmt.Errorf("virtiofs: shared dir required")
	}
	info, err := os.Stat(cfg.SharedDir)
	if err != nil {
		return nil, fmt.Errorf("virtiofs: shared dir: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("virtiofs: shared dir %s is not a directory", cfg.SharedDir)
	}
	bin := cfg.Binary
	if bin == "" {
		bin = "virtiofsd"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, bin)
	}
	if err := os.Remove(cfg.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("virtiofs: remove stale socket: %w", err)
	}
	if !ValidSandbox(cfg.Sandbox) {
		return nil, fmt.Errorf("virtiofs: unknown sandbox mode %q (none, chroot or namespace)", cfg.Sandbox)
	}
	// proc is the daemon: a child of this process, or a transient unit.
	var proc interface {
		Wait() error
		Kill() error
	}
	daemonArgs := DaemonArgs(cfg.SocketPath, cfg.SharedDir, cfg.Sandbox)
	if cfg.Launcher != nil {
		p, err := cfg.Launcher.Start(cfg.Unit, path, daemonArgs...)
		if err != nil {
			return nil, fmt.Errorf("virtiofs: start %s: %w", path, err)
		}
		proc = p
	} else {
		cmd := exec.Command(path, daemonArgs...)
		cmd.Stdout = nil
		cmd.Stderr = nil
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("virtiofs: start %s: %w", path, err)
		}
		proc = childProc{cmd}
	}
	var waitErr error
	exited := make(chan struct{})
	go func() { waitErr = proc.Wait(); close(exited) }()
	pidFile := cfg.SocketPath + PIDFileSuffix

	stop := func() {
		_ = proc.Kill()
		select {
		case <-exited:
			// virtiofsd holds a lock on its pid file while it runs.
			_ = os.Remove(pidFile)
		case <-time.After(2 * time.Second):
		}
		_ = os.Remove(cfg.SocketPath)
	}

	deadline := time.Now().Add(3 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	for {
		if err := ctx.Err(); err != nil {
			stop()
			return nil, err
		}
		if _, err := os.Stat(cfg.SocketPath); err == nil {
			// Only root, the user the node-agent and Cloud Hypervisor run as,
			// speaks vhost-user to this daemon.
			_ = os.Chmod(cfg.SocketPath, 0o600)
			return stop, nil
		}
		select {
		case <-exited:
			_ = os.Remove(cfg.SocketPath)
			_ = os.Remove(pidFile)
			err := waitErr
			if err == nil {
				err = fmt.Errorf("exited before creating %s", cfg.SocketPath)
			}
			return nil, fmt.Errorf("virtiofs: %s: %w", path, err)
		default:
		}
		if time.Now().After(deadline) {
			stop()
			return nil, fmt.Errorf("virtiofs: timeout waiting for socket %s", cfg.SocketPath)
		}
		select {
		case <-ctx.Done():
			stop()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Adopt takes over the virtiofsd an earlier agent started in the service
// cfg.Unit describes and left running: it must still be active and serve its
// socket. The returned function stops it and removes its socket and pid file,
// as Start's does.
func Adopt(cfg Config) (func(), error) {
	if cfg.Launcher == nil || cfg.Unit.Name == "" || cfg.SocketPath == "" {
		return nil, fmt.Errorf("virtiofs: adopting needs the launcher, the unit and the socket of the daemon")
	}
	if !cfg.Launcher.Active(cfg.Unit.Name) {
		return nil, fmt.Errorf("virtiofs: %s.service is not active", cfg.Unit.Name)
	}
	if _, err := os.Stat(cfg.SocketPath); err != nil {
		return nil, fmt.Errorf("virtiofs: %w", err)
	}
	proc := cfg.Launcher.Adopt(cfg.Unit.Name)
	pidFile := cfg.SocketPath + PIDFileSuffix
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = proc.Kill()
			deadline := time.Now().Add(2 * time.Second)
			for cfg.Launcher.Active(cfg.Unit.Name) && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			proc.Stop()
			_ = os.Remove(pidFile)
			_ = os.Remove(cfg.SocketPath)
		})
	}, nil
}
