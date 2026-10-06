package main

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/capacity"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/cpclient"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/egress"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/egress/mitm"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/execproxy"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/hostvsock"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/identity"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/localnet"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/nftredirect"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/poddaemon"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/reconciler"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/sshagent"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/tap"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

type config struct {
	ControlPlaneURL      string
	ControlPlaneCA       string // --control-plane-ca: CA of the control plane's TLS certificate
	EnrollURL            string // --enroll-url: where to enroll (default: ControlPlaneURL)
	NodeID               string
	CHAPISocket          string
	CHSocketDir          string
	VMMBinary            string
	DryRun               bool
	Endpoint             string
	AgentListen          string
	AgentTLSListen       string
	InsecureAgentListen  bool
	Enroll               bool
	BootstrapToken       string
	CertDir              string
	MTLS                 bool
	PodDaemonSock        string
	PodDaemonPort        uint
	HeartbeatEvery       time.Duration
	EgressEnforce        bool
	SSHAgentBridge       string
	IdentityListen       string
	DefaultSandboxID     string
	TrustSandboxHeader   bool // --insecure-identity-sandbox-header: unbound identity listeners trust X-ASP-Sandbox-ID (lab)
	Reconcile            bool
	ReconcileEvery       time.Duration
	TapAuto              bool
	HostVsock            bool
	HostVsockDir         string // unix factory fallback when AF_VSOCK unavailable / lab
	EgressProxyListen    string
	EgressDNSSink        string
	EgressMITMCA         string
	EgressMITM           bool
	SSHAgentConfirm      bool
	SSHGlobalApprovals   bool   // --insecure-ssh-agent-global-approvals: unscoped approvals for listeners without a sandbox (lab)
	SSHAgentSockTemplate string // ASP_SSH_AGENT_SOCK_TEMPLATE
	MultiUser            bool   // ASP_MULTI_USER=1 → confirm default-on + scoped SSH
	EgressNFTRedirect    bool
	NFTEgressMode        string // soft | enforce
	NFTDNSAction         string // redirect | drop
	NFTHTTPPorts         string
	GuestSubnet          string
	CapacityCPU          int    // --capacity-cpu: cores offered; -1 detect, 0 not enforced
	CapacityMemMiB       int    // --capacity-mem-mib: MiB offered; -1 detect, 0 not enforced
	MaxSandboxes         int    // --max-sandboxes: 0 = no limit
	LocalNetDial         string // --local-net-dial: host[:port] laptops dial for local-net
	InstanceID           string // random per process; sent on every register
	GuestSSHAgentAuto    bool
	VirtiofsdBin         string
	DiskDir              string
	ReapLeftovers        string // --reap-leftovers: on | report | off
	ReapOnly             bool   // --reap-only: clean up and exit, without registering
}

func main() {
	cfg := loadConfig()
	if cfg.ReapOnly {
		os.Exit(reapOnly(cfg))
	}
	slog.Info("node-agent starting",
		"node_id", cfg.NodeID,
		"control_plane_url", cfg.ControlPlaneURL,
		"ch_api_socket", cfg.CHAPISocket,
		"ch_socket_dir", cfg.CHSocketDir,
		"dry_run", cfg.DryRun,
		"enroll", cfg.Enroll,
		"agent_listen", cfg.AgentListen,
		"pod_daemon_sock", cfg.PodDaemonSock,
		"pod_daemon_port", cfg.PodDaemonPort,
		"egress_enforce", cfg.EgressEnforce,
		"ssh_agent_bridge", cfg.SSHAgentBridge,
		"identity_listen", cfg.IdentityListen,
		"insecure_identity_sandbox_header", cfg.TrustSandboxHeader,
		"reconcile", cfg.Reconcile,
		"tap_auto", cfg.TapAuto,
		"host_vsock", cfg.HostVsock,
		"egress_proxy_listen", cfg.EgressProxyListen,
		"egress_dns_sink", cfg.EgressDNSSink,
		"egress_mitm", cfg.EgressMITM,
		"ssh_agent_confirm", cfg.SSHAgentConfirm,
		"insecure_ssh_agent_global_approvals", cfg.SSHGlobalApprovals,
		"ssh_agent_sock_template", cfg.SSHAgentSockTemplate,
		"multi_user", cfg.MultiUser,
		"egress_nft_redirect", cfg.EgressNFTRedirect,
		"nft_egress_mode", cfg.NFTEgressMode,
		"guest_ssh_agent_auto", cfg.GuestSSHAgentAuto,
		"reap_leftovers", cfg.ReapLeftovers,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Before any socket is bound and before registering: the new instance id
	// fails the previous process's sandboxes, so their VMs must be gone too.
	hostLock, err := cleanHost(ctx, cfg)
	if err != nil {
		slog.Error("refusing to start", "error", err)
		os.Exit(1)
	}
	defer hostLock.Release()

	// Enrollment uses a plain (or server-TLS) client with bootstrap token — no client cert yet.
	plain, err := cpclient.NewEnrollHTTPClient(cfg.ControlPlaneCA)
	if err != nil {
		slog.Error("load --control-plane-ca", "error", err)
		os.Exit(1)
	}
	if cfg.Enroll {
		if cfg.BootstrapToken == "" {
			slog.Error("--enroll requires --bootstrap-token or ASP_NODE_BOOTSTRAP_TOKEN")
			os.Exit(2)
		}
		enrollClient := cpclient.New(cfg.EnrollURL, plain)
		resp, err := enrollClient.Enroll(ctx, cfg.BootstrapToken, cpclient.EnrollRequest{
			ID:             cfg.NodeID,
			Name:           cfg.NodeID,
			Endpoint:       cfg.Endpoint,
			AgentEndpoint:  agentEndpointURL(cfg),
			VMMProfiles:    []string{"cloud-hypervisor"},
			CapacityCPU:    cfg.CapacityCPU,
			CapacityMemMiB: cfg.CapacityMemMiB,
		})
		if err != nil {
			slog.Error("enrollment failed", "error", err)
			os.Exit(1)
		}
		if err := cpclient.WriteCerts(cfg.CertDir, resp.ClientCertPEM, resp.ClientKeyPEM, resp.CACertPEM); err != nil {
			slog.Error("write certs", "error", err)
			os.Exit(1)
		}
		if resp.NodeID != "" {
			cfg.NodeID = resp.NodeID
		}
		slog.Info("enrolled", "node_id", cfg.NodeID, "cert_dir", cfg.CertDir, "fingerprint", resp.CertFingerprint)
	}

	httpClient, mtls, err := cpclient.LoadMTLSClient(cfg.CertDir, cfg.MTLS, cfg.ControlPlaneCA)
	if err != nil {
		slog.Error("load mTLS client", "error", err)
		os.Exit(1)
	}
	if mtls {
		slog.Info("using mTLS client certs", "cert_dir", cfg.CertDir)
	}
	cp := cpclient.New(cfg.ControlPlaneURL, httpClient)

	var engine vmm.VMM
	if cfg.DryRun {
		engine = vmm.NewFakeVMM(slog.Default())
		slog.Info("using FakeVMM (dry-run)")
	} else if cfg.CHAPISocket != "" {
		// Legacy shared/debug: talk to a pre-started CH on one socket (no spawn).
		engine = vmm.NewCloudHypervisor(cfg.VMMBinary, cfg.CHAPISocket)
		slog.Info("using Cloud Hypervisor shared API socket", "socket", cfg.CHAPISocket, "binary", cfg.VMMBinary)
	} else {
		// Default: spawn one cloud-hypervisor per sandbox under --ch-socket-dir.
		engine = vmm.NewSpawningCloudHypervisor(cfg.VMMBinary, cfg.CHSocketDir)
		slog.Info("using Cloud Hypervisor per-sandbox spawn", "socket_dir", cfg.CHSocketDir, "binary", cfg.VMMBinary)
	}

	var podClient *poddaemon.Client
	var fallbackDialer poddaemon.Dialer
	if cfg.PodDaemonSock != "" {
		fallbackDialer = &poddaemon.UnixDialer{Path: cfg.PodDaemonSock}
		podClient = poddaemon.NewClient(cfg.PodDaemonSock)
		if err := podClient.Healthz(ctx); err != nil {
			slog.Warn("pod-daemon healthz failed (will still proxy)", "error", err, "sock", cfg.PodDaemonSock)
		} else {
			slog.Info("pod-daemon reachable", "sock", cfg.PodDaemonSock)
		}
	}
	pdRegistry := poddaemon.NewRegistry(fallbackDialer)

	defaultAL := egress.NewAllowlistFromPolicy("deny-default", nil)
	policyCache := &egress.PolicyCache{}
	proxy := &execproxy.Server{
		Pod:              podClient,
		Registry:         pdRegistry,
		Logger:           slog.Default(),
		EgressEnforce:    cfg.EgressEnforce,
		DefaultAllowlist: defaultAL,
		PolicyCache:      policyCache,
	}
	httpSrv, ln, err := execproxy.ListenAndServe(cfg.AgentListen, proxy.Handler())
	if err != nil {
		slog.Error("exec proxy listen", "error", err)
		os.Exit(1)
	}
	defer func() {
		_ = httpSrv.Close()
		_ = ln.Close()
	}()
	slog.Info("exec proxy listening", "addr", ln.Addr().String())

	if cfg.AgentTLSListen != "" {
		tlsCfg, err := execproxy.MTLSConfig(
			filepath.Join(cfg.CertDir, "client.crt"),
			filepath.Join(cfg.CertDir, "client.key"),
			filepath.Join(cfg.CertDir, "ca.crt"),
		)
		if err != nil {
			slog.Error("--agent-tls-listen needs the enrolled node certificate (run with --enroll)", "cert_dir", cfg.CertDir, "error", err)
			os.Exit(1)
		}
		tlsSrv, tlsLn, err := execproxy.ListenAndServeTLS(cfg.AgentTLSListen, proxy.RemoteHandler(), tlsCfg)
		if err != nil {
			slog.Error("agent TLS listen", "error", err)
			os.Exit(1)
		}
		defer func() {
			_ = tlsSrv.Close()
			_ = tlsLn.Close()
		}()
		slog.Info("control-plane exec API listening (mTLS)", "addr", tlsLn.Addr().String(), "advertised", agentEndpointURL(cfg))
	}

	if cfg.EgressProxyListen != "" {
		if !cfg.EgressEnforce {
			slog.Warn("egress proxy listen set but --egress-enforce is off; proxy will still deny non-allowlisted")
		}
		fp := &egress.ForwardProxy{
			Default:   defaultAL,
			Cache:     policyCache,
			Logger:    slog.Default(),
			Enforce:   true, // proxy always deny-by-default when listening
			RateLimit: egress.NewTokenBucket(egress.DefaultRate, egress.DefaultBurst),
		}
		if cfg.EgressMITM {
			_ = os.Setenv("ASP_EGRESS_MITM", "1")
			ca, err := mitm.LoadOrGenerate(cfg.EgressMITMCA)
			if err != nil {
				slog.Error("egress mitm ca", "error", err)
				os.Exit(1)
			}
			fp.MITM = ca
			slog.Warn("egress MITM CONNECT bump ENABLED — corp caution; guests must trust MITM CA")
		}
		go func() {
			slog.Info("egress HTTP forward proxy listening", "addr", cfg.EgressProxyListen, "mitm", cfg.EgressMITM)
			if err := fp.ListenAndServe(ctx, cfg.EgressProxyListen); err != nil {
				slog.Error("egress proxy stopped", "error", err)
			}
		}()
	}
	if cfg.EgressDNSSink != "" {
		sink := &egress.DNSSink{
			Allowlist: defaultAL,
			Cache:     policyCache,
			Logger:    slog.Default(),
			Enforce:   true,
		}
		go func() {
			slog.Info("egress DNS sink listening", "addr", cfg.EgressDNSSink)
			if err := sink.ListenAndServe(ctx, cfg.EgressDNSSink); err != nil {
				slog.Error("egress dns sink stopped", "error", err)
			}
		}()
	}

	if cfg.EgressNFTRedirect {
		proxyPort := 8888
		if cfg.EgressProxyListen != "" {
			// best-effort parse :PORT from listen addr
			if i := strings.LastIndex(cfg.EgressProxyListen, ":"); i >= 0 {
				if p, err := strconv.Atoi(cfg.EgressProxyListen[i+1:]); err == nil && p > 0 {
					proxyPort = p
				}
			}
		}
		dnsSinkPort := 5353
		if cfg.EgressDNSSink != "" {
			if i := strings.LastIndex(cfg.EgressDNSSink, ":"); i >= 0 {
				if p, err := strconv.Atoi(cfg.EgressDNSSink[i+1:]); err == nil && p > 0 {
					dnsSinkPort = p
				}
			}
		}
		mode := nftredirect.ModeSoft
		if strings.EqualFold(cfg.NFTEgressMode, "enforce") {
			mode = nftredirect.ModeEnforce
		}
		if err := nftredirect.Apply(nftredirect.Config{
			GuestSubnet: cfg.GuestSubnet,
			ProxyPort:   proxyPort,
			DNSSinkPort: dnsSinkPort,
			HTTPPorts:   cfg.NFTHTTPPorts,
			DNSAction:   cfg.NFTDNSAction,
			Mode:        mode,
			Logger:      slog.Default(),
		}); err != nil {
			slog.Error("egress-nft-redirect", "error", err, "mode", mode)
			os.Exit(1)
		}
	}

	// Per-sandbox SSH agent upstream registry (ADR-0007 phase 4).
	sshFallback := os.Getenv("SSH_AUTH_SOCK")
	sshRegistry := sshagent.NewRegistry(cfg.SSHAgentSockTemplate, sshFallback)
	if cfg.SSHAgentSockTemplate != "" {
		slog.Info("ssh-agent sock template enabled (per-sandbox/owner upstream)",
			"template", cfg.SSHAgentSockTemplate,
			"note", "missing path → FakeAgent; keys provisioned outside ASP")
	}

	var sshApprover *sshagent.Approver
	if cfg.SSHAgentConfirm {
		sshApprover = sshagent.NewApprover(30 * time.Second)
		sshApprover.GlobalApprovals = cfg.SSHGlobalApprovals
		proxy.SSHApprover = sshApprover
		slog.Info("ssh-agent confirmation gate enabled",
			"approve", "POST /v1/internal/ssh-agent/approve",
			"approvals", "per sandbox (sandbox_id)",
			"multi_user", cfg.MultiUser)
		switch {
		case cfg.SSHGlobalApprovals:
			slog.Warn("--insecure-ssh-agent-global-approvals: an approval without sandbox_id unlocks one sign on --ssh-agent-bridge or the global host-vsock listener, from whichever guest asks first (lab only)")
		case cfg.SSHAgentBridge != "":
			slog.Warn("--ssh-agent-bridge cannot tell guests apart: with --ssh-agent-confirm every sign through it is denied; guests sign through their sandbox's host-vsock acceptor, or use --insecure-ssh-agent-global-approvals (lab only)")
		}
	} else if cfg.SSHGlobalApprovals {
		slog.Warn("--insecure-ssh-agent-global-approvals has no effect without --ssh-agent-confirm")
	}

	var sshBridge *sshagent.Bridge
	if cfg.SSHAgentBridge != "" {
		sshBridge = &sshagent.Bridge{
			ListenPath: cfg.SSHAgentBridge,
			HostSock:   sshFallback,
			Logger:     slog.Default(),
			Confirm:    sshApprover,
		}
		if err := sshBridge.Start(); err != nil {
			slog.Error("ssh-agent bridge", "error", err)
			os.Exit(1)
		}
		defer sshBridge.Close()
		slog.Info("ssh-agent bridge listening", "path", cfg.SSHAgentBridge,
			"host_sock", sshFallback != "",
			"confirm", sshApprover != nil)
	}

	idProxy := &identity.Proxy{
		ControlPlaneURL:    cfg.ControlPlaneURL,
		HTTP:               httpClient,
		TrustSandboxHeader: cfg.TrustSandboxHeader,
		DefaultSandboxID:   cfg.DefaultSandboxID,
		Logger:             slog.Default(),
	}
	// Only the per-sandbox hybrid acceptors know which guest is calling;
	// --identity-listen and the global host-vsock listeners do not.
	if cfg.TrustSandboxHeader {
		slog.Warn("--insecure-identity-sandbox-header: identity listeners without a sandbox binding trust the client's X-ASP-Sandbox-ID; anyone who reaches them can mint any sandbox's token (lab only)")
	} else if cfg.IdentityListen != "" || cfg.DefaultSandboxID != "" {
		slog.Warn("--identity-listen is not bound to a sandbox and --default-sandbox-id is ignored: token requests there get 403 without --insecure-identity-sandbox-header (lab only); guests use their sandbox's host-vsock port 26502",
			"identity_listen", cfg.IdentityListen, "default_sandbox_id", cfg.DefaultSandboxID)
	}

	var idLn interface{ Close() error }
	if cfg.IdentityListen != "" {
		if filepath.Ext(cfg.IdentityListen) == ".sock" || cfg.IdentityListen[0] == '/' {
			ln, srv, err := identity.ListenUnix(cfg.IdentityListen, idProxy.Handler())
			if err != nil {
				slog.Error("identity proxy listen", "error", err)
				os.Exit(1)
			}
			defer func() { _ = srv.Close(); _ = ln.Close() }()
			idLn = ln
			slog.Info("identity proxy listening (unix)", "path", cfg.IdentityListen)
		} else {
			ln, srv, err := identity.ListenTCP(cfg.IdentityListen, idProxy.Handler())
			if err != nil {
				slog.Error("identity proxy listen", "error", err)
				os.Exit(1)
			}
			defer func() { _ = srv.Close(); _ = ln.Close() }()
			idLn = ln
			slog.Info("identity proxy listening (tcp)", "addr", ln.Addr().String())
		}
		_ = idLn
	}

	var hvSvc *hostvsock.Service
	if cfg.HostVsock {
		factory, err := hostVsockFactory(cfg)
		if err != nil {
			slog.Error("host-vsock factory", "error", err)
			os.Exit(1)
		}
		hvSvc = &hostvsock.Service{
			Factory:         factory,
			SSHHostSock:     sshFallback,
			SSHConfirm:      sshApprover,
			SSHRegistry:     sshRegistry,
			IdentityHandler: idProxy.Handler(),
			Logger:          slog.Default(),
		}
		if err := hvSvc.Start(); err != nil {
			slog.Error("host-vsock start", "error", err)
			os.Exit(1)
		}
		defer hvSvc.Close()
		slog.Info("host-vsock guest→host services up",
			"ssh_port", hostvsock.PortSSHAgent,
			"identity_port", hostvsock.PortIdentity,
			"guest_dial_cid", hostvsock.HostCID,
			"hybrid_attach", "per-sandbox on reconciler Start ({vsock}_{port})",
		)
	}

	if cfg.GuestSSHAgentAuto {
		if !cfg.HostVsock && cfg.SSHAgentBridge == "" {
			slog.Warn("guest-ssh-agent-auto set but neither --host-vsock nor --ssh-agent-bridge; guest unit will have nothing to dial")
		} else {
			slog.Info("guest-ssh-agent-auto: guest image should run ssh-agent-vsock.service",
				"guest_sock", "/run/agent-sandbox/ssh-agent.sock",
				"host_cid", hostvsock.HostCID,
				"host_port", hostvsock.PortSSHAgent,
				"env_SSH_AUTH_SOCK", "/run/agent-sandbox/ssh-agent.sock",
			)
		}
	}

	register := func(ctx context.Context) error {
		return cp.Register(ctx, registerRequest(cfg))
	}
	if err := register(ctx); err != nil {
		slog.Error("control-plane registration failed", "error", err)
		os.Exit(1)
	}
	slog.Info("registered with control plane", "node_id", cfg.NodeID, "agent_endpoint", agentEndpointURL(cfg),
		"capacity_cpu", cfg.CapacityCPU, "capacity_mem_mib", cfg.CapacityMemMiB, "max_sandboxes", cfg.MaxSandboxes,
		"accepts_work", cfg.Reconcile, "local_net_dial", cfg.LocalNetDial)

	if err := cp.Heartbeat(ctx, cfg.NodeID); err != nil {
		slog.Warn("initial heartbeat failed", "error", err)
	}

	if !cfg.DryRun && cfg.CHAPISocket != "" {
		if err := engine.Ping(ctx); err != nil {
			slog.Warn("CH ping failed (is cloud-hypervisor running with --api-socket?)", "error", err)
		}
	}

	var rec *reconciler.Reconciler
	if cfg.Reconcile {
		micro, ok := engine.(vmm.MicroVM)
		if !ok {
			slog.Error("engine does not implement MicroVM")
			os.Exit(1)
		}
		rec = reconciler.New(cp, cfg.NodeID, micro, slog.Default(), cfg.ReconcileEvery)
		rec.OnUnknownNode = func(ctx context.Context) {
			if err := register(ctx); err != nil {
				slog.Warn("re-register failed", "error", err)
			}
		}
		keyDir := localNetKeyDir(cfg)
		rec.LocalNet = localnet.NewHost(keyDir)
		slog.Info("local-net host applier", "key_dir", keyDir, "note", "needs wireguard-tools and CAP_NET_ADMIN; does not change the host default route")
		rec.VsockDir = cfg.CHSocketDir
		if cfg.PodDaemonPort > 0 {
			rec.VsockPort = uint32(cfg.PodDaemonPort)
		}
		rec.Registry = pdRegistry
		if cfg.DryRun {
			rec.PodDaemonUnix = cfg.PodDaemonSock
		}
		rec.TapAuto = cfg.TapAuto
		if cfg.TapAuto {
			rec.Tap = tapManager(cfg)
			subnet, err := netip.ParsePrefix(strings.TrimSpace(cfg.GuestSubnet))
			if err != nil {
				slog.Error("invalid --guest-subnet", "value", cfg.GuestSubnet, "error", err)
				os.Exit(2)
			}
			rec.GuestSubnet = subnet
		}
		rec.Egress = policyCache
		rec.SSHAgentShared = cfg.SSHAgentBridge
		rec.SSHRegistry = sshRegistry
		rec.VirtiofsdBin = cfg.VirtiofsdBin
		if !cfg.DryRun {
			// Never boot the shared image writable: every VM gets its own copy.
			rec.DiskDir = cfg.DiskDir
		}
		if hvSvc != nil {
			rec.GuestHost = hvSvc
			slog.Info("reconciler will attach CH hybrid guest→host acceptors per sandbox",
				"ssh_scoped", cfg.SSHAgentSockTemplate != "" || cfg.MultiUser)
		}
		go rec.Run(ctx)
	} else if cfg.DryRun {
		// Legacy smoke without reconciler: one-shot FakeVMM create/boot demo.
		demo := vmm.MicroVMConfig{
			ID:         "dry-run-demo",
			KernelPath: "/opt/sandbox/vmlinux",
			RootFSPath: "/opt/sandbox/rootfs.img",
			CPUs:       1,
			MemoryMiB:  256,
			TapDevice:  "asp-demo0000",
			VsockCID:   3,
			VsockPath:  "/tmp/dry-run-vsock.sock",
		}
		if err := engine.CreateVM(ctx, demo); err != nil {
			slog.Error("dry-run CreateVM", "error", err)
			os.Exit(1)
		}
		if err := engine.Boot(ctx); err != nil {
			slog.Error("dry-run Boot", "error", err)
			os.Exit(1)
		}
		slog.Info("dry-run create/boot complete; serving exec proxy until signal")
	} else {
		slog.Info("node-agent idle; pass --reconcile to claim sandboxes")
	}

	ticker := time.NewTicker(cfg.HeartbeatEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("node-agent shutting down")
			return
		case <-ticker.C:
			if err := cp.Heartbeat(ctx, cfg.NodeID); err != nil {
				if cpclient.IsNotFound(err) {
					// The control plane lost this node (e.g. a memory-store restart).
					slog.Warn("control plane does not know this node; registering again", "node_id", cfg.NodeID)
					if err := register(ctx); err != nil {
						slog.Warn("re-register failed", "error", err)
					}
					continue
				}
				slog.Warn("heartbeat failed", "error", err)
			}
		}
	}
}

func hostVsockFactory(cfg config) (hostvsock.ListenerFactory, error) {
	if cfg.HostVsockDir != "" {
		return hostvsock.UnixFactory{Dir: cfg.HostVsockDir}, nil
	}
	if cfg.DryRun {
		dir := filepath.Join(os.TempDir(), "asp-host-vsock")
		return hostvsock.UnixFactory{Dir: dir}, nil
	}
	// Prefer AF_VSOCK; if /dev/vsock missing, fall back to unix under socket dir.
	if _, err := os.Stat("/dev/vsock"); err != nil {
		dir := cfg.CHSocketDir
		if dir == "" {
			dir = "/run/asp"
		}
		slog.Warn("/dev/vsock unavailable; host-vsock using unix factory", "dir", dir)
		return hostvsock.UnixFactory{Dir: dir}, nil
	}
	return hostvsock.AFVsockFactory{}, nil
}

func agentEndpointURL(cfg config) string {
	if cfg.Endpoint != "" && cfg.Endpoint != "http://127.0.0.1:0" {
		return cfg.Endpoint
	}
	return "http://" + cfg.AgentListen
}

func loadConfig() config {
	var cfg config
	flag.StringVar(&cfg.ControlPlaneURL, "control-plane-url", getenv("CONTROL_PLANE_URL", "http://127.0.0.1:8080"), "control plane base URL")
	flag.StringVar(&cfg.ControlPlaneCA, "control-plane-ca", os.Getenv("ASP_CONTROL_PLANE_CA"), "PEM CA that signed the control plane's TLS certificate (enroll and API calls); default: cert-dir/ca.crt, then system roots")
	flag.StringVar(&cfg.EnrollURL, "enroll-url", os.Getenv("ASP_ENROLL_URL"), "control-plane URL for --enroll when it differs from --control-plane-url (ASP_MTLS_STRICT serves enroll on a separate listener)")
	flag.StringVar(&cfg.NodeID, "node-id", os.Getenv("NODE_ID"), "node identifier")
	flag.StringVar(&cfg.CHAPISocket, "ch-api-socket", os.Getenv("CH_API_SOCKET"), "optional shared CH --api-socket (legacy/debug); empty = per-sandbox spawn via --ch-socket-dir")
	flag.StringVar(&cfg.CHSocketDir, "ch-socket-dir", getenv("CH_SOCKET_DIR", "/run/asp"), "directory for per-sandbox CH API sockets (ch-{sandboxID}.sock)")
	flag.StringVar(&cfg.VMMBinary, "ch-binary", getenv("CLOUD_HYPERVISOR_BIN", "cloud-hypervisor"), "cloud-hypervisor binary path (spawned per sandbox when not using --ch-api-socket)")
	flag.StringVar(&cfg.VirtiofsdBin, "virtiofsd-bin", getenv("VIRTIOFSD_BIN", "virtiofsd"), "Rust virtiofsd binary; started per sandbox only when workspace_host_path is set")
	flag.StringVar(&cfg.DiskDir, "disk-dir", getenv("ASP_DISK_DIR", "/var/lib/asp/disks"), "per-sandbox rootfs copies (rootfs-{id}.img, deleted on stop); ignored with --dry-run")
	flag.BoolVar(&cfg.DryRun, "dry-run", getenv("DRY_RUN", "") == "1", "use FakeVMM and skip real CH")
	flag.StringVar(&cfg.ReapLeftovers, "reap-leftovers", getenv("ASP_REAP_LEFTOVERS", reapOn), "at start, remove what a previous node-agent left on this host: cloud-hypervisor and virtiofsd processes of --ch-socket-dir, its per-sandbox sockets, asp-* TAPs, wg-asp-* tunnels and their routing, rootfs copies in --disk-dir. on | report (log, remove nothing) | off; --dry-run only reports")
	flag.BoolVar(&cfg.ReapOnly, "reap-only", false, "remove those leftovers and exit without registering (systemd ExecStopPost); refused while a node-agent runs with this --ch-socket-dir")
	flag.StringVar(&cfg.Endpoint, "endpoint", getenv("NODE_ENDPOINT", ""), "node callback endpoint advertised to control plane")
	flag.StringVar(&cfg.AgentListen, "agent-listen", getenv("ASP_AGENT_LISTEN", "127.0.0.1:9100"), "loopback listen addr for the exec proxy and operator routes (ssh-agent approve, egress-check); plain HTTP, no authentication")
	flag.StringVar(&cfg.AgentTLSListen, "agent-tls-listen", os.Getenv("ASP_AGENT_TLS_LISTEN"), "listen addr for the control plane's mTLS exec API (e.g. 0.0.0.0:9443) when the control plane runs on another host; uses the enrolled node certificate")
	flag.BoolVar(&cfg.InsecureAgentListen, "insecure-agent-listen", getenv("ASP_INSECURE_AGENT_LISTEN", "") == "1", "allow --agent-listen on a non-loopback address (plain HTTP, no authentication; lab only)")
	flag.BoolVar(&cfg.Enroll, "enroll", getenv("ASP_ENROLL", "") == "1", "perform bootstrap enrollment before register")
	flag.StringVar(&cfg.BootstrapToken, "bootstrap-token", os.Getenv("ASP_NODE_BOOTSTRAP_TOKEN"), "bootstrap token for enrollment")
	flag.StringVar(&cfg.CertDir, "cert-dir", getenv("ASP_CERT_DIR", "/var/lib/asp/node-certs"), "directory for node client certs")
	flag.BoolVar(&cfg.MTLS, "mtls", getenv("ASP_MTLS", "") == "1", "require mTLS client certs for control-plane calls")
	flag.StringVar(&cfg.PodDaemonSock, "pod-daemon-sock", os.Getenv("ASP_POD_DAEMON_SOCK"), "unix socket path for pod-daemon (dry-run / local fallback)")
	flag.UintVar(&cfg.PodDaemonPort, "pod-daemon-port", 26500, "guest vsock/TCP port for pod-daemon HTTP (CH hybrid CONNECT)")
	flag.BoolVar(&cfg.EgressEnforce, "egress-enforce", getenv("ASP_EGRESS_ENFORCE", "") == "1", "return 403 on /v1/internal/egress-check denials; required intent for --egress-proxy-listen")
	flag.StringVar(&cfg.EgressProxyListen, "egress-proxy-listen", os.Getenv("ASP_EGRESS_PROXY_LISTEN"), "optional HTTP forward proxy listen (e.g. :8888); guests set HTTP_PROXY to host TAP IP:port")
	flag.StringVar(&cfg.EgressDNSSink, "egress-dns-sink", os.Getenv("ASP_EGRESS_DNS_SINK"), "optional UDP DNS sink (e.g. :5353) that NXDOMAIN non-allowlisted names")
	flag.StringVar(&cfg.EgressMITMCA, "egress-mitm-ca", os.Getenv("ASP_EGRESS_MITM_CA"), "optional path to MITM CA PEM (generate/load); used only with --egress-mitm / ASP_EGRESS_MITM=1")
	flag.BoolVar(&cfg.EgressMITM, "egress-mitm", getenv("ASP_EGRESS_MITM", "") == "1", "ENABLE CONNECT TLS bump (corp caution; default off)")
	flag.StringVar(&cfg.SSHAgentBridge, "ssh-agent-bridge", os.Getenv("ASP_SSH_AGENT_BRIDGE"), "unix socket path for SSH agent bridge (proxies SSH_AUTH_SOCK or FakeAgent)")
	flag.StringVar(&cfg.IdentityListen, "identity-listen", os.Getenv("ASP_IDENTITY_LISTEN"), "unix path (.sock) or TCP addr for guest OIDC identity proxy")
	flag.StringVar(&cfg.DefaultSandboxID, "default-sandbox-id", os.Getenv("ASP_SANDBOX_ID"), "sandbox for identity requests without X-ASP-Sandbox-ID on listeners not bound to a sandbox; only with --insecure-identity-sandbox-header (dry-run)")
	flag.BoolVar(&cfg.TrustSandboxHeader, "insecure-identity-sandbox-header", getenv("ASP_INSECURE_IDENTITY_SANDBOX_HEADER", "") == "1", "let identity listeners not bound to a sandbox (--identity-listen, global --host-vsock) take the sandbox from the client's X-ASP-Sandbox-ID, then --default-sandbox-id; anyone who reaches them can mint any sandbox's token (lab only)")
	flag.BoolVar(&cfg.Reconcile, "reconcile", getenv("ASP_RECONCILE", "") == "1", "poll control-plane work and drive VMM lifecycle")
	flag.BoolVar(&cfg.TapAuto, "tap-auto", getenv("ASP_TAP_AUTO", "") == "1", "create/delete asp-{shortid} TAP around VMM Start/Stop (soft-fail without CAP_NET_ADMIN)")
	flag.BoolVar(&cfg.HostVsock, "host-vsock", getenv("ASP_HOST_VSOCK", "") == "1", "guest→host SSH(26501)+identity(26502): AF_VSOCK/unix lab + per-sandbox CH hybrid {vsock}_{port}")
	flag.StringVar(&cfg.HostVsockDir, "host-vsock-dir", os.Getenv("ASP_HOST_VSOCK_DIR"), "if set, use unix sockets under this dir instead of AF_VSOCK (lab)")
	flag.BoolVar(&cfg.SSHAgentConfirm, "ssh-agent-confirm", false, "require POST /v1/internal/ssh-agent/approve with the signing sandbox's sandbox_id before each SignRequest (one-shot TTL); default on in multi-user")
	flag.BoolVar(&cfg.SSHGlobalApprovals, "insecure-ssh-agent-global-approvals", getenv("ASP_INSECURE_SSH_AGENT_GLOBAL_APPROVALS", "") == "1", "with --ssh-agent-confirm, accept approvals without sandbox_id; they unlock one sign on listeners that cannot tell guests apart (--ssh-agent-bridge, global --host-vsock), from whichever guest asks first (lab only)")
	flag.StringVar(&cfg.SSHAgentSockTemplate, "ssh-agent-sock-template", os.Getenv("ASP_SSH_AGENT_SOCK_TEMPLATE"), "per-sandbox SSH agent upstream path template ({owner_sub}/{sandbox_id}/{id}); missing → FakeAgent")
	flag.BoolVar(&cfg.MultiUser, "multi-user", getenv("ASP_MULTI_USER", "") == "1" || getenv("ASP_IDP_REQUIRED", "") == "1", "multi-user profile: SSH confirm default-on + prefer scoped agent socks")
	flag.BoolVar(&cfg.EgressNFTRedirect, "egress-nft-redirect", getenv("ASP_EGRESS_NFT_REDIRECT", "") == "1" || getenv("ASP_NFT_EGRESS_REDIRECT", "") == "1", "apply nftables guest HTTP+DNS redirect (see --nft-egress-mode)")
	flag.BoolVar(&cfg.EgressNFTRedirect, "nft-egress-redirect", getenv("ASP_NFT_EGRESS_REDIRECT", "") == "1" || getenv("ASP_EGRESS_NFT_REDIRECT", "") == "1", "alias of --egress-nft-redirect (Fase 2e)")
	flag.StringVar(&cfg.NFTEgressMode, "nft-egress-mode", getenv("ASP_NFT_EGRESS_MODE", "soft"), "nft redirect failure mode: soft (SoftFail) | enforce (fail hard)")
	flag.StringVar(&cfg.NFTDNSAction, "nft-dns-action", getenv("ASP_NFT_DNS_ACTION", "redirect"), "guest DNS handling: redirect (to --egress-dns-sink port) | drop")
	flag.StringVar(&cfg.NFTHTTPPorts, "nft-http-ports", getenv("ASP_NFT_HTTP_PORTS", "80,443"), "comma-separated guest TCP ports redirected to egress proxy")
	flag.IntVar(&cfg.CapacityCPU, "capacity-cpu", getenvInt("ASP_CAPACITY_CPU", capacity.Detected), "CPU cores offered to sandboxes: -1 detects, 0 is not enforced (the control plane overcommits CPU)")
	flag.IntVar(&cfg.CapacityMemMiB, "capacity-mem-mib", getenvInt("ASP_CAPACITY_MEM_MIB", capacity.Detected), "memory offered to sandboxes in MiB: -1 detects MemTotal minus max(1 GiB, 10%), 0 is not enforced")
	flag.IntVar(&cfg.MaxSandboxes, "max-sandboxes", getenvInt("ASP_MAX_SANDBOXES", 0), "maximum sandboxes on this node; 0 = no limit")
	flag.StringVar(&cfg.LocalNetDial, "local-net-dial", os.Getenv("ASP_LOCAL_NET_DIAL"), "host[:port] laptops dial for this node's local-net tunnels (default: the control plane's ASP_LOCAL_NET_DIAL)")
	flag.StringVar(&cfg.GuestSubnet, "guest-subnet", getenv("ASP_GUEST_SUBNET", "10.200.0.0/16"), "guest pool: each TAP gets its own /30 from it; also the nft --egress-nft-redirect match")
	flag.BoolVar(&cfg.GuestSSHAgentAuto, "guest-ssh-agent-auto", guestSSHAgentAutoDefault(), "expect guest image unit to expose host SSH agent at /run/agent-sandbox/ssh-agent.sock via vsock CID2:26501")
	recEvery := flag.Duration("reconcile-interval", 2*time.Second, "reconciler poll interval")
	hb := flag.Duration("heartbeat-interval", 30*time.Second, "control-plane heartbeat interval")
	flag.Parse()
	cfg.HeartbeatEvery = *hb
	cfg.ReconcileEvery = *recEvery

	// ADR-0007 phase 4: confirm default-on when multi-user / sock template / IdP required.
	// ASP_SSH_AGENT_CONFIRM=0 forces off; =1 forces on; unset → multi-user default.
	cfg.SSHAgentConfirm = sshAgentConfirmDefault(cfg)

	// Fase 2e: default guest SSH auto-mount when host side is enabled, unless
	// ASP_GUEST_SSH_AGENT_AUTO=0 was used to force-disable via guestSSHAgentAutoDefault.
	if !cfg.GuestSSHAgentAuto {
		disable := os.Getenv("ASP_GUEST_SSH_AGENT_AUTO")
		forcedOff := disable == "0" || disable == "false" || disable == "FALSE" || disable == "no" || disable == "NO"
		if !forcedOff && (cfg.HostVsock || cfg.SSHAgentBridge != "") {
			cfg.GuestSSHAgentAuto = true
		}
	}
	if cfg.NFTEgressMode == "" {
		cfg.NFTEgressMode = "soft"
	}
	if cfg.EnrollURL == "" {
		cfg.EnrollURL = cfg.ControlPlaneURL
	}
	switch cfg.ReapLeftovers {
	case reapOn, reapReport, reapOff:
	default:
		slog.Error("--reap-leftovers takes on, report or off", "value", cfg.ReapLeftovers)
		os.Exit(2)
	}
	if cfg.CapacityCPU < capacity.Detected || cfg.CapacityMemMiB < capacity.Detected || cfg.MaxSandboxes < 0 {
		slog.Error("--capacity-cpu and --capacity-mem-mib take -1 (detect), 0 (not enforced) or a count; --max-sandboxes takes 0 or more")
		os.Exit(2)
	}
	cfg.InstanceID = newInstanceID()
	host := capacity.Detect()
	cfg.CapacityCPU = capacity.CPU(cfg.CapacityCPU, host)
	cfg.CapacityMemMiB = capacity.MemMiB(cfg.CapacityMemMiB, host)

	if cfg.NodeID == "" {
		// An enrolled node keeps the identity of its certificate: the control plane
		// rejects requests for any other node id.
		cfg.NodeID = certNodeID(cfg.CertDir)
	}
	if cfg.NodeID == "" {
		hostname, err := os.Hostname()
		if err != nil {
			slog.Error("derive node-id", "error", err)
			os.Exit(2)
		}
		cfg.NodeID = hostname
	}
	if err := checkAgentListen(cfg.AgentListen, cfg.InsecureAgentListen); err != nil {
		slog.Error("refusing to start", "error", err)
		os.Exit(2)
	}
	if cfg.Endpoint == "" {
		if cfg.AgentTLSListen != "" {
			cfg.Endpoint = defaultTLSEndpoint(cfg.AgentTLSListen)
		} else {
			cfg.Endpoint = "http://" + cfg.AgentListen
		}
	}
	if cfg.CertDir == "" {
		cfg.CertDir = filepath.Join(os.TempDir(), "asp-node-certs")
	}
	// For lab/dry-run default cert dir under /tmp if default system path is not writable.
	if cfg.CertDir == "/var/lib/asp/node-certs" {
		if err := os.MkdirAll(cfg.CertDir, 0o755); err != nil {
			cfg.CertDir = filepath.Join(os.TempDir(), "asp-node-certs", cfg.NodeID)
		}
	}
	return cfg
}

// tapManager builds the reconciler's TAP manager. Only a dry-run agent shrugs
// off a failed TAP: a real VM must not boot without its network device.
func tapManager(cfg config) *tap.Manager {
	return &tap.Manager{Logger: slog.Default(), SoftFail: cfg.DryRun}
}

// registerRequest is what the node tells the control plane on every register.
// accepts_work mirrors --reconcile: an agent that does not claim work gets none.
func registerRequest(cfg config) cpclient.RegisterRequest {
	acceptsWork := cfg.Reconcile
	return cpclient.RegisterRequest{
		ID:              cfg.NodeID,
		Name:            cfg.NodeID,
		Endpoint:        cfg.Endpoint,
		AgentEndpoint:   agentEndpointURL(cfg),
		VMMProfiles:     []string{"cloud-hypervisor"},
		CapacityCPU:     cfg.CapacityCPU,
		CapacityMemMiB:  cfg.CapacityMemMiB,
		MaxSandboxes:    cfg.MaxSandboxes,
		AcceptsWork:     &acceptsWork,
		LocalNetDial:    cfg.LocalNetDial,
		AgentInstanceID: cfg.InstanceID,
		FenceEndpoint:   os.Getenv("ASP_FENCE_ENDPOINT"),
		FenceToken:      os.Getenv("ASP_FENCE_TOKEN"),
	}
}

// newInstanceID identifies this process to the control plane. VMs are not adopted
// across restarts, so a new id makes the control plane fail the running ones;
// cleanHost has already stopped them.
func newInstanceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("pid-%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func getenvInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Error("invalid integer", "env", key, "value", v)
		os.Exit(2)
	}
	return n
}

// certNodeID returns the CN of the enrolled node certificate in certDir, or "".
func certNodeID(certDir string) string {
	raw, err := os.ReadFile(filepath.Join(certDir, "client.crt"))
	if err != nil {
		return ""
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return ""
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(cert.Subject.CommonName)
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkAgentListen keeps the plain exec listener on loopback: it has no
// authentication, and its routes run commands in any sandbox on this node.
func checkAgentListen(addr string, insecure bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--agent-listen %q: %w", addr, err)
	}
	if isLoopbackHost(host) || insecure {
		return nil
	}
	return fmt.Errorf("--agent-listen %s is not a loopback address and the plain exec API has no authentication; use --agent-tls-listen for a control plane on another host, or --insecure-agent-listen (lab only)", addr)
}

// defaultTLSEndpoint advertises https://<host>:<port> for --agent-tls-listen. An
// unspecified host (0.0.0.0, ::, empty) becomes this machine's hostname; set
// --endpoint when the control plane reaches the node by another name.
func defaultTLSEndpoint(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "https://" + addr
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		if h, err := os.Hostname(); err == nil {
			host = h
		}
	}
	return "https://" + net.JoinHostPort(host, port)
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// sshAgentConfirmDefault: ASP_SSH_AGENT_CONFIRM=1 on, =0 off;
// unset → on when multi-user (ASP_MULTI_USER / ASP_IDP_REQUIRED / sock template).
func sshAgentConfirmDefault(cfg config) bool {
	v := os.Getenv("ASP_SSH_AGENT_CONFIRM")
	switch v {
	case "1", "true", "TRUE", "yes", "YES":
		return true
	case "0", "false", "FALSE", "no", "NO":
		return false
	}
	// Flag explicitly passed as true via --ssh-agent-confirm without env.
	if cfg.SSHAgentConfirm {
		return true
	}
	if cfg.MultiUser || cfg.SSHAgentSockTemplate != "" {
		return true
	}
	return false
}

// guestSSHAgentAutoDefault: ASP_GUEST_SSH_AGENT_AUTO=0 disables; =1 enables;
// unset → enabled when ASP_HOST_VSOCK=1 or ASP_SSH_AGENT_BRIDGE is set (Fase 2e).
func guestSSHAgentAutoDefault() bool {
	v := os.Getenv("ASP_GUEST_SSH_AGENT_AUTO")
	switch v {
	case "0", "false", "FALSE", "no", "NO":
		return false
	case "1", "true", "TRUE", "yes", "YES":
		return true
	}
	if os.Getenv("ASP_HOST_VSOCK") == "1" {
		return true
	}
	if os.Getenv("ASP_SSH_AGENT_BRIDGE") != "" {
		return true
	}
	return false
}
