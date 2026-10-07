package virtiofs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/hostproc"
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
}

// PIDFileSuffix is appended to the socket path for the pid file virtiofsd
// writes and locks next to its socket (virtiofs-{id}.sock.pid). virtiofsd does
// not remove it when it exits.
const PIDFileSuffix = ".pid"

// DaemonArgs is the argv virtiofsd gets after the binary name.
// --sandbox none avoids needing a user namespace inside the node-agent
// process; the share is still only SharedDir. --cache never keeps the guest
// view coherent with host writes without a DAX window.
func DaemonArgs(socketPath, sharedDir string) []string {
	return []string{
		"--socket-path", socketPath,
		"--shared-dir", sharedDir,
		"--cache", "never",
		"--sandbox", "none",
	}
}

// SocketPathOf returns the socket a virtiofsd started with DaemonArgs serves,
// from its argv. The node-agent reaper uses it to find daemons a previous
// agent process left running.
func SocketPathOf(argv []string) (string, bool) {
	return hostproc.FlagValue(argv, "--socket-path")
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
	cmd := exec.Command(path, DaemonArgs(cfg.SocketPath, cfg.SharedDir)...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("virtiofs: start %s: %w", path, err)
	}
	var waitErr error
	exited := make(chan struct{})
	go func() { waitErr = cmd.Wait(); close(exited) }()
	pidFile := cfg.SocketPath + PIDFileSuffix

	stop := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
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
