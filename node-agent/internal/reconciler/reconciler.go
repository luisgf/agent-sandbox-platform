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
	"strings"
	"sync"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/attest"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/cpclient"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/egress"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/localnet"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/poddaemon"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/sshagent"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/tap"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/virtiofs"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
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
	stopFS    func() // stops virtiofsd when a workspace was mounted
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
	// Egress, when set, learns each sandbox's /30 so the forward proxy and
	// the DNS sink apply that sandbox's policy to its traffic.
	Egress *egress.PolicyCache

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

	mu      sync.Mutex
	handles map[string]Handle
	nextCID uint32 // next guest CID to assign (starts at 3)
	freeCID []uint32
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
	rootfsPrefix   = "rootfs-"    // DiskDir: private rootfs copy
)

func vsockName(id string) string    { return vsockPrefix + id + ".sock" }
func virtiofsName(id string) string { return virtiofsPrefix + id + ".sock" }
func sshAgentName(id string) string { return sshAgentPrefix + id + ".sock" }
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

// Run loops until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) {
	r.Logger.Info("reconciler started", "interval", r.Every.String(), "node_id", r.NodeID, "tap_auto", r.TapAuto)
	ticker := time.NewTicker(r.Every)
	defer ticker.Stop()
	// Immediate first pass.
	r.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			r.Logger.Info("reconciler stopped")
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

func (r *Reconciler) tick(ctx context.Context) {
	// Soft lease renew for sandboxes this node already runs (multi-node fencing).
	r.renewLocalLeases(ctx)

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
	for _, sb := range work {
		switch sb.State {
		case "requested", "starting", "running":
			if sb.State == "running" && !sb.LocalNet {
				continue
			}
			if err := r.ensureRunning(ctx, sb); err != nil {
				r.Logger.Warn("ensure running failed", "sandbox_id", sb.ID, "error", err)
			}
		case "stopping":
			if err := r.ensureStopped(ctx, sb); err != nil {
				r.Logger.Warn("ensure stopped failed", "sandbox_id", sb.ID, "error", err)
			}
		}
	}
}

func (r *Reconciler) renewLocalLeases(ctx context.Context) {
	r.mu.Lock()
	ids := make([]string, 0, len(r.handles))
	for id := range r.handles {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	for _, id := range ids {
		if _, err := r.CP.RenewLease(ctx, id, r.NodeID); err != nil {
			if cpclient.IsConflict(err) {
				r.selfFence(ctx, id, "lease renewal refused: "+err.Error())
				continue
			}
			r.Logger.Warn("renew lease failed", "sandbox_id", id, "error", err)
		}
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
			_, _ = r.CP.ReportStatus(ctx, sb.ID, "failed", "guest net: "+err.Error())
			return fmt.Errorf("guest net: %w", err)
		}
		slot = n
		cfg.Cmdline += " " + gnet.KernelIPArg()
		if err := r.tapMgr().CreateWithCIDR(cfg.TapDevice, gnet.HostCIDR()); err != nil {
			r.releaseSlot(slot)
			r.releaseCID(cfg.VsockCID)
			_, _ = r.CP.ReportStatus(ctx, sb.ID, "failed", "tap: "+err.Error())
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
		_, _ = r.CP.ReportStatus(ctx, sb.ID, "failed", "guest-host: "+err.Error())
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
		_, _ = r.CP.ReportStatus(ctx, sb.ID, "failed", err.Error())
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
		_, _ = r.CP.ReportStatus(ctx, sb.ID, "failed", "local-net: "+err.Error())
		return fmt.Errorf("local-net: %w", err)
	}

	rootfs, err := r.cloneRootFS(sb.ID)
	if err != nil {
		err = fmt.Errorf("rootfs: %w", err)
	} else {
		if rootfs != "" {
			cfg.RootFSPath = rootfs
		}
		err = r.Engine.Start(ctx, cfg)
	}
	if err != nil {
		r.removeRootFS(rootfs)
		_ = r.localApplier().Clear(sb.ID)
		if stopFS != nil {
			stopFS()
		}
		r.detachGuestHost(sb.ID)
		if r.TapAuto {
			_ = r.tapMgr().Delete(cfg.TapDevice)
		}
		r.releaseCID(cfg.VsockCID)
		releaseNet()
		_, _ = r.CP.ReportStatus(ctx, sb.ID, "failed", err.Error())
		return fmt.Errorf("vmm start: %w", err)
	}

	sshSock := r.linkSSHAgent(sb.ID)

	h := Handle{CID: cfg.VsockCID, VsockPath: cfg.VsockPath, TapName: cfg.TapDevice, SSHSock: sshSock, Slot: slot, RootFS: rootfs, stopFS: stopFS}
	r.mu.Lock()
	r.handles[sb.ID] = h
	r.mu.Unlock()

	r.registerEndpoint(sb.ID, h)

	if _, err := r.CP.ReportStatus(ctx, sb.ID, "running", "vmm started"); err != nil {
		if cpclient.IsConflict(err) {
			// The boot outlived the assignment (failover or destroy meanwhile).
			r.selfFence(ctx, sb.ID, "running report refused: "+err.Error())
		}
		return fmt.Errorf("report running: %w", err)
	}
	r.postAttestation(ctx, sb, h)
	r.Logger.Info("sandbox running", "sandbox_id", sb.ID, "vsock_cid", h.CID, "vsock_path", h.VsockPath, "tap", h.TapName)
	return nil
}

func (r *Reconciler) postAttestation(ctx context.Context, sb cpclient.Sandbox, h Handle) {
	digest := sb.ImageRef
	if digest == "" {
		digest = r.RootFSPath
	}
	profile := sb.VMMProfile
	if profile == "" {
		profile = "cloud-hypervisor"
	}
	if len(r.Attest) == 0 {
		ev, err := attest.SignNow(sb.ID, r.NodeID, digest, profile, h.CID)
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
		ev, err := s.SignBoot(sb.ID, r.NodeID, digest, profile, h.CID)
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

func (r *Reconciler) ensureStopped(ctx context.Context, sb cpclient.Sandbox) error {
	r.teardownLocal(ctx, sb.ID)
	if _, err := r.CP.ReportStatus(ctx, sb.ID, "stopped", "vmm deleted"); err != nil {
		return fmt.Errorf("report stopped: %w", err)
	}
	r.Logger.Info("sandbox stopped", "sandbox_id", sb.ID)
	return nil
}

// selfFence stops a VM the control plane no longer assigns to this node (it was
// failed over, destroyed or never ours), without reporting: the control plane has
// already moved on, and two copies of a sandbox must not run.
func (r *Reconciler) selfFence(ctx context.Context, id, why string) {
	r.Logger.Warn("self-fencing: stopping a sandbox the control plane no longer assigns here",
		"sandbox_id", id, "node_id", r.NodeID, "reason", why)
	r.teardownLocal(ctx, id)
}

// teardownLocal stops the VM and releases everything it held on this host.
func (r *Reconciler) teardownLocal(ctx context.Context, id string) {
	r.mu.Lock()
	h, had := r.handles[id]
	if had {
		delete(r.handles, id)
	}
	r.mu.Unlock()

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
		r.removeRootFS(h.RootFS)
		if r.TapAuto && h.TapName != "" {
			if err := r.tapMgr().Delete(h.TapName); err != nil {
				r.Logger.Warn("tap delete", "tap", h.TapName, "error", err)
			}
		}
	}
	r.Egress.Forget(id)
	if r.LocalNet != nil {
		_ = r.LocalNet.Clear(id)
	}
}

func (r *Reconciler) localApplier() localnet.Applier {
	if r.LocalNet != nil {
		return r.LocalNet
	}
	r.LocalNet = localnet.NewMemory()
	return r.LocalNet
}

// applyLocalNet installs the ADR-0010 plan. local_net=false records the
// public path and does not add a tunnel. local_net=true never records the
// public proxy: pending/withdrawn are blackholes, up creates wg-asp-* and
// points that sandbox's policy table at it. Host.Apply runs ip/wg.
// Memory (tests, FakeVMM default) only records the plan.
func (r *Reconciler) applyLocalNet(ctx context.Context, sb cpclient.Sandbox) error {
	plan := localnet.Decide(sb.ID, sb.OwnerSub, sb.LocalNet, sb.LocalNetState)
	plan.PeerPublic = strings.TrimSpace(sb.LocalNetClientPublic)
	if sb.LocalNet {
		if ks, ok := r.LocalNet.(localnet.NodeKeyer); ok {
			pub, _, _, err := ks.EnsureNodeKey(sb.ID)
			if err != nil {
				return err
			}
			if r.CP != nil && pub != "" {
				if err := r.CP.PublishLocalNetNode(ctx, sb.ID, pub); err != nil {
					r.Logger.Warn("local-net node public", "sandbox_id", sb.ID, "error", err)
				}
			}
		}
	}
	if err := r.localApplier().Apply(plan); err != nil {
		return err
	}
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
	_ = os.MkdirAll(vsockDir, 0o755)
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
	_ = os.MkdirAll(vsockDir, 0o755)
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
	info, err := os.Stat(host)
	if err != nil {
		return "", nil, fmt.Errorf("workspace host path: %w", err)
	}
	if !info.IsDir() {
		return "", nil, fmt.Errorf("workspace host path %s is not a directory", host)
	}
	vsockDir := r.VsockDir
	if vsockDir == "" {
		vsockDir = "/run/asp"
	}
	if err := os.MkdirAll(vsockDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("workspace socket dir: %w", err)
	}
	sock := filepath.Join(vsockDir, virtiofsName(sb.ID))
	var stop func()
	if r.FSLauncher != nil {
		stop, err = r.FSLauncher(ctx, sb.ID, host, sock)
	} else {
		stop, err = virtiofs.Start(ctx, virtiofs.Config{
			Binary:     r.VirtiofsdBin,
			SocketPath: sock,
			SharedDir:  host,
		})
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
func (r *Reconciler) cloneRootFS(sandboxID string) (string, error) {
	if r.DiskDir == "" {
		return "", nil
	}
	if err := os.MkdirAll(r.DiskDir, 0o700); err != nil {
		return "", err
	}
	dst := filepath.Join(r.DiskDir, rootfsName(sandboxID))
	_ = os.Remove(dst) // a leftover from a crash must not be reused
	clone := r.CloneDisk
	if clone == nil {
		clone = cloneDisk
	}
	if err := clone(r.RootFSPath, dst); err != nil {
		_ = os.Remove(dst)
		return "", err
	}
	if err := os.Chmod(dst, 0o600); err != nil {
		_ = os.Remove(dst)
		return "", err
	}
	return dst, nil
}

func (r *Reconciler) removeRootFS(path string) {
	if path == "" {
		return
	}
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
