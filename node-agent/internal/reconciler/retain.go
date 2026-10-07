package reconciler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/cpclient"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/measure"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/poddaemon"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

// A stopped sandbox keeps its disk (ADR-0012). The control plane says whether
// it does: a poll that carries the retained list means stopping keeps the
// disk, and a control plane that predates it still means stopping deletes it.

// errDiskLost: a resume found no disk where the sandbox's was kept.
var errDiskLost = errors.New("the retained disk of this sandbox is missing on this node")

// diskGCEvery is how often the node looks for disks nobody owns.
const diskGCEvery = time.Minute

// poweroffCmd asks the guest to sync and power itself off. The ACPI power
// button does nothing with the guest image (no logind), and without dbus
// systemctl falls back to powering off at once after the sync. --no-block
// returns before the guest goes down; the connection may drop anyway.
var poweroffCmd = []string{"/bin/sh", "-c", "sync; exec systemctl poweroff --no-block"}

// teardownOpts says how teardownLocal ends a VM.
type teardownOpts struct {
	// keepDisk leaves the sandbox's private rootfs copy on the host: a stop.
	// A delete, a self-fence and a failed first start remove it.
	keepDisk bool
	// graceful asks the guest to power off before the VMM is stopped, so the
	// disk is left clean.
	graceful bool
}

// retainsDisks reports whether the control plane that answered the last poll
// keeps the disks of stopped sandboxes.
func (r *Reconciler) retainsDisks() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.retains
}

// shutdownGuest asks a running guest to power off and waits up to StopGrace
// for its VMM to say so. It returns when the VM is down or the grace is spent;
// the caller stops the VMM either way. Only real VMs (hybrid vsock endpoints)
// have a guest to ask.
func (r *Reconciler) shutdownGuest(ctx context.Context, id string) {
	if r.StopGrace <= 0 || r.Registry == nil {
		return
	}
	if ep, ok := r.Registry.Lookup(id); !ok || ep.Mode != poddaemon.ModeHybrid {
		return
	}
	c, err := r.Registry.ClientFor(id)
	if err != nil {
		return
	}
	start := time.Now()
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	// The guest may go down before pod-daemon answers: either outcome is fine.
	_, execErr := c.Exec(callCtx, poddaemon.ExecRequest{Cmd: poweroffCmd})
	cancel()
	w, ok := r.Engine.(vmm.Shutdowner)
	if !ok {
		return
	}
	if w.WaitShutdown(ctx, id, r.StopGrace) {
		r.Logger.Info("guest powered off", "sandbox_id", id, "after", time.Since(start).Round(time.Millisecond))
		return
	}
	r.Logger.Warn("guest did not power off in time; stopping the VM hard", "sandbox_id", id,
		"grace", r.StopGrace.String(), "exec_error", execErr)
}

// rootFSFor returns the disk a sandbox boots from. A first boot gets a fresh
// copy of the base image; a resume (boot_count above 1) reuses the disk its
// stop kept, and never makes a new one in its place: a missing disk is
// errDiskLost. resumed says which of the two it was.
//
// baseDigest is the digest of the base image the disk was copied from: measured
// for a first boot, read from the record next to the disk for a resume ("" for
// a disk that has none).
func (r *Reconciler) rootFSFor(sb cpclient.Sandbox) (path, baseDigest string, resumed bool, err error) {
	if r.DiskDir == "" {
		return "", "", false, nil
	}
	resumed = sb.BootCount > 1
	if err := r.checkDiskSpace(); err != nil {
		return "", "", resumed, err
	}
	if resumed {
		dst := filepath.Join(r.DiskDir, rootfsName(sb.ID))
		if fi, statErr := os.Stat(dst); statErr != nil || !fi.Mode().IsRegular() {
			return "", "", true, fmt.Errorf("%w (%s)", errDiskLost, dst)
		}
		return dst, readBaseDigest(dst), true, nil
	}
	path, baseDigest, err = r.cloneRootFS(sb.ID)
	return path, baseDigest, false, err
}

// baseDigestPath is where the digest of the base image a disk was copied from is
// kept: next to the disk, so it lives and dies with it.
func baseDigestPath(disk string) string { return strings.TrimSuffix(disk, ".img") + baseDigestSuffix }

const baseDigestSuffix = ".base-sha256"

// writeBaseDigest records digest next to disk.
func writeBaseDigest(disk, digest string) error {
	path := baseDigestPath(disk)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(digest+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// readBaseDigest returns the digest recorded for disk, or "" when there is none
// or it is not a digest.
func readBaseDigest(disk string) string {
	b, err := os.ReadFile(baseDigestPath(disk))
	if err != nil {
		return ""
	}
	d := strings.TrimSpace(string(b))
	if !measure.Valid(d) {
		return ""
	}
	return d
}

func removeBaseDigest(disk string) {
	_ = os.Remove(baseDigestPath(disk))
}

// removeDiskFiles removes a disk and the record kept next to it.
func removeDiskFiles(disk string) {
	_ = os.Remove(disk)
	removeBaseDigest(disk)
}

// startFailure is what a start that could not finish reports. A first boot
// failed. A resume goes back to stopped, with its disk, so it can be tried
// again; only a missing disk fails it for good.
func startFailure(sb cpclient.Sandbox, err error) (state, detail string) {
	switch {
	case errors.Is(err, errDiskLost):
		return "failed", "disk_lost: " + err.Error()
	case sb.BootCount > 1:
		return "stopped", "resume failed: " + err.Error()
	}
	return "failed", err.Error()
}

// failStart reports a start that did not get as far as a VM.
func (r *Reconciler) failStart(ctx context.Context, sb cpclient.Sandbox, err error) {
	state, detail := startFailure(sb, err)
	_, _ = r.CP.ReportStatus(ctx, sb.ID, state, detail)
}

// DiskFreeMiB is the free space in dir's filesystem, in MiB. ok is false when
// it cannot be read.
func DiskFreeMiB(dir string) (mib int64, ok bool) {
	b, err := freeBytes(dir)
	if err != nil {
		return 0, false
	}
	return int64(b >> 20), true
}

// checkDiskSpace refuses to give a sandbox a disk when --disk-dir is nearly
// full: the clone is sparse, but the guest writes into it.
func (r *Reconciler) checkDiskSpace() error {
	if r.MinFreeDiskMiB <= 0 || r.DiskDir == "" {
		return nil
	}
	if err := os.MkdirAll(r.DiskDir, 0o700); err != nil {
		return err
	}
	free := r.FreeDisk
	if free == nil {
		free = freeBytes
	}
	b, err := free(r.DiskDir)
	if err != nil {
		r.Logger.Warn("cannot read free disk space; not checking it", "dir", r.DiskDir, "error", err)
		return nil
	}
	if mib := int64(b >> 20); mib < r.MinFreeDiskMiB {
		return fmt.Errorf("disk: %d MiB free in %s, need %d", mib, r.DiskDir, r.MinFreeDiskMiB)
	}
	return nil
}

// removeRootFSByID removes a sandbox's private disk by name: there may be no
// handle for it (a stopped sandbox being deleted).
func (r *Reconciler) removeRootFSByID(id string) {
	if r.DiskDir == "" {
		return
	}
	r.removeRootFS(filepath.Join(r.DiskDir, rootfsName(id)))
}

// ensureDeleted ends a sandbox the control plane is deleting: its VM, if it
// has one, and its disk, whether it was running or kept by a stop.
func (r *Reconciler) ensureDeleted(ctx context.Context, sb cpclient.Sandbox) error {
	r.teardownLocal(ctx, sb.ID, teardownOpts{})
	r.removeRootFSByID(sb.ID)
	if _, err := r.CP.ReportStatus(ctx, sb.ID, "deleted", "vmm and disk removed"); err != nil {
		return fmt.Errorf("report deleted: %w", err)
	}
	r.Logger.Info("sandbox deleted", "sandbox_id", sb.ID)
	return nil
}

// gcDisks removes the rootfs copies no sandbox owns: not assigned to this
// node, not retained by a stop, not being deleted, and not held or started by
// this agent. It replaces the startup reaper's disk pass, which would have
// removed the disks a stop keeps. It runs after a successful poll only, at
// most once a minute, and never when the control plane did not send Assigned
// (an older one): then nothing can be said to be missing from it.
func (r *Reconciler) gcDisks(work cpclient.Work) {
	if r.DiskDir == "" || work.Assigned == nil {
		return
	}
	r.mu.Lock()
	now := r.clock()
	if !r.lastDiskGC.IsZero() && now.Sub(r.lastDiskGC) < diskGCEvery {
		r.mu.Unlock()
		return
	}
	r.lastDiskGC = now
	keep := map[string]bool{}
	own := func(id string) { keep[filepath.Join(r.DiskDir, rootfsName(id))] = true }
	for _, id := range work.Assigned {
		own(id)
	}
	for _, id := range work.Retained {
		own(id)
	}
	for _, sb := range work.Sandboxes {
		own(sb.ID)
	}
	for id := range r.handles {
		own(id)
	}
	for id := range r.inflight {
		own(id)
	}
	r.mu.Unlock()

	disks, err := leftoverDisks(r.DiskDir, keep)
	if err != nil {
		if !os.IsNotExist(err) {
			r.Logger.Warn("disk GC: list", "dir", r.DiskDir, "error", err)
		}
		return
	}
	for _, p := range disks {
		r.Logger.Info("removing a disk no sandbox owns", "path", p)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			r.Logger.Warn("disk GC: remove", "path", p, "error", err)
		}
		removeBaseDigest(p)
	}
	removeOrphanBaseDigests(r.DiskDir)
}

// removeOrphanBaseDigests removes digest records whose disk is gone (a crash
// between the two removals, or a disk deleted by hand).
func removeOrphanBaseDigests(diskDir string) {
	entries, err := os.ReadDir(diskDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		id, ok := between(e.Name(), rootfsPrefix, baseDigestSuffix)
		if !ok || !isSandboxID(id) {
			continue
		}
		if _, err := os.Stat(filepath.Join(diskDir, rootfsName(id))); os.IsNotExist(err) {
			_ = os.Remove(filepath.Join(diskDir, e.Name()))
		}
	}
}
