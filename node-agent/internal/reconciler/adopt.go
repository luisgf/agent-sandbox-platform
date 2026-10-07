package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/tap"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/virtiofs"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

// A VM runs in a service of its own, not bound to the agent's, so a restart of the
// agent (an upgrade, a crash) leaves it running. What the new agent process needs
// to take it over is the record it writes for each VM: where the VM's pieces are
// (its CID, TAP, sockets, disk, virtiofsd) and who it belongs to. The VMM tells
// whether the VM is really alive; the record only says how it was set up.

// stateVersion is the version of the record. A record of another version is not
// understood and its VM is not adopted.
const stateVersion = 1

// vmState is the record of one VM, in StateDir as <sandbox id>.json.
type vmState struct {
	Version   int    `json:"version"`
	SandboxID string `json:"sandbox_id"`
	TenantID  string `json:"tenant_id,omitempty"`
	OwnerSub  string `json:"owner_sub,omitempty"`

	CID        uint32    `json:"cid"`
	VsockPath  string    `json:"vsock_path"`
	TapName    string    `json:"tap_name,omitempty"`
	Slot       int       `json:"slot"`
	SSHSock    string    `json:"ssh_sock,omitempty"`
	RootFS     string    `json:"rootfs,omitempty"`
	BaseDigest string    `json:"base_digest,omitempty"`
	Serial     string    `json:"serial_socket,omitempty"`
	FSUnit     string    `json:"fs_unit,omitempty"`
	FSSocket   string    `json:"fs_socket,omitempty"`
	StartedAt  time.Time `json:"started_at"`
}

func (r *Reconciler) statePath(id string) string { return filepath.Join(r.StateDir, id+".json") }

// saveState records a VM that is up, so that an agent started after this one
// can take it over. It is best effort: without the record the VM works, but it
// does not survive a restart of the agent. A VM is recorded once its guest
// answers, never while it boots, so a crash in between leaves nothing to adopt.
func (r *Reconciler) saveState(id string, h Handle) {
	if r.StateDir == "" {
		return
	}
	st := vmState{
		Version: stateVersion, SandboxID: id, TenantID: h.TenantID, OwnerSub: h.OwnerSub,
		CID: h.CID, VsockPath: h.VsockPath, TapName: h.TapName, Slot: h.Slot, SSHSock: h.SSHSock,
		RootFS: h.RootFS, BaseDigest: h.BaseDigest, Serial: h.Serial, FSUnit: h.FSUnit, FSSocket: h.FSSocket,
		StartedAt: h.StartedAt,
	}
	if err := writeState(r.statePath(id), st); err != nil {
		r.Logger.Warn("could not record the VM; it will not survive a restart of the agent", "sandbox_id", id, "error", err)
	}
}

func (r *Reconciler) removeState(id string) {
	if r.StateDir == "" {
		return
	}
	_ = os.Remove(r.statePath(id))
}

func writeState(path string, st vmState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// readStates lists the records in dir, oldest VM first. Files that are not a
// record of this version (or not for the sandbox they are named after) are
// returned in bad so that the caller can remove them.
func readStates(dir string) (states []vmState, bad []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	for _, e := range entries {
		name := e.Name()
		id, ok := strings.CutSuffix(name, ".json")
		if !ok || !isSandboxID(id) || !e.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dir, name)
		b, rerr := os.ReadFile(path)
		var st vmState
		if rerr != nil || json.Unmarshal(b, &st) != nil || st.Version != stateVersion || st.SandboxID != id || st.VsockPath == "" {
			bad = append(bad, path)
			continue
		}
		states = append(states, st)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].StartedAt.Before(states[j].StartedAt) })
	return states, bad, nil
}

// AdoptableIDs are the sandboxes in stateDir whose VM is still alive according to
// alive (nil error). The host cleanup spares what they hold (Reap's Adopt); the
// reconciler takes them over after it (Adopt).
func AdoptableIDs(ctx context.Context, stateDir string, alive func(context.Context, string) error, log *slog.Logger) []string {
	if stateDir == "" || alive == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	states, _, err := readStates(stateDir)
	if err != nil {
		log.Warn("cannot read the records of the VMs of a previous agent; none is adopted", "dir", stateDir, "error", err)
		return nil
	}
	var ids []string
	for _, st := range states {
		if err := alive(ctx, st.SandboxID); err != nil {
			log.Info("a VM of a previous agent is not running any more; it is cleaned up", "sandbox_id", st.SandboxID, "reason", err.Error())
			continue
		}
		ids = append(ids, st.SandboxID)
	}
	return ids
}

// Adopt takes over the VMs a previous agent process left running: those with a
// record in StateDir that the VMM finds alive. Each is supervised again, its
// endpoints and bindings (the exec endpoint, the guest-to-host acceptors, the
// egress prefix, the SSH agent) are put back, and its CID and /30 are kept out of
// the allocators. A record whose VM is gone, or cannot be taken over, is removed.
// It returns the sandboxes adopted, for the register body: the control plane does
// not fail those as orphans of the restart.
//
// It runs once, before the first poll.
func (r *Reconciler) Adopt(ctx context.Context) []string {
	adopter, ok := r.Engine.(vmm.Adopter)
	if !ok || r.StateDir == "" {
		return nil
	}
	states, bad, err := readStates(r.StateDir)
	if err != nil {
		r.Logger.Warn("cannot read the records of the VMs of a previous agent; none is adopted", "dir", r.StateDir, "error", err)
		return nil
	}
	for _, p := range bad {
		r.Logger.Warn("removing a VM record that cannot be read", "path", p)
		_ = os.Remove(p)
	}
	var adopted []string
	for _, st := range states {
		if err := adopter.Adopt(ctx, st.SandboxID, st.StartedAt, st.Serial); err != nil {
			r.Logger.Warn("a VM of a previous agent could not be adopted", "sandbox_id", st.SandboxID, "error", err)
			r.Metrics.adopt("stale")
			r.removeState(st.SandboxID)
			continue
		}
		r.take(st)
		r.Metrics.adopt("ok")
		adopted = append(adopted, st.SandboxID)
	}
	if len(adopted) > 0 {
		r.Logger.Info("adopted the VMs a previous agent left running", "count", len(adopted), "sandbox_ids", adopted)
	}
	return adopted
}

// take puts back everything around a VM that was adopted.
func (r *Reconciler) take(st vmState) {
	h := Handle{
		CID: st.CID, VsockPath: st.VsockPath, TapName: st.TapName, SSHSock: st.SSHSock, Slot: st.Slot,
		RootFS: st.RootFS, BaseDigest: st.BaseDigest, TenantID: st.TenantID, OwnerSub: st.OwnerSub,
		StartedAt: st.StartedAt, Serial: st.Serial, FSUnit: st.FSUnit, FSSocket: st.FSSocket,
	}
	if st.FSUnit != "" && r.Confine != nil {
		stop, err := virtiofs.Adopt(virtiofs.Config{Launcher: &r.Confine.Launcher, Unit: r.Confine.FSSpec(st.SandboxID), SocketPath: st.FSSocket})
		if err != nil {
			r.Logger.Warn("the workspace daemon of an adopted VM could not be taken over", "sandbox_id", st.SandboxID, "error", err)
		} else {
			h.stopFS = stop
		}
	}
	r.reserveCID(st.CID)
	if st.Slot >= 0 {
		r.reserveSlot(st.Slot)
		subnet := r.GuestSubnet
		if !subnet.IsValid() {
			subnet = netip.MustParsePrefix(tap.DefaultGuestSubnet)
		}
		if gnet, err := tap.Slot(subnet, st.Slot); err == nil {
			r.Egress.Bind(st.SandboxID, gnet.Prefix)
		}
	}
	if r.SSHRegistry != nil {
		r.SSHRegistry.Bind(st.SandboxID, st.OwnerSub)
	}
	if err := r.attachGuestHost(st.SandboxID, st.VsockPath); err != nil {
		r.Logger.Warn("the guest-to-host acceptors of an adopted VM could not be attached", "sandbox_id", st.SandboxID, "error", err)
	}
	if link := r.linkSSHAgent(st.SandboxID); link != "" {
		h.SSHSock = link
	}
	r.mu.Lock()
	r.handles[st.SandboxID] = h
	r.mu.Unlock()
	r.registerEndpoint(st.SandboxID, h)
}

// reserveCID keeps cid out of the allocator: a VM that is already running holds it.
func (r *Reconciler) reserveCID(cid uint32) {
	if cid < 3 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, f := range r.freeCID {
		if f == cid {
			r.freeCID = append(r.freeCID[:i], r.freeCID[i+1:]...)
			return
		}
	}
	if r.nextCID < 3 {
		r.nextCID = 3
	}
	for r.nextCID < cid {
		r.freeCID = append(r.freeCID, r.nextCID)
		r.nextCID++
	}
	if cid >= r.nextCID {
		r.nextCID = cid + 1
	}
}

// reserveSlot keeps /30 number n out of the allocator.
func (r *Reconciler) reserveSlot(n int) {
	if n < 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, f := range r.freeSlot {
		if f == n {
			r.freeSlot = append(r.freeSlot[:i], r.freeSlot[i+1:]...)
			return
		}
	}
	for r.nextSlot < n {
		r.freeSlot = append(r.freeSlot, r.nextSlot)
		r.nextSlot++
	}
	if n >= r.nextSlot {
		r.nextSlot = n + 1
	}
}
