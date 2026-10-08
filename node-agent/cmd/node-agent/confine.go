package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/reconciler"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/unit"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

// vmConfinement decides whether each microVM runs in a transient systemd
// service of its own, from --vm-confine and whether this host can (available is
// nil when it can, else the reason it cannot).
//
//	off    never: the VMMs are children of the agent, as before.
//	auto   when the host can; otherwise as off, saying why.
//	on     always; refuses to start when the host cannot, so a node that is meant
//	       to confine its VMs never runs them unconfined by accident.
func vmConfinement(cfg config, available error) (*vmm.Confinement, error) {
	switch mode := strings.ToLower(strings.TrimSpace(cfg.VMConfine)); mode {
	case "off":
		return nil, nil
	case "", "auto":
		if available != nil {
			slog.Info("microVMs run as children of the agent, without a cgroup of their own", "reason", available.Error())
			return nil, nil
		}
	case "on":
		if available != nil {
			return nil, fmt.Errorf("cannot confine microVMs: %w", available)
		}
	default:
		return nil, fmt.Errorf("unknown mode %q (auto, on or off)", cfg.VMConfine)
	}
	if cfg.VMMemoryOverheadMiB < 0 || cfg.VMCPUOverheadPercent < 0 {
		return nil, fmt.Errorf("--vm-memory-overhead-mib and --vm-cpu-overhead-percent cannot be negative")
	}
	if cfg.VMTasksMax <= 0 {
		return nil, fmt.Errorf("--vm-tasks-max must be positive")
	}
	slice := strings.TrimSpace(cfg.VMSlice)
	if slice != "" && !strings.HasSuffix(slice, ".slice") {
		return nil, fmt.Errorf("--vm-slice %q is not a slice (it ends in .slice)", slice)
	}
	return &vmm.Confinement{
		Slice:              slice,
		MemoryOverheadMiB:  cfg.VMMemoryOverheadMiB,
		CPUOverheadPercent: cfg.VMCPUOverheadPercent,
		TasksMax:           cfg.VMTasksMax,
		FSMemoryMiB:        vmm.DefaultFSMemoryMiB,
		FSTasksMax:         vmm.DefaultFSTasksMax,
		BindTo:             bindTo(cfg),
	}, nil
}

// bindTo is the service the VM units are bound to: the agent's own when the VMs
// must end with it (--vm-survive-restart=false), none when they outlive it.
func bindTo(cfg config) string {
	if cfg.VMSurviveRestart {
		return ""
	}
	return unit.SelfUnit()
}

// vmStateDir is where the record of each running VM is kept, or "" when VMs are
// not recorded: only a confined VM (a service of its own) can survive the agent,
// and only when it is meant to.
func vmStateDir(cfg config, confine *vmm.Confinement) string {
	if cfg.DryRun || confine == nil || !cfg.VMSurviveRestart || cfg.CHAPISocket != "" {
		return ""
	}
	return filepath.Join(cfg.CHSocketDir, "state")
}

// adoptableIDs are the sandboxes whose VM an earlier agent process left running
// and this one can take over. The host cleanup spares what they hold.
func adoptableIDs(ctx context.Context, cfg config) []string {
	if cfg.DryRun || cfg.CHAPISocket != "" {
		return nil
	}
	confine, err := vmConfinement(cfg, unit.Available())
	if err != nil || confine == nil {
		return nil
	}
	dir := vmStateDir(cfg, confine)
	if dir == "" {
		return nil
	}
	ch := vmm.NewSpawningCloudHypervisor(cfg.VMMBinary, cfg.CHSocketDir)
	ch.LookPath = exec.LookPath
	ch.Confine = confine
	return reconciler.AdoptableIDs(ctx, dir, ch.Alive, slog.Default())
}
