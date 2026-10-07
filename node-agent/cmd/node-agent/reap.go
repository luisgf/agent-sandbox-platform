package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/hostproc"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/localnet"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/reconciler"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/tap"
)

// Values of --reap-leftovers.
const (
	reapOn     = "on"
	reapReport = "report"
	reapOff    = "off"
)

// cleanHost runs before this agent binds a socket or registers. What the previous
// process left on this host (VMs, TAPs, tunnels) belongs to nobody, except the VMs
// that are still alive and recorded: those are adopted (see reconciler.Adopt), and
// their processes, sockets, TAP and tunnel are spared. Disks are not in that list:
// see reapConfig.
//
// A real agent keeps the socket-dir lock for its whole life, whatever
// --reap-leftovers says, so that a second agent or --reap-only never takes its
// VMs for leftovers. The error is fatal: another agent holds the lock.
func cleanHost(ctx context.Context, cfg config) (*reconciler.SocketDirLock, error) {
	if cfg.DryRun {
		// FakeVMM never spawns a VM: report only, and do not keep the lock, so
		// a dry-run agent never keeps a real one from starting.
		if cfg.ReapLeftovers == reapOff {
			return nil, nil
		}
		lock, err := reconciler.LockSocketDir(cfg.CHSocketDir)
		if err != nil {
			slog.Info("dry-run: leftovers not listed", "socket_dir", cfg.CHSocketDir, "reason", err)
			return nil, nil
		}
		lock.Release()
		if rep := reconciler.Reap(ctx, reapConfig(cfg, true)); rep.Err != nil {
			slog.Warn("dry-run: listing leftovers failed", "error", rep.Err)
		}
		return nil, nil
	}
	lock, err := reconciler.LockSocketDir(cfg.CHSocketDir)
	if err != nil {
		if errors.Is(err, reconciler.ErrSocketDirLocked) {
			return nil, err
		}
		slog.Warn("cannot lock --ch-socket-dir; leftovers of a previous node-agent are not removed",
			"socket_dir", cfg.CHSocketDir, "error", err)
		return nil, nil
	}
	if cfg.ReapLeftovers != reapOff {
		rc := reapConfig(cfg, cfg.ReapLeftovers == reapReport)
		rc.Adopt = adoptableIDs(ctx, cfg)
		if rep := reconciler.Reap(ctx, rc); rep.Err != nil {
			slog.Warn("host cleanup incomplete; starting anyway", "error", rep.Err)
		}
	}
	return lock, nil
}

// reapOnly is --reap-only: the cleanup cleanHost runs at start, without
// registering, then exit. The systemd unit runs it as ExecStopPost, after the
// agent stopped. VMs that keep running without it (--vm-survive-restart, the
// default) are spared: it removes what is dead, not what is alive.
func reapOnly(cfg config) int {
	if cfg.ReapLeftovers == reapOff {
		slog.Error("--reap-only with --reap-leftovers=off has nothing to do")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	lock, err := reconciler.LockSocketDir(cfg.CHSocketDir)
	if err != nil {
		slog.Error("host cleanup", "socket_dir", cfg.CHSocketDir, "error", err)
		return 1
	}
	defer lock.Release()
	report := cfg.DryRun || cfg.ReapLeftovers == reapReport
	rc := reapConfig(cfg, report)
	rc.Adopt = adoptableIDs(ctx, cfg)
	if rep := reconciler.Reap(ctx, rc); rep.Err != nil {
		slog.Error("host cleanup incomplete", "error", rep.Err)
		return 1
	}
	return 0
}

// reapConfig is where this agent's configuration puts per-sandbox state.
func reapConfig(cfg config, report bool) reconciler.ReapConfig {
	rc := reconciler.ReapConfig{
		SocketDir: cfg.CHSocketDir,
		// Never removed, even when named like a leftover.
		Keep:     []string{cfg.GuestRootFS, cfg.CHAPISocket, cfg.SSHAgentBridge, cfg.IdentityListen, cfg.PodDaemonSock},
		Report:   report,
		Tap:      &tap.Manager{Logger: slog.Default()},
		LocalNet: localnet.NewHost(localNetKeyDir(cfg)),
		Logger:   slog.Default(),
	}
	if !cfg.DryRun {
		// The host-wide TAP and WireGuard devices of a real agent next to a
		// dry-run one are not its leftovers.
		rc.SysClassNet = "/sys/class/net"
		// No rc.DiskDir: a stopped sandbox's disk is kept (ADR-0012), and the
		// reaper cannot tell it from a leftover. The reconciler removes the disks
		// the control plane does not list, after its first poll.
	}
	if runtime.GOOS == "linux" {
		rc.Procs = hostproc.ProcFS{}
	}
	return rc
}

// localNetKeyDir holds the per-sandbox WireGuard node keys.
func localNetKeyDir(cfg config) string {
	if dir := os.Getenv("ASP_LOCAL_NET_KEY_DIR"); dir != "" {
		return dir
	}
	if cfg.DryRun {
		return filepath.Join(os.TempDir(), "asp-local-net-keys")
	}
	return "/var/lib/asp/local-net"
}
