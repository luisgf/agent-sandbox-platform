package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/rundir"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

// vmRunDir is the directory that holds one directory per VM whose VMM runs as a
// user of its own: --vm-run-dir, else next to the socket directory (/run/asp-vm
// for /run/asp). It is not inside it: the socket directory is 0700, and every
// VMM user has to pass through the parent of its own directory.
func vmRunDir(cfg config) string {
	if d := strings.TrimSpace(cfg.VMRunDir); d != "" {
		return filepath.Clean(d)
	}
	return filepath.Clean(cfg.CHSocketDir) + "-vm"
}

// vmUserProbes are the things vmUnprivileged asks of the host; tests substitute
// their own.
type vmUserProbes struct {
	// LookPath finds an executable.
	LookPath func(name string) (string, error)
	// KVMGroup is the group that owns /dev/kvm and whether a user outside it is
	// kept out (the device is not world-accessible).
	KVMGroup func() (gid uint32, needed bool, err error)
	// RunAs runs argv with the identity of u (setpriv, as Wrap builds it) and
	// returns what it printed when it fails.
	RunAs func(u *vmm.Unprivileged, argv ...string) error
	// EnsureDir creates dir the way the VM users need it (0711).
	EnsureDir func(dir string) error
}

func hostVMUserProbes() vmUserProbes {
	return vmUserProbes{
		LookPath: exec.LookPath,
		KVMGroup: func() (uint32, bool, error) {
			fi, err := os.Stat("/dev/kvm")
			if err != nil {
				return 0, false, err
			}
			st, ok := fi.Sys().(*syscall.Stat_t)
			if !ok {
				return 0, false, fmt.Errorf("cannot read the owner of /dev/kvm")
			}
			return st.Gid, fi.Mode().Perm()&0o006 != 0o006, nil
		},
		RunAs: func(u *vmm.Unprivileged, argv ...string) error {
			name, args := u.Wrap(0, argv[0], argv[1:]...)
			out, err := exec.Command(name, args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
			}
			return nil
		},
		EnsureDir: rundir.EnsureSearchable,
	}
}

// vmUnprivileged decides whether each Cloud Hypervisor runs as a user of its own
// (vmm.Unprivileged), from --vm-unprivileged, and whether this host can.
//
//	off    never: the VMM is root, as before.
//	auto   when the host can; otherwise as off, saying why.
//	on     always; refuses to start when the host cannot, so a node that is meant
//	       to keep its VMMs out of root never runs one as root by accident.
//
// It needs the VMM to run in a unit of its own (confine non-nil): the drop of
// privileges and the restrictions on the service are one mechanism. It checks, by
// running as the VM's user, that the VMM can reach what it needs: the kernel, its
// own binary, /dev/kvm and /dev/net/tun, and the directories it works in.
func vmUnprivileged(cfg config, confine *vmm.Confinement, p vmUserProbes) (*vmm.Unprivileged, error) {
	mode := strings.ToLower(strings.TrimSpace(cfg.VMUnprivileged))
	switch mode {
	case "off":
		return nil, nil
	case "", "auto", "on":
	default:
		return nil, fmt.Errorf("unknown mode %q (auto, on or off)", cfg.VMUnprivileged)
	}
	// fail is the reason the VMM cannot run as another user: refused with on,
	// logged with auto.
	fail := func(format string, args ...any) (*vmm.Unprivileged, error) {
		err := fmt.Errorf(format, args...)
		if mode == "on" {
			return nil, fmt.Errorf("cannot run microVMs as unprivileged users: %w", err)
		}
		slog.Warn("microVMs run as root: they cannot run as users of their own on this host", "reason", err.Error(),
			"fix", "see docs/bare-metal-ch.md, section 5.7; --vm-unprivileged=off silences this")
		return nil, nil
	}
	if confine == nil {
		return fail("they need --vm-confine: the VMM has to run in a unit of its own")
	}
	if cfg.VMUIDBase > 0xFFFFFFFF {
		return nil, fmt.Errorf("--vm-uid-base %d is not a user id", cfg.VMUIDBase)
	}
	u := &vmm.Unprivileged{UIDBase: uint32(cfg.VMUIDBase), RunDir: vmRunDir(cfg)}
	if err := u.Validate(); err != nil {
		return nil, fmt.Errorf("--vm-uid-base / --vm-run-dir: %w", err)
	}
	setpriv, err := p.LookPath("setpriv")
	if err != nil {
		return fail("setpriv (util-linux) is not installed")
	}
	u.Setpriv = setpriv
	gid, needed, err := p.KVMGroup()
	if err != nil {
		return fail("/dev/kvm: %v", err)
	}
	if needed {
		u.Groups = []uint32{gid}
	}
	if err := p.EnsureDir(u.RunDir); err != nil {
		return fail("the VM directory %s: %v", u.RunDir, err)
	}
	// A VM's user must be able to pass through the disk directory to its own disk.
	if cfg.DiskDir != "" {
		if err := p.EnsureDir(cfg.DiskDir); err != nil {
			return fail("the disk directory %s: %v", cfg.DiskDir, err)
		}
	}
	chBin, err := p.LookPath(cfg.VMMBinary)
	if err != nil {
		return fail("the hypervisor %q is not installed: %v", cfg.VMMBinary, err)
	}
	// Each probe does what the VMM will do, as the VM's user: open the file, change
	// into the directory, run the binary. test(1) is not asked: uutils' says no to a
	// device that only a supplementary group can open, which open() opens fine.
	type probe struct {
		what, hint string
		argv       []string
	}
	open := func(mode, path string) []string { return []string{"sh", "-c", "exec 3" + mode + "\"$1\"", "sh", path} }
	enter := func(dir string) []string { return []string{"sh", "-c", "cd \"$1\"", "sh", dir} }
	probes := []probe{
		{"switch users with setpriv", "setpriv needs util-linux 2.31 or later", []string{"true"}},
		{"run " + chBin, "give it o+x, and every directory above it o+x", []string{chBin, "--version"}},
		{"read the kernel " + cfg.GuestKernel, "give it o+r, and every directory above it o+x", open("<", cfg.GuestKernel)},
		{"open /dev/kvm", "put the user's group in the device's group (or chmod 0666 /dev/kvm)", open("<>", "/dev/kvm")},
		{"open /dev/net/tun", "chmod 0666 /dev/net/tun", open("<>", "/dev/net/tun")},
		{"enter the VM directory " + u.RunDir, "every directory above it needs o+x", enter(u.RunDir)},
	}
	if cfg.DiskDir != "" {
		probes = append(probes, probe{"enter the disk directory " + cfg.DiskDir, "every directory above it needs o+x", enter(cfg.DiskDir)})
	}
	for _, pr := range probes {
		if err := p.RunAs(u, pr.argv...); err != nil {
			return fail("a VM's user cannot %s (%s): %v", pr.what, pr.hint, err)
		}
	}
	return u, nil
}
