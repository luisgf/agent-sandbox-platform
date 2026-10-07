//go:build unix

package main

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/reconciler"
)

// These tests never run a destructive cleanup: it would look at the real
// /sys/class/net and /proc of the machine running them.

func TestRealAgentHoldsTheSocketDirLock(t *testing.T) {
	t.Setenv("ASP_LOCAL_NET_KEY_DIR", t.TempDir())
	cfg := config{CHSocketDir: t.TempDir(), ReapLeftovers: reapOff}
	lock, err := cleanHost(context.Background(), cfg)
	if err != nil || lock == nil {
		t.Fatalf("lock=%v err=%v", lock, err)
	}
	// A second agent on the same directory refuses to start, and --reap-only
	// refuses to run: either would take this agent's VMs for leftovers.
	if _, err := cleanHost(context.Background(), cfg); !errors.Is(err, reconciler.ErrSocketDirLocked) {
		t.Fatalf("second agent: %v, want ErrSocketDirLocked", err)
	}
	only := config{DryRun: true, CHSocketDir: cfg.CHSocketDir, ReapLeftovers: reapOn}
	if code := reapOnly(only); code != 1 {
		t.Fatalf("--reap-only next to a running agent exited %d, want 1", code)
	}
	lock.Release()
	if code := reapOnly(only); code != 0 {
		t.Fatalf("--reap-only (dry-run) after the agent stopped exited %d, want 0", code)
	}
	if code := reapOnly(config{CHSocketDir: cfg.CHSocketDir, ReapLeftovers: reapOff}); code != 2 {
		t.Fatalf("--reap-only with --reap-leftovers=off exited %d, want 2", code)
	}
}

// A dry-run agent only reports, and never keeps a real agent from starting.
func TestDryRunNeverKeepsTheLock(t *testing.T) {
	t.Setenv("ASP_LOCAL_NET_KEY_DIR", t.TempDir())
	cfg := config{DryRun: true, CHSocketDir: t.TempDir(), ReapLeftovers: reapOn}
	if lock, err := cleanHost(context.Background(), cfg); err != nil || lock != nil {
		t.Fatalf("dry-run kept lock=%v err=%v", lock, err)
	}
	real, err := reconciler.LockSocketDir(cfg.CHSocketDir)
	if err != nil {
		t.Fatalf("a real agent cannot start after a dry-run one: %v", err)
	}
	defer real.Release()
	if lock, err := cleanHost(context.Background(), cfg); err != nil || lock != nil {
		t.Fatalf("dry-run next to a real agent: lock=%v err=%v", lock, err)
	}
}

func TestReapConfigDryRunStaysOffTheHost(t *testing.T) {
	t.Setenv("ASP_LOCAL_NET_KEY_DIR", "/keys")
	real := reapConfig(config{CHSocketDir: "/run/asp", DiskDir: "/var/lib/asp/disks", CHAPISocket: "/run/ch.sock", GuestRootFS: "/srv/asp/base.img"}, false)
	if real.DiskDir != "" || real.SysClassNet != "/sys/class/net" || real.Report {
		t.Fatalf("real agent: %+v", real)
	}
	// The base image is never removed, wherever --guest-rootfs puts it.
	if !slices.Contains(real.Keep, "/srv/asp/base.img") || !slices.Contains(real.Keep, "/run/ch.sock") {
		t.Fatalf("keep=%q", real.Keep)
	}
	if real.LocalNet == nil || real.LocalNet.KeyDir != "/keys" {
		t.Fatalf("local-net key dir: %+v", real.LocalNet)
	}
	dry := reapConfig(config{DryRun: true, CHSocketDir: "/tmp/x", DiskDir: "/var/lib/asp/disks"}, true)
	if dry.DiskDir != "" || dry.SysClassNet != "" || !dry.Report {
		t.Fatalf("dry-run: %+v", dry)
	}
}
