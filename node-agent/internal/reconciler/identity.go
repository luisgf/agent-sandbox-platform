package reconciler

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/hostvsock"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/rundir"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

// A VM whose VMM runs as a user of its own (vmm.Unprivileged) hands that user
// exactly what the VMM opens: a directory for its sockets, the sockets the agent
// and virtiofsd make for it to connect to, its disk, and the TAP. All of it is
// given to the user just before the VMM starts and taken back when the VM is
// released, so a user id that is later reused by another VM owns nothing of the
// previous one. The agent runs as root and never follows a link in these
// directories: they are the VMM's to write.

// unprivileged is how VMMs run as users of their own on this node, or nil.
func (r *Reconciler) unprivileged() *vmm.Unprivileged {
	if r.Confine == nil {
		return nil
	}
	return r.Confine.Unprivileged
}

// vmUID is the user the VM with guest CID cid runs as, and whether VMs run as
// users of their own at all.
func (r *Reconciler) vmUID(cid uint32) (uint32, bool) {
	u := r.unprivileged()
	if u == nil {
		return 0, false
	}
	return u.UID(cid), true
}

func (r *Reconciler) chown(path string, uid uint32) error {
	if r.Chown != nil {
		return r.Chown(path, int(uid), int(uid))
	}
	return os.Lchown(path, int(uid), int(uid))
}

// prepareRunDir makes the empty directory the VMM of cfg works in, owned by the VM's
// user. Nothing of an earlier VM of the same sandbox may be in it.
func (r *Reconciler) prepareRunDir(cfg vmm.MicroVMConfig) error {
	uid, ok := r.vmUID(cfg.VsockCID)
	if !ok {
		return nil
	}
	u := r.unprivileged()
	if err := rundir.EnsureSearchable(u.RunDir); err != nil {
		return fmt.Errorf("VM directory: %w", err)
	}
	if err := os.RemoveAll(cfg.RunDir); err != nil {
		return fmt.Errorf("VM directory: remove a leftover: %w", err)
	}
	if err := os.Mkdir(cfg.RunDir, 0o700); err != nil {
		return fmt.Errorf("VM directory: %w", err)
	}
	if err := r.chown(cfg.RunDir, uid); err != nil {
		_ = os.RemoveAll(cfg.RunDir)
		return fmt.Errorf("VM directory: give it to the VM's user: %w", err)
	}
	return nil
}

// removeRunDir deletes a VM's directory and whatever the VMM left in it. The
// VMM is gone by now; RemoveAll does not follow links.
func (r *Reconciler) removeRunDir(dir string) {
	if dir == "" {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		r.Logger.Warn("remove the VM directory", "dir", dir, "error", err)
	}
}

// prepareDiskDir lets the VMM users reach the disks in DiskDir: they pass
// through it but cannot list it, and each disk is readable by its VM's user only.
func (r *Reconciler) prepareDiskDir() error {
	if r.unprivileged() == nil || r.DiskDir == "" {
		return nil
	}
	if err := rundir.EnsureSearchable(r.DiskDir); err != nil {
		return fmt.Errorf("disk directory: %w", err)
	}
	return nil
}

// giveDisk makes path (the VM's disk) the VM's user's.
func (r *Reconciler) giveDisk(path string, cid uint32) error {
	uid, ok := r.vmUID(cid)
	if !ok || path == "" {
		return nil
	}
	if err := r.chown(path, uid); err != nil {
		return fmt.Errorf("give the disk to the VM's user: %w", err)
	}
	return nil
}

// takeBackDisk makes a disk that outlives its VM (a stopped sandbox keeps it)
// root's again. uid is the user the VM ran as, 0 for a VM that ran as root.
func (r *Reconciler) takeBackDisk(path string, uid uint32) {
	if path == "" || uid == 0 {
		return
	}
	if err := r.chown(path, 0); err != nil && !os.IsNotExist(err) {
		r.Logger.Warn("take the disk back from the VM's user", "path", path, "error", err)
	}
}

// giveGuestHostSockets lets the VMM, which runs as user uid (0 for root, which
// needs nothing), connect to the guest to host acceptors the agent opened for it
// ({vsock}_{port}): connecting to a unix socket needs write access to it.
func (r *Reconciler) giveGuestHostSockets(muxerPath string, uid uint32) error {
	if uid == 0 {
		return nil
	}
	for _, port := range []uint32{hostvsock.PortSSHAgent, hostvsock.PortIdentity} {
		p := hostvsock.HybridGuestPath(muxerPath, port)
		if err := r.chown(p, uid); err != nil {
			return fmt.Errorf("give %s to the VM's user: %w", filepath.Base(p), err)
		}
	}
	return nil
}

// giveFSSocket lets the VMM connect to the socket of the virtiofsd of its VM.
func (r *Reconciler) giveFSSocket(path string, cid uint32) error {
	uid, ok := r.vmUID(cid)
	if !ok || path == "" {
		return nil
	}
	if err := r.chown(path, uid); err != nil {
		return fmt.Errorf("give the workspace socket to the VM's user: %w", err)
	}
	return nil
}
