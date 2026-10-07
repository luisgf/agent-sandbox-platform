// Package reconciler claims sandboxes from the control plane and drives the VMM.
package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/attest"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/cpclient"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/egress"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/localnet"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/poddaemon"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/rundir"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/safe"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/sshagent"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/tap"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/virtiofs"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/workspace"
)

// GuestHostAcceptor registers per-sandbox CH/Firecracker hybrid guest→host
// listeners on {vsockMuxer}_{port}. Implemented by hostvsock.Service.
type GuestHostAcceptor interface {
	AttachSandbox(sandboxID, muxerPath string) error
	DetachSandbox(sandboxID string)
}

// Handle holds per-sandbox local state after a successful Start.
type Handle struct {
	CID       uint32
	VsockPath string
	TapName   string
	SSHSock   string // per-sandbox symlink path under VsockDir (virtiofs docs)
	Slot      int    // index of the guest /30 in GuestSubnet; -1 without a TAP
	RootFS    string // this sandbox's private rootfs copy; "" when not cloned
	// BaseDigest is "sha256:<hex>" of the base image the disk was copied from; ""
	// when that is not known (not measured, or a disk older than the record).
	BaseDigest string
	stopFS     func() // stops virtiofsd when a workspace was mounted
}

// Reconciler polls control-plane work, claims sandboxes, and starts/stops VMs.
type Reconciler struct {
	CP     *cpclient.Client
	NodeID string
	Engine vmm.MicroVM
	Logger *slog.Logger
	Every  time.Duration

	// KernelPath / RootFSPath are defaults for dry-run FakeVMM configs.
	KernelPath string
	RootFSPath string
	// DiskDir, when set, gives every VM a private copy of RootFSPath
	// (DiskDir/rootfs-{id}.img), deleted on stop. Without it all VMs would
	// boot one writable image: concurrent guests corrupt the filesystem and
	// whatever one sandbox writes persists into the next.
	DiskDir string
	// CloneDisk copies src to dst (tests). Nil uses cp --reflink=auto --sparse=always.
	CloneDisk func(src, dst string) error

	// VsockDir holds host CH vsock muxer sockets (vsock-{sandboxID}.sock).
	VsockDir string
	// VsockPort is the guest AF_VSOCK port where pod-daemon listens (DefaultGuestPort).
	VsockPort uint32
	// PodDaemonUnix, when set (dry-run), registers unix endpoints instead of hybrid vsock.
	PodDaemonUnix string
	// Registry receives dial endpoints so exec can reach the guest (or dry-run sock).
	Registry *poddaemon.Registry

	// TapAuto enables create/delete of TAP devices around VMM Start/Stop.
	TapAuto bool
	// Tap is the TAP manager. Nil uses a strict one (ip(8), errors returned);
	// main passes a soft-failing manager only to dry-run agents.
	Tap *tap.Manager
	// GuestSubnet is the pool each TAP's /30 is carved from (default
	// tap.DefaultGuestSubnet). Must match the nft --guest-subnet.
	GuestSubnet netip.Prefix
	// Egress, when set, learns each sandbox's /30 and its tenant's policy (from
	// every work poll) so the forward proxy and
	// the DNS sink apply that sandbox's policy to its traffic.
	Egress *egress.PolicyCache
	// egressVersions is the policy version applied to each sandbox (mu).
	egressVersions map[string]string
	// localNetDone is the local-net plan applied to each sandbox (mu).
	localNetDone map[string]*localNetApplied
	// now is the clock (tests); nil is time.Now.
	now func() time.Time

	// SSHAgentShared is the host bridge socket path (--ssh-agent-bridge).
	// When set (and no per-sandbox registry path), Start creates
	// VsockDir/ssh-agent-{id}.sock → shared for virtiofs docs (legacy).
	SSHAgentShared string

	// SSHRegistry optional per-sandbox SSH agent upstream map (ADR-0007 phase 4).
	// On Start, Bind(sandboxID, owner_sub) expands ASP_SSH_AGENT_SOCK_TEMPLATE;
	// AttachSandbox ServeConn uses the resolved HostSock. Symlink targets the
	// resolved path when non-empty.
	SSHRegistry *sshagent.Registry

	// GuestHost, when set, attaches hybrid guest→host acceptors on the CH
	// muxer path ({vsock}_26501 / {vsock}_26502). Required for SSH agent +
	// identity under Cloud Hypervisor hybrid vsock.
	GuestHost GuestHostAcceptor

	// Attest signs boot attestations, tried in order until the control plane
	// accepts one (the node certificate key over mTLS, then ASP_ATTEST_KEY).
	// Empty uses ASP_ATTEST_KEY (attest.SignNow).
	Attest []*attest.Signer
	// Measure returns "sha256:<hex>" of a file. Boot attestations use it for the
	// kernel and the base image, so they say what was booted. Nil leaves them
	// unmeasured, as in dry-run, which boots nothing.
	Measure func(path string) (string, error)
	// VMMVersion is what the hypervisor binary reports as its version, for the
	// boot attestation. Empty is left out.
	VMMVersion string

	// WorkspaceRoots are the directories a sandbox's workspace may live under
	// (<root>/<tenant>/…). With none, no sandbox may have a workspace: the path
	// comes from the sandbox spec and virtiofsd shares it with the guest.
	WorkspaceRoots workspace.Roots
	// VirtiofsdSandbox is virtiofsd's --sandbox mode (none, chroot, namespace).
	VirtiofsdSandbox string
	// Confine, when set, runs each virtiofsd in a transient systemd service of its
	// own with resource limits, as the VMM does (vmm.CloudHypervisor.Confine).
	Confine *vmm.Confinement
	// VirtiofsdBin is the virtiofsd executable. Empty means "virtiofsd" on PATH.
	// Used only when the sandbox spec has a workspace_host_path.
	VirtiofsdBin string
	// FSLauncher overrides virtiofs.Start (unit tests). Nil uses the real daemon.
	// Signature: sandbox ID, host directory, socket path. The stop func kills
	// the daemon and removes the socket.
	FSLauncher func(ctx context.Context, sandboxID, hostPath, socketPath string) (func(), error)

	// LocalNet records the per-sandbox egress plan. Nil becomes an in-memory
	// applier (FakeVMM): it does not install kernel routes or WireGuard.
	LocalNet localnet.Applier

	// Workers bounds how many sandboxes are started or stopped at once
	// (ASP_RECONCILE_WORKERS, default DefaultWorkers). A slow boot (CH ready
	// timeout, a multi-GB rootfs copy) no longer delays every other item.
	Workers int

	// StopGrace is how long a stop waits for the guest to power itself off
	// before the VMM is stopped hard (ADR-0012). Zero stops hard at once.
	StopGrace time.Duration
	// MinFreeDiskMiB is the free space --disk-dir must have to clone or resume a
	// disk. Zero does not check.
	MinFreeDiskMiB int64
	// FreeDisk reports free bytes (tests). Nil uses statfs.
	FreeDisk func(dir string) (uint64, error)

	// GuestReadyTimeout is how long a start waits for pod-daemon in the new VM
	// to answer before it reports running (waitGuest). Zero reports running
	// as soon as the VMM is up, as dry-run does: FakeVMM boots no guest.
	GuestReadyTimeout time.Duration

	// inflight holds the sandboxes a worker is handling (mu); the next poll
	// skips them, so one id is never handled by two workers, and a stopping
	// that arrives during a start is handled once the start is done.
	inflight map[string]bool
	sem      chan struct{}
	work     sync.WaitGroup

	mu      sync.Mutex
	handles map[string]Handle
	// retains: the last poll carried the retained list, so stopping keeps a
	// sandbox's disk (mu). lastDiskGC is the last disk sweep (mu).
	retains    bool
	lastDiskGC time.Time
	nextCID    uint32 // next guest CID to assign (starts at 3)
	freeCID    []uint32
	// nextSlot / freeSlot allocate guest /30s like CIDs.
	nextSlot int
	freeSlot []int
	// OnUnknownNode runs when the control plane answers 404 to the work poll: it
	// lost this node (e.g. a memory-store restart). main registers again. It runs
	// at most every unknownNodeEvery.
	OnUnknownNode   func(context.Context)
	lastUnknownNode time.Time
}

// Guest kernel and base image every VM boots (the base is cloned per sandbox
// under DiskDir). The reaper never removes the base image.
const (
	DefaultKernelPath = "/opt/sandbox/vmlinux"
	DefaultRootFSPath = "/opt/sandbox/rootfs.img"
)

// Per-sandbox state on the host. reap.go parses these names back, so the
// reaper removes exactly what ensureRunning creates.
const (
	vsockPrefix    = "vsock-"     // VsockDir: CH vsock muxer, and its {muxer}_{port} hybrid listeners
	virtiofsPrefix = "virtiofs-"  // VsockDir: virtiofsd socket
	sshAgentPrefix = "ssh-agent-" // VsockDir: link to the sandbox's SSH agent upstream
	serialPrefix   = "serial-"    // VsockDir: socket CH serves the guest's serial console on
	rootfsPrefix   = "rootfs-"    // DiskDir: private rootfs copy
)

func vsockName(id string) string    { return vsockPrefix + id + ".sock" }
func virtiofsName(id string) string { return virtiofsPrefix + id + ".sock" }
func sshAgentName(id string) string { return sshAgentPrefix + id + ".sock" }
func serialName(id string) string   { return serialPrefix + id + ".sock" }
func rootfsName(id string) string   { return rootfsPrefix + id + ".img" }

func New(cp *cpclient.Client, nodeID string, engine vmm.MicroVM, logger *slog.Logger, every time.Duration) *Reconciler {
	if logger == nil {
		logger = slog.Default()
	}
	if every <= 0 {
		every = 2 * time.Second
	}
	return &Reconciler{
		CP:         cp,
		NodeID:     nodeID,
		Engine:     engine,
		Logger:     logger,
		Every:      every,
		KernelPath: DefaultKernelPath,
		RootFSPath: DefaultRootFSPath,
		VsockDir:   "/run/asp",
		VsockPort:  poddaemon.DefaultGuestPort,
		handles:    make(map[string]Handle),
		nextCID:    3,
		LocalNet:   localnet.NewMemory(),
	}
}

// unknownNodeEvery rate-limits OnUnknownNode.
const unknownNodeEvery = 10 * time.Second

// DefaultWorkers is how many sandboxes the reconciler handles at once.
const DefaultWorkers = 4

// Run loops until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) {
	r.Logger.Info("reconciler started", "interval", r.Every.String(), "node_id", r.NodeID, "tap_auto", r.TapAuto)
	ticker := time.NewTicker(r.Every)
	defer ticker.Stop()
	// Immediate first pass. Polls do not wait for the workers: a sandbox still
	// being handled is skipped until it is done.
	r.safePoll(ctx)
	for {
		select {
		case <-ctx.Done():
			r.work.Wait()
			r.Logger.Info("reconciler stopped")
			return
		case <-ticker.C:
			r.safePoll(ctx)
		}
	}
}

// tick is one poll and the work it started (tests).
func (r *Reconciler) tick(ctx context.Context) {
	r.safePoll(ctx)
	r.work.Wait()
}

// dispatch runs fn for sandbox id on a worker, unless a worker already
// handles id. It returns whether fn was scheduled. A panic in fn is logged with
// its stack and ends only this item: onPanic (may be nil) then frees what the
// item held and tells the control plane, so the sandbox is not left in a state
// nobody drives, and the agent and the other VMs carry on.
func (r *Reconciler) dispatch(id string, fn func(), onPanic func(value any)) bool {
	r.mu.Lock()
	if r.inflight == nil {
		r.inflight = make(map[string]bool)
	}
	if r.inflight[id] {
		r.mu.Unlock()
		return false
	}
	r.inflight[id] = true
	if r.sem == nil {
		n := r.Workers
		if n <= 0 {
			n = DefaultWorkers
		}
		r.sem = make(chan struct{}, n)
	}
	sem := r.sem
	r.mu.Unlock()

	r.work.Add(1)
	go func() {
		defer r.work.Done()
		sem <- struct{}{}
		defer func() { <-sem }()
		defer func() {
			r.mu.Lock()
			delete(r.inflight, id)
			r.mu.Unlock()
		}()
		defer safe.Recover(r.Logger, "reconciler worker", onPanic, "sandbox_id", id)
		fn()
	}()
	return true
}

// safePoll is poll with a panic contained: a bug in what a poll does (work
// parsing, egress, the disk GC, a callback) costs this poll, not the agent.
func (r *Reconciler) safePoll(ctx context.Context) {
	defer safe.Recover(r.Logger, "reconciler poll", nil)
	r.poll(ctx)
}

func (r *Reconciler) poll(ctx context.Context) {
	work, err := r.CP.ListWork(ctx, r.NodeID)
	if err != nil {
		if cpclient.IsNotFound(err) && r.OnUnknownNode != nil && time.Since(r.lastUnknownNode) >= unknownNodeEvery {
			r.lastUnknownNode = time.Now()
			r.Logger.Warn("control plane does not know this node; registering again", "node_id", r.NodeID)
			r.OnUnknownNode(ctx)
			return
		}
		r.Logger.Warn("list work failed", "error", err)
		return
	}
	r.mu.Lock()
	r.retains = work.Retains()
	r.mu.Unlock()
	for _, sb := range work.Sandboxes {
		sb := sb
		switch sb.State {
		case "requested", "starting", "running":
			if sb.State == "running" && !sb.LocalNet {
				continue
			}
			r.dispatch(sb.ID, func() {
				if err := r.ensureRunning(ctx, sb); err != nil {
					r.Logger.Warn("ensure running failed", "sandbox_id", sb.ID, "error", err)
				}
			}, func(v any) { r.afterStartPanic(sb, v) })
		case "stopping":
			r.dispatch(sb.ID, func() {
				if err := r.ensureStopped(ctx, sb); err != nil {
					r.Logger.Warn("ensure stopped failed", "sandbox_id", sb.ID, "error", err)
				}
			}, func(v any) { r.afterStopPanic(sb, v) })
		case "deleting":
			r.dispatch(sb.ID, func() {
				if err := r.ensureDeleted(ctx, sb); err != nil {
					r.Logger.Warn("ensure deleted failed", "sandbox_id", sb.ID, "error", err)
				}
			}, func(v any) { r.afterDeletePanic(sb, v) })
		}
	}
	r.fenceUnassigned(ctx, work.Assigned)
	r.applyEgress(work.Egress)
	r.gcDisks(work)
}

// applyEgress gives every assigned sandbox its tenant's egress policy, before
// its first exec and again whenever the policy's version changes. A sandbox
// whose policy was dropped (teardown) gets it again.
func (r *Reconciler) applyEgress(e *cpclient.WorkEgress) {
	if r.Egress == nil || e == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.egressVersions == nil {
		r.egressVersions = make(map[string]string)
	}
	for id, tenant := range e.Tenants {
		pol, ok := e.Policies[tenant]
		if !ok {
			continue
		}
		if v, applied := r.egressVersions[id]; applied && v == pol.Version && r.Egress.Get(id) != nil {
			continue
		}
		r.Egress.Set(id, egressAllowlist(pol))
		r.egressVersions[id] = pol.Version
		r.Logger.Info("egress policy applied", "sandbox_id", id, "tenant_id", tenant, "mode", pol.Mode, "rules", len(pol.Rules), "version", pol.Version)
	}
	for id := range r.egressVersions {
		if _, ok := e.Tenants[id]; !ok {
			delete(r.egressVersions, id)
		}
	}
}

// egressAllowlist turns a control-plane policy into the allowlist the proxy
// and the DNS sink use; only enabled rules count.
func egressAllowlist(pol cpclient.EgressPolicy) *egress.Allowlist {
	rules := make([]egress.Rule, 0, len(pol.Rules))
	for _, rule := range pol.Rules {
		if rule.Enabled && rule.HostPattern != "" {
			rules = append(rules, egress.Rule{HostPattern: rule.HostPattern, Port: rule.Port})
		}
	}
	return egress.NewAllowlistFromPolicy(pol.Mode, rules)
}

// fenceUnassigned stops every local VM the control plane no longer assigns to
// this node: it failed the sandbox over (node lost), destroyed it, or never
// placed it here. Two copies of a sandbox must not run. A nil set comes from
// a control plane that does not send it; then nothing is stopped.
func (r *Reconciler) fenceUnassigned(ctx context.Context, assigned []string) {
	if assigned == nil {
		return
	}
	keep := make(map[string]bool, len(assigned))
	for _, id := range assigned {
		keep[id] = true
	}
	r.mu.Lock()
	var gone []string
	for id := range r.handles {
		if !keep[id] {
			gone = append(gone, id)
		}
	}
	r.mu.Unlock()
	for _, id := range gone {
		id := id
		// A worker that is starting or stopping id finishes first; the next
		// poll fences what is left.
		// A panic here is only logged: the next poll fences it again.
		r.dispatch(id, func() {
			r.selfFence(ctx, id, "the control plane no longer assigns it to this node")
		}, nil)
	}
}

func (r *Reconciler) tapMgr() *tap.Manager {
	if r.Tap != nil {
		return r.Tap
	}
	return &tap.Manager{Logger: r.Logger}
}

func (r *Reconciler) ensureRunning(ctx context.Context, sb cpclient.Sandbox) error {
	r.mu.Lock()
	_, have := r.handles[sb.ID]
	r.mu.Unlock()
	if have && sb.State == "starting" {
		if err := r.applyLocalNet(ctx, sb); err != nil {
			_, _ = r.CP.ReportStatus(ctx, sb.ID, "failed", "local-net: "+err.Error())
			return fmt.Errorf("local-net: %w", err)
		}
		// Already started locally; just report running if CP still says starting.
		if _, err := r.CP.ReportStatus(ctx, sb.ID, "running", "reconciler"); err != nil {
			if cpclient.IsConflict(err) {
				r.selfFence(ctx, sb.ID, "running report refused: "+err.Error())
			}
			return err
		}
		return nil
	}
	if have {
		return r.applyLocalNet(ctx, sb)
	}

	// Claim if still requested (or soft-assigned to us).
	if sb.State == "requested" {
		claimed, err := r.CP.Claim(ctx, sb.ID, r.NodeID)
		if err != nil {
			return fmt.Errorf("claim: %w", err)
		}
		sb = claimed
	}

	cfg := r.vmConfig(sb)

	// Each TAP gets its own /30. A shared prefix would send every guest's
	// replies to one TAP and leave the proxy unable to tell sandboxes apart.
	slot := -1
	if r.TapAuto {
		n, gnet, err := r.allocSlot()
		if err != nil {
			r.releaseCID(cfg.VsockCID)
			r.failStart(ctx, sb, fmt.Errorf("guest net: %w", err))
			return fmt.Errorf("guest net: %w", err)
		}
		slot = n
		cfg.Cmdline += " " + gnet.KernelIPArg()
		if err := r.tapMgr().CreateWithCIDR(cfg.TapDevice, gnet.HostCIDR()); err != nil {
			r.releaseSlot(slot)
			r.releaseCID(cfg.VsockCID)
			r.failStart(ctx, sb, fmt.Errorf("tap: %w", err))
			return fmt.Errorf("tap create: %w", err)
		}
		r.Egress.Bind(sb.ID, gnet.Prefix)
	}
	// releaseNet undoes the slot and the proxy binding on a failed start.
	releaseNet := func() {
		r.releaseSlot(slot)
		r.Egress.Forget(sb.ID)
	}

	// Per-sandbox SSH agent upstream (ADR-0007 phase 4) before hybrid attach
	// so ServeConn sees the bound HostSock.
	if r.SSHRegistry != nil {
		path := r.SSHRegistry.Bind(sb.ID, sb.OwnerSub)
		r.Logger.Info("ssh-agent upstream bound",
			"sandbox_id", sb.ID,
			"owner_sub", sb.OwnerSub,
			"host_sock", path,
			"template", r.SSHRegistry.Template != "",
		)
	}

	// Hybrid guest→host acceptors before VMM start so CH can connect as soon
	// as the guest dials CID 2. Distinct paths from the muxer UDS itself.
	if err := r.attachGuestHost(sb.ID, cfg.VsockPath); err != nil {
		if r.SSHRegistry != nil {
			r.SSHRegistry.Unset(sb.ID)
		}
		if r.TapAuto {
			_ = r.tapMgr().Delete(cfg.TapDevice)
		}
		r.releaseCID(cfg.VsockCID)
		releaseNet()
		r.failStart(ctx, sb, fmt.Errorf("guest-host: %w", err))
		return fmt.Errorf("guest-host attach: %w", err)
	}

	fsSock, stopFS, err := r.startWorkspace(ctx, sb)
	if err != nil {
		r.detachGuestHost(sb.ID)
		if r.SSHRegistry != nil {
			r.SSHRegistry.Unset(sb.ID)
		}
		if r.TapAuto {
			_ = r.tapMgr().Delete(cfg.TapDevice)
		}
		r.releaseCID(cfg.VsockCID)
		releaseNet()
		r.failStart(ctx, sb, err)
		return fmt.Errorf("workspace: %w", err)
	}
	cfg.WorkspaceFSSocket = fsSock

	if err := r.applyLocalNet(ctx, sb); err != nil {
		if stopFS != nil {
			stopFS()
		}
		r.detachGuestHost(sb.ID)
		if r.TapAuto {
			_ = r.tapMgr().Delete(cfg.TapDevice)
		}
		r.releaseCID(cfg.VsockCID)
		releaseNet()
		r.failStart(ctx, sb, fmt.Errorf("local-net: %w", err))
		return fmt.Errorf("local-net: %w", err)
	}

	rootfs, baseDigest, resumed, err := r.rootFSFor(sb)
	if err != nil {
		err = fmt.Errorf("rootfs: %w", err)
	} else {
		if rootfs != "" {
			cfg.RootFSPath = rootfs
		}
		err = r.Engine.Start(ctx, cfg)
	}
	if err != nil {
		if !resumed {
			r.removeRootFS(rootfs) // a retained disk survives a failed resume
		}
		_ = r.localApplier().Clear(sb.ID)
		r.mu.Lock()
		delete(r.localNetDone, sb.ID)
		r.mu.Unlock()
		if stopFS != nil {
			stopFS()
		}
		r.detachGuestHost(sb.ID)
		if r.TapAuto {
			_ = r.tapMgr().Delete(cfg.TapDevice)
		}
		r.releaseCID(cfg.VsockCID)
		releaseNet()
		r.failStart(ctx, sb, err)
		return fmt.Errorf("vmm start: %w", err)
	}

	sshSock := r.linkSSHAgent(sb.ID)

	h := Handle{CID: cfg.VsockCID, VsockPath: cfg.VsockPath, TapName: cfg.TapDevice, SSHSock: sshSock, Slot: slot, RootFS: rootfs, BaseDigest: baseDigest, stopFS: stopFS}
	r.mu.Lock()
	r.handles[sb.ID] = h
	r.mu.Unlock()

	r.registerEndpoint(sb.ID, h)
	if !r.waitGuest(ctx, sb.ID) {
		// A guest that never answers (a kernel that cannot find its disk, a broken
		// image, a kernel that no longer matches a resumed disk's modules) is no
		// use: tear the VM down, with what it printed on its console in the log, and
		// report the start failed. A first boot is failed and loses its fresh disk;
		// a resume goes back to stopped with its disk, as for any failed resume.
		console := r.consoleTail(sb.ID)
		err := fmt.Errorf("guest_not_ready: the guest did not answer within %s%s", r.GuestReadyTimeout, lastConsoleLine(console))
		r.teardownLocal(ctx, sb.ID, teardownOpts{keepDisk: resumed})
		r.failStart(ctx, sb, err)
		return fmt.Errorf("start: %w", err)
	}

	if _, err := r.CP.ReportStatus(ctx, sb.ID, "running", "vmm started"); err != nil {
		if cpclient.IsConflict(err) {
			// The boot outlived the assignment (failover or destroy meanwhile).
			r.selfFence(ctx, sb.ID, "running report refused: "+err.Error())
		}
		return fmt.Errorf("report running: %w", err)
	}
	boot := "new"
	if resumed {
		boot = "resume"
	}
	r.postAttestation(ctx, sb, h, boot)
	r.Logger.Info("sandbox running", "sandbox_id", sb.ID, "vsock_cid", h.CID, "vsock_path", h.VsockPath, "tap", h.TapName)
	return nil
}

// measurement is what this boot loaded, as far as it can be said: the kernel the
// VM was started with (hashed now), the base image its disk was copied from
// (hashed when the disk was made, or read back for a resume), and the VMM
// version. What cannot be measured is left empty and the statement says less
// instead of naming something it did not check.
func (r *Reconciler) measurement(sb cpclient.Sandbox, h Handle, boot string) attest.Measurement {
	m := attest.Measurement{ImageDigest: h.BaseDigest, VMMVersion: r.VMMVersion, Boot: boot}
	if r.Measure != nil {
		kernel, err := r.Measure(r.KernelPath)
		if err != nil {
			r.Logger.Warn("attestation: kernel not measured", "sandbox_id", sb.ID, "path", r.KernelPath, "error", err)
		}
		m.KernelDigest = kernel
	}
	return m
}

func (r *Reconciler) postAttestation(ctx context.Context, sb cpclient.Sandbox, h Handle, boot string) {
	m := r.measurement(sb, h, boot)
	profile := sb.VMMProfile
	if profile == "" {
		profile = "cloud-hypervisor"
	}
	if len(r.Attest) == 0 {
		ev, err := attest.SignNow(sb.ID, r.NodeID, profile, h.CID, m)
		if err != nil {
			r.Logger.Warn("attest sign", "sandbox_id", sb.ID, "error", err)
			return
		}
		if err := r.CP.Attest(ctx, sb.ID, ev); err != nil {
			r.Logger.Warn("attest post", "sandbox_id", sb.ID, "error", err)
			return
		}
		r.Logger.Info("attestation posted", "sandbox_id", sb.ID, "cid", h.CID, "key_id", ev.KeyID)
		return
	}
	for i, s := range r.Attest {
		ev, err := s.SignBoot(sb.ID, r.NodeID, profile, h.CID, m)
		if err == nil {
			err = r.CP.Attest(ctx, sb.ID, ev)
		}
		if err == nil {
			r.Logger.Info("attestation posted", "sandbox_id", sb.ID, "cid", h.CID, "key_id", s.KeyID())
			return
		}
		if i == len(r.Attest)-1 {
			r.Logger.Warn("attest post", "sandbox_id", sb.ID, "key_id", s.KeyID(), "error", err)
		} else {
			r.Logger.Info("attestation refused; trying the next key", "sandbox_id", sb.ID, "key_id", s.KeyID(), "error", err)
		}
	}
}

// ensureStopped stops a VM the control plane is stopping. When the control
// plane keeps stopped sandboxes' disks (ADR-0012) the guest powers off first
// and the disk stays; otherwise it is the old stop, which removes the disk.
func (r *Reconciler) ensureStopped(ctx context.Context, sb cpclient.Sandbox) error {
	keep := r.retainsDisks()
	r.teardownLocal(ctx, sb.ID, teardownOpts{keepDisk: keep, graceful: keep})
	detail := "vmm deleted"
	if keep {
		detail = "vmm stopped, disk kept"
	}
	if _, err := r.CP.ReportStatus(ctx, sb.ID, "stopped", detail); err != nil {
		return fmt.Errorf("report stopped: %w", err)
	}
	r.Logger.Info("sandbox stopped", "sandbox_id", sb.ID, "disk_kept", keep)
	return nil
}

// selfFence stops a VM the control plane no longer assigns to this node (it was
// failed over, destroyed or never ours), without reporting: the control plane has
// already moved on, and two copies of a sandbox must not run.
func (r *Reconciler) selfFence(ctx context.Context, id, why string) {
	r.Logger.Warn("self-fencing: stopping a sandbox the control plane no longer assigns here",
		"sandbox_id", id, "node_id", r.NodeID, "reason", why)
	// A control plane that keeps disks decides what happens to this one: a stop
	// the user asked for while the VM was still booting (the running report was
	// refused because the sandbox is already stopping) must keep it, a delete or
	// the disk GC removes it. Removing it here would leave a stopped sandbox that
	// cannot be resumed. An older control plane keeps no disks: remove it as before.
	r.teardownLocal(ctx, id, teardownOpts{keepDisk: r.retainsDisks()})
}

// teardownLocal stops the VM and releases everything it held on this host,
// the disk included unless opts keep it.
func (r *Reconciler) teardownLocal(ctx context.Context, id string, opts teardownOpts) {
	r.mu.Lock()
	h, had := r.handles[id]
	if had {
		delete(r.handles, id)
	}
	r.mu.Unlock()

	if had && opts.graceful {
		r.shutdownGuest(ctx, id)
	}
	if err := r.Engine.Stop(ctx, id); err != nil {
		r.Logger.Warn("vmm stop", "sandbox_id", id, "error", err)
	}
	if had {
		r.releaseCID(h.CID)
		r.releaseSlot(h.Slot)
		r.detachGuestHost(id)
		if r.Registry != nil {
			r.Registry.Unregister(id)
		}
		if h.VsockPath != "" {
			_ = os.Remove(h.VsockPath)
		}
		if h.SSHSock != "" {
			_ = os.Remove(h.SSHSock)
		}
		if h.stopFS != nil {
			h.stopFS()
		}
		if !opts.keepDisk {
			r.removeRootFS(h.RootFS)
		}
		if r.TapAuto && h.TapName != "" {
			if err := r.tapMgr().Delete(h.TapName); err != nil {
				r.Logger.Warn("tap delete", "tap", h.TapName, "error", err)
			}
		}
	}
	r.Egress.Forget(id)
	_ = r.localApplier().Clear(id)
	r.mu.Lock()
	delete(r.localNetDone, id)
	r.mu.Unlock()
}

func (r *Reconciler) localApplier() localnet.Applier {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.LocalNet == nil {
		r.LocalNet = localnet.NewMemory()
	}
	return r.LocalNet
}

// applyLocalNet installs the ADR-0010 plan. local_net=false records the
// public path and does not add a tunnel. local_net=true never records the
// public proxy: pending/withdrawn are blackholes, up creates wg-asp-* and
// points that sandbox's policy table at it. Host.Apply runs ip/wg.
// Memory (tests, FakeVMM default) only records the plan.
func (r *Reconciler) applyLocalNet(ctx context.Context, sb cpclient.Sandbox) error {
	var alloc localnet.Allocation
	if sb.LocalNet {
		// Table, port and /30 are allocated on the node, not hashed from the
		// short id: a collision would route one session into another's tunnel.
		a, err := r.localApplier().Allocation(sb.ID)
		if err != nil {
			return err
		}
		alloc = a
	}
	plan := localnet.DecideWith(sb.ID, sb.OwnerSub, sb.LocalNet, sb.LocalNetState, alloc)
	plan.PeerPublic = strings.TrimSpace(sb.LocalNetClientPublic)
	keyer, hasKeys := r.localApplier().(localnet.NodeKeyer)
	needKey := sb.LocalNet && hasKeys
	now := r.clock()

	r.mu.Lock()
	prev := r.localNetDone[sb.ID]
	r.mu.Unlock()
	// The plan is applied on every running local-net sandbox's work item,
	// every tick. Its recipe is about 15 processes: run it only when the plan
	// changed, and check that the device survived every localNetVerifyEvery.
	if prev != nil && samePlan(prev.plan, plan) && (!needKey || prev.published.key != "") {
		if now.Sub(prev.checked) < localNetVerifyEvery {
			return nil
		}
		if v, ok := r.localApplier().(localnet.Verifier); !ok || v.Healthy(sb.ID) {
			r.mu.Lock()
			prev.checked = now
			r.mu.Unlock()
			return nil
		}
		r.Logger.Warn("local-net device missing; applying the plan again", "sandbox_id", sb.ID, "iface", plan.Iface)
	}

	var published localNetPublished
	if prev != nil {
		published = prev.published
	}
	if needKey {
		pub, _, err := keyer.EnsureNodeKey(sb.ID)
		if err != nil {
			return err
		}
		want := localNetPublished{key: pub, alloc: alloc}
		if r.CP != nil && pub != "" && want != published {
			tun := cpclient.LocalNetTunnel{ListenPort: alloc.ListenPort, NodeAddr: alloc.NodeCIDR(), ClientAddr: alloc.ClientCIDR()}
			if err := r.CP.PublishLocalNetNode(ctx, sb.ID, pub, tun); err != nil {
				r.Logger.Warn("local-net node public", "sandbox_id", sb.ID, "error", err)
			} else {
				published = want
			}
		}
	}
	if err := r.localApplier().Apply(plan); err != nil {
		return err
	}
	r.mu.Lock()
	if r.localNetDone == nil {
		r.localNetDone = make(map[string]*localNetApplied)
	}
	r.localNetDone[sb.ID] = &localNetApplied{plan: plan, published: published, checked: now}
	r.mu.Unlock()
	r.Logger.Info("local-net plan",
		"sandbox_id", sb.ID,
		"owner_sub", sb.OwnerSub,
		"local_net", sb.LocalNet,
		"state", sb.LocalNetState,
		"kind", plan.Kind,
		"public_proxy", plan.UsePublicProxy,
		"iface", plan.Iface,
	)
	return nil
}

// localNetVerifyEvery is how often an unchanged local-net plan's device is
// checked (one `ip link show`) and re-created if it is gone.
const localNetVerifyEvery = 30 * time.Second

// localNetApplied is what the reconciler last applied for a sandbox.
type localNetApplied struct {
	plan      localnet.Plan
	published localNetPublished // what the control plane accepted
	checked   time.Time         // last apply or device check
}

// localNetPublished is the node key and tunnel parameters a grant hands out.
type localNetPublished struct {
	key   string
	alloc localnet.Allocation
}

// samePlan compares plans as the reconciler computes them: the host fills
// KeyPath in while applying.
func samePlan(a, b localnet.Plan) bool {
	a.KeyPath, b.KeyPath = "", ""
	return a == b
}

func (r *Reconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *Reconciler) linkSSHAgent(sandboxID string) string {
	target := r.SSHAgentShared
	if r.SSHRegistry != nil {
		if p, ok := r.SSHRegistry.Get(sandboxID); ok {
			if p == "" {
				// Intentional FakeAgent — no virtiofs symlink.
				return ""
			}
			target = p
		}
	}
	if target == "" {
		return ""
	}
	vsockDir := r.VsockDir
	if vsockDir == "" {
		vsockDir = "/run/asp"
	}
	_ = rundir.Ensure(vsockDir)
	link := filepath.Join(vsockDir, sshAgentName(sandboxID))
	_ = os.Remove(link)
	if err := os.Symlink(target, link); err != nil {
		r.Logger.Warn("ssh-agent symlink", "link", link, "target", target, "error", err)
		return ""
	}
	return link
}

func (r *Reconciler) registerEndpoint(sandboxID string, h Handle) {
	if r.Registry == nil {
		return
	}
	port := r.VsockPort
	if port == 0 {
		port = poddaemon.DefaultGuestPort
	}
	if r.PodDaemonUnix != "" {
		// Dry-run / lab: exec still hits the host unix sock; CID is recorded for parity.
		r.Registry.Register(sandboxID, poddaemon.Endpoint{
			Mode:     poddaemon.ModeUnix,
			UnixPath: r.PodDaemonUnix,
			CID:      h.CID,
			Port:     port,
		})
		return
	}
	r.Registry.Register(sandboxID, poddaemon.Endpoint{
		Mode:      poddaemon.ModeHybrid,
		VsockPath: h.VsockPath,
		CID:       h.CID,
		Port:      port,
	})
}

// guestPoll is how often waitGuest asks a booting guest.
const guestPoll = 200 * time.Millisecond

// waitGuest returns once pod-daemon in a just-started VM answers /healthz, so
// that "running" means an exec reaches the guest. Cloud Hypervisor is up in
// well under a second, but the guest needs a few more to boot (about 3s on a
// KVM host), and "asp sandbox run" execs as soon as it sees running: it got
// "hybrid vsock ACK: EOF". Only hybrid vsock endpoints (real VMs) wait. It
// returns false when it waited and the guest never answered: a first start
// then reports running anyway, as it did before, and a resume stops again.
func (r *Reconciler) waitGuest(ctx context.Context, sandboxID string) bool {
	if r.GuestReadyTimeout <= 0 || r.Registry == nil {
		return true
	}
	if ep, ok := r.Registry.Lookup(sandboxID); !ok || ep.Mode != poddaemon.ModeHybrid {
		return true
	}
	c, err := r.Registry.ClientFor(sandboxID)
	if err != nil {
		return true
	}
	start := time.Now()
	deadline := start.Add(r.GuestReadyTimeout)
	for {
		attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
		err = c.Healthz(attempt)
		cancel()
		if err == nil {
			r.Logger.Info("guest answering", "sandbox_id", sandboxID, "after", time.Since(start).Round(time.Millisecond))
			return true
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(guestPoll):
		}
	}
	r.Logger.Warn("guest did not answer", "sandbox_id", sandboxID,
		"waited", time.Since(start).Round(time.Millisecond), "error", err)
	return false
}

func (r *Reconciler) vmConfig(sb cpclient.Sandbox) vmm.MicroVMConfig {
	cpus := sb.CPUMillis / 1000
	if cpus < 1 {
		cpus = 1
	}
	mem := sb.MemoryMiB
	if mem < 64 {
		mem = 64
	}
	cid := r.allocCID()
	vsockDir := r.VsockDir
	if vsockDir == "" {
		vsockDir = "/run/asp"
	}
	_ = rundir.Ensure(vsockDir)
	vsockPath := filepath.Join(vsockDir, vsockName(sb.ID))
	_ = os.Remove(vsockPath) // drop stale muxer socket before CH binds it
	return vmm.MicroVMConfig{
		ID:         sb.ID,
		KernelPath: r.KernelPath,
		RootFSPath: r.RootFSPath,
		Cmdline:    "console=ttyS0 root=/dev/vda reboot=k panic=1",
		CPUs:       cpus,
		MemoryMiB:  mem,
		TapDevice:  tap.DeviceName(sb.ID),
		VsockCID:   cid,
		VsockPath:  vsockPath,
		// The agent keeps the last of the guest's console for the log of a guest
		// that never comes up. Only Cloud Hypervisor in per-sandbox mode serves it.
		SerialSocket: filepath.Join(vsockDir, serialName(sb.ID)),
		// Host path only. The virtiofsd socket is filled by startWorkspace
		// before Engine.Start when this string is non-empty. Empty means no
		// fs device.
		WorkspaceHostPath: strings.TrimSpace(sb.WorkspaceHostPath),
	}
}

// startWorkspace launches virtiofsd when the spec names a host directory.
// An empty path returns a zero socket and does not look for the binary, so
// sandboxes without a workspace still boot (and FakeVMM smokes stay green).
// A non-empty path fails the start when virtiofsd is missing or the directory
// is not on this node: we do not pretend the guest can see it.
func (r *Reconciler) startWorkspace(ctx context.Context, sb cpclient.Sandbox) (string, func(), error) {
	host := strings.TrimSpace(sb.WorkspaceHostPath)
	if host == "" {
		return "", nil, nil
	}
	// The path is the caller's: it must be inside this tenant's directory under
	// a workspace root, and what virtiofsd shares is the resolved path.
	host, err := r.WorkspaceRoots.Resolve(sb.TenantID, host)
	if err != nil {
		return "", nil, err
	}
	vsockDir := r.VsockDir
	if vsockDir == "" {
		vsockDir = "/run/asp"
	}
	if err := rundir.Ensure(vsockDir); err != nil {
		return "", nil, fmt.Errorf("workspace socket dir: %w", err)
	}
	sock := filepath.Join(vsockDir, virtiofsName(sb.ID))
	var stop func()
	if r.FSLauncher != nil {
		stop, err = r.FSLauncher(ctx, sb.ID, host, sock)
	} else {
		cfg := virtiofs.Config{
			Binary:     r.VirtiofsdBin,
			SocketPath: sock,
			SharedDir:  host,
			Sandbox:    r.VirtiofsdSandbox,
		}
		if r.Confine != nil {
			cfg.Launcher = &r.Confine.Launcher
			cfg.Unit = r.Confine.FSSpec(sb.ID)
		}
		stop, err = virtiofs.Start(ctx, cfg)
	}
	if err != nil {
		if errors.Is(err, virtiofs.ErrNotFound) {
			return "", nil, fmt.Errorf("workspace set but virtiofsd is not installed on this node: %w", err)
		}
		return "", nil, err
	}
	r.Logger.Info("virtiofsd started",
		"sandbox_id", sb.ID,
		"socket", sock,
		"host", host,
		"tag", vmm.WorkspaceVirtiofsTag,
		"guest_mount", vmm.WorkspaceGuestMount,
	)
	return sock, stop, nil
}

func (r *Reconciler) allocCID() uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.freeCID) > 0 {
		cid := r.freeCID[len(r.freeCID)-1]
		r.freeCID = r.freeCID[:len(r.freeCID)-1]
		return cid
	}
	if r.nextCID < 3 {
		r.nextCID = 3
	}
	cid := r.nextCID
	r.nextCID++
	return cid
}

func (r *Reconciler) releaseCID(cid uint32) {
	if cid < 3 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.freeCID = append(r.freeCID, cid)
}

// cloneRootFS makes this sandbox's private disk. It returns "" when DiskDir
// is unset (dry-run), so the shared RootFSPath is used as before.
//
// It also returns the digest of the base image the copy was made from, hashed
// just before the copy and kept next to the disk so a resume can say where the
// disk came from. "" when the image is not measured.
func (r *Reconciler) cloneRootFS(sandboxID string) (path, baseDigest string, err error) {
	if r.DiskDir == "" {
		return "", "", nil
	}
	if err := os.MkdirAll(r.DiskDir, 0o700); err != nil {
		return "", "", err
	}
	dst := filepath.Join(r.DiskDir, rootfsName(sandboxID))
	removeDiskFiles(dst) // a leftover from a crash must not be reused
	if r.Measure != nil {
		d, err := r.Measure(r.RootFSPath)
		if err != nil {
			r.Logger.Warn("attestation: base image not measured", "sandbox_id", sandboxID, "path", r.RootFSPath, "error", err)
		}
		baseDigest = d
	}
	clone := r.CloneDisk
	if clone == nil {
		clone = cloneDisk
	}
	if err := clone(r.RootFSPath, dst); err != nil {
		removeDiskFiles(dst)
		return "", "", err
	}
	if err := os.Chmod(dst, 0o600); err != nil {
		removeDiskFiles(dst)
		return "", "", err
	}
	if baseDigest != "" {
		if err := writeBaseDigest(dst, baseDigest); err != nil {
			r.Logger.Warn("attestation: base image digest not recorded", "sandbox_id", sandboxID, "error", err)
			baseDigest = ""
		}
	}
	return dst, baseDigest, nil
}

func (r *Reconciler) removeRootFS(path string) {
	if path == "" {
		return
	}
	defer removeBaseDigest(path)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		r.Logger.Warn("remove sandbox rootfs", "path", path, "error", err)
	}
}

// cloneDisk is a reflink copy where the filesystem supports it (btrfs, XFS)
// and a sparse copy otherwise, so only used blocks are written.
func cloneDisk(src, dst string) error {
	out, err := exec.Command("cp", "--reflink=auto", "--sparse=always", "--", src, dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("cp %s: %w (%s)", src, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// allocSlot reserves the next free guest /30.
func (r *Reconciler) allocSlot() (int, tap.GuestNet, error) {
	subnet := r.GuestSubnet
	if !subnet.IsValid() {
		subnet = netip.MustParsePrefix(tap.DefaultGuestSubnet)
	}
	r.mu.Lock()
	var n int
	if len(r.freeSlot) > 0 {
		n = r.freeSlot[len(r.freeSlot)-1]
		r.freeSlot = r.freeSlot[:len(r.freeSlot)-1]
	} else {
		n = r.nextSlot
		r.nextSlot++
	}
	r.mu.Unlock()
	gnet, err := tap.Slot(subnet, n)
	if err != nil {
		// Past the end of the pool: give the index back so the counter
		// does not keep growing, but never hand it out as free.
		r.mu.Lock()
		if n == r.nextSlot-1 {
			r.nextSlot--
		}
		r.mu.Unlock()
		return -1, tap.GuestNet{}, err
	}
	return n, gnet, nil
}

func (r *Reconciler) releaseSlot(n int) {
	if n < 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.freeSlot = append(r.freeSlot, n)
}

func (r *Reconciler) attachGuestHost(sandboxID, muxerPath string) error {
	if r.GuestHost == nil || muxerPath == "" {
		return nil
	}
	// Skip hybrid attach in dry-run unix pod-daemon mode (no CH muxer).
	if r.PodDaemonUnix != "" {
		return nil
	}
	return r.GuestHost.AttachSandbox(sandboxID, muxerPath)
}

func (r *Reconciler) detachGuestHost(sandboxID string) {
	if r.GuestHost != nil {
		r.GuestHost.DetachSandbox(sandboxID)
	}
	if r.SSHRegistry != nil {
		r.SSHRegistry.Unset(sandboxID)
	}
}

// Handles returns a copy of locally tracked sandbox IDs (tests).
func (r *Reconciler) Handles() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.handles))
	for id := range r.handles {
		out = append(out, id)
	}
	return out
}

// HandleOf returns the local handle for a sandbox (tests / exec).
func (r *Reconciler) HandleOf(id string) (Handle, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.handles[id]
	return h, ok
}

// consoleTail is the last of what sandbox id's guest wrote to its serial console,
// logged as it is gathered. It must run before the VM is torn down.
func (r *Reconciler) consoleTail(id string) string {
	cr, ok := r.Engine.(vmm.ConsoleReader)
	if !ok {
		return ""
	}
	tail := cr.ConsoleTail(id, 2048)
	if tail == "" {
		r.Logger.Warn("the guest wrote nothing to its console", "sandbox_id", id)
		return ""
	}
	r.Logger.Warn("last output of the guest's console", "sandbox_id", id, "console", tail)
	return tail
}

// ansiSequence is a terminal escape sequence (colours, cursor movement).
var ansiSequence = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// consoleTrouble marks a line that says what went wrong.
var consoleTrouble = regexp.MustCompile(`(?i)kernel panic|vfs:|unable to|cannot |can't |no init|not found|failed|error|oops`)

// consoleLine picks the line of a console tail worth showing in a status detail,
// without its terminal escapes: the last one that says something went wrong
// among the last few, else the last printable one. A kernel that cannot mount
// its root panics and reboots in a loop, so the last line is "Rebooting in 1
// seconds.." and the useful one is just above it. Returns " (console: ...)" or "".
func lastConsoleLine(tail string) string {
	lines := strings.Split(strings.ReplaceAll(ansiSequence.ReplaceAllString(tail, ""), "\r", "\n"), "\n")
	var printable []string
	for _, l := range lines {
		l = strings.TrimSpace(strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, l))
		if l != "" {
			printable = append(printable, l)
		}
	}
	if len(printable) == 0 {
		return ""
	}
	pick := printable[len(printable)-1]
	for i := len(printable) - 1; i >= 0 && i >= len(printable)-40; i-- {
		if consoleTrouble.MatchString(printable[i]) {
			pick = printable[i]
			break
		}
	}
	if len(pick) > 160 {
		pick = pick[:160]
	}
	return fmt.Sprintf(" (console: %s)", pick)
}
