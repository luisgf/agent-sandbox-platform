// Package reconciler claims sandboxes from the control plane and drives the VMM.
package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/attest"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/cpclient"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/poddaemon"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/sshagent"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/tap"
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
	// Tap is the TAP manager (created with SoftFail when nil and TapAuto).
	Tap *tap.Manager

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

	mu      sync.Mutex
	handles map[string]Handle
	nextCID uint32 // next guest CID to assign (starts at 3)
	freeCID []uint32
}

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
		KernelPath: "/opt/sandbox/vmlinux",
		RootFSPath: "/opt/sandbox/rootfs.img",
		VsockDir:   "/run/asp",
		VsockPort:  poddaemon.DefaultGuestPort,
		handles:    make(map[string]Handle),
		nextCID:    3,
	}
}

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
		r.Logger.Warn("list work failed", "error", err)
		return
	}
	for _, sb := range work {
		switch sb.State {
		case "requested", "starting":
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
			r.Logger.Warn("renew lease failed", "sandbox_id", id, "error", err)
		}
	}
}

func (r *Reconciler) tapMgr() *tap.Manager {
	if r.Tap != nil {
		return r.Tap
	}
	return &tap.Manager{Logger: r.Logger, SoftFail: true}
}

func (r *Reconciler) ensureRunning(ctx context.Context, sb cpclient.Sandbox) error {
	r.mu.Lock()
	_, have := r.handles[sb.ID]
	r.mu.Unlock()
	if have && sb.State == "starting" {
		// Already started locally; just report running if CP still says starting.
		if _, err := r.CP.ReportStatus(ctx, sb.ID, "running", "reconciler"); err != nil {
			return err
		}
		return nil
	}
	if have {
		return nil
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

	if r.TapAuto {
		if err := r.tapMgr().Create(cfg.TapDevice); err != nil {
			r.releaseCID(cfg.VsockCID)
			_, _ = r.CP.ReportStatus(ctx, sb.ID, "failed", "tap: "+err.Error())
			return fmt.Errorf("tap create: %w", err)
		}
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
		_, _ = r.CP.ReportStatus(ctx, sb.ID, "failed", "guest-host: "+err.Error())
		return fmt.Errorf("guest-host attach: %w", err)
	}

	if err := r.Engine.Start(ctx, cfg); err != nil {
		r.detachGuestHost(sb.ID)
		if r.TapAuto {
			_ = r.tapMgr().Delete(cfg.TapDevice)
		}
		r.releaseCID(cfg.VsockCID)
		_, _ = r.CP.ReportStatus(ctx, sb.ID, "failed", err.Error())
		return fmt.Errorf("vmm start: %w", err)
	}

	sshSock := r.linkSSHAgent(sb.ID)

	h := Handle{CID: cfg.VsockCID, VsockPath: cfg.VsockPath, TapName: cfg.TapDevice, SSHSock: sshSock}
	r.mu.Lock()
	r.handles[sb.ID] = h
	r.mu.Unlock()

	r.registerEndpoint(sb.ID, h)

	if _, err := r.CP.ReportStatus(ctx, sb.ID, "running", "vmm started"); err != nil {
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
	ev, err := attest.SignNow(sb.ID, r.NodeID, digest, profile, h.CID)
	if err != nil {
		r.Logger.Warn("attest sign", "sandbox_id", sb.ID, "error", err)
		return
	}
	if err := r.CP.Attest(ctx, sb.ID, ev); err != nil {
		r.Logger.Warn("attest post", "sandbox_id", sb.ID, "error", err)
		return
	}
	r.Logger.Info("attestation posted", "sandbox_id", sb.ID, "cid", h.CID)
}

func (r *Reconciler) ensureStopped(ctx context.Context, sb cpclient.Sandbox) error {
	r.mu.Lock()
	h, had := r.handles[sb.ID]
	if had {
		delete(r.handles, sb.ID)
	}
	r.mu.Unlock()

	if err := r.Engine.Stop(ctx, sb.ID); err != nil {
		r.Logger.Warn("vmm stop", "sandbox_id", sb.ID, "error", err)
	}
	if had {
		r.releaseCID(h.CID)
		r.detachGuestHost(sb.ID)
		if r.Registry != nil {
			r.Registry.Unregister(sb.ID)
		}
		if h.VsockPath != "" {
			_ = os.Remove(h.VsockPath)
		}
		if h.SSHSock != "" {
			_ = os.Remove(h.SSHSock)
		}
		if r.TapAuto && h.TapName != "" {
			if err := r.tapMgr().Delete(h.TapName); err != nil {
				r.Logger.Warn("tap delete", "tap", h.TapName, "error", err)
			}
		}
	}

	if _, err := r.CP.ReportStatus(ctx, sb.ID, "stopped", "vmm deleted"); err != nil {
		return fmt.Errorf("report stopped: %w", err)
	}
	r.Logger.Info("sandbox stopped", "sandbox_id", sb.ID)
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
	link := filepath.Join(vsockDir, "ssh-agent-"+sandboxID+".sock")
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
	vsockPath := filepath.Join(vsockDir, "vsock-"+sb.ID+".sock")
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
		// Record the requested host directory for FakeVMM / dry-run.
		// WorkspaceFSSocket stays empty: this process does not spawn virtiofsd,
		// so Cloud Hypervisor will not receive an fs device.
		WorkspaceHostPath: strings.TrimSpace(sb.WorkspaceHostPath),
	}
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
