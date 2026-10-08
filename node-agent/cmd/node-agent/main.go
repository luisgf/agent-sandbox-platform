package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/attest"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/capacity"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/certrenew"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/cpclient"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/doctor"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/egress"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/egress/mitm"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/execproxy"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/hostvsock"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/identity"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/localnet"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/measure"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/metrics"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/nftredirect"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/poddaemon"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/reconciler"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/settings"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/sshagent"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/tap"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/unit"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/version"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/virtiofs"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/workspace"
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
	EnrollToken          string // --enroll-token: single-use, admin-issued (asp node enroll-token)
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
	ReconcileWorkers     int // --reconcile-workers: sandboxes started/stopped at once
	GuestReadyTimeout    time.Duration
	TapAuto              bool
	HostVsock            bool
	HostVsockDir         string // unix factory fallback when AF_VSOCK unavailable / lab
	EgressProxyListen    string
	EgressDNSSink        string
	EgressMITMCA         string
	EgressAllowCIDRs     string // --egress-allow-cidr: private destinations the proxy may reach
	AgentTokenFile       string // --agent-token-file: secret of the local API, shared with a same-host control plane
	APIKeyFile           string // --api-key-file: the node's API key for a control plane reached over plain HTTP
	EgressMITM           bool
	SSHAgentConfirm      bool
	sshAgentConfirm      optBool // --ssh-agent-confirm as given; unset: on in the multi-user profile
	SSHGlobalApprovals   bool    // --insecure-ssh-agent-global-approvals: unscoped approvals for listeners without a sandbox (lab)
	SSHAgentSockTemplate string  // ASP_SSH_AGENT_SOCK_TEMPLATE
	MultiUser            bool    // ASP_MULTI_USER=1 → confirm default-on + scoped SSH
	EgressNFTRedirect    bool    // resolved by resolveEgress
	egressNFTRedirect    optBool // --egress-nft-redirect as given (unset: on with an egress proxy)
	EgressEnforced       bool    // reported on register: proxy listening and nft rules applied in enforce mode
	MetricsListen        string  // --metrics-listen: GET /metrics, Prometheus text format
	PprofListen          string  // --pprof-listen: runtime profiles under /debug/pprof/
	InsecureObsListen    bool    // --insecure-obs-listen: let those two listen off the loopback
	NFTEgressMode        string  // soft | enforce ("": enforce, soft with --dry-run)
	NFTDNSAction         string  // redirect | drop
	NFTHTTPPorts         string
	GuestSubnet          string
	CapacityCPU          int    // --capacity-cpu: cores offered; -1 detect, 0 not enforced
	CapacityMemMiB       int    // --capacity-mem-mib: MiB offered; -1 detect, 0 not enforced
	MaxSandboxes         int    // --max-sandboxes: 0 = no limit
	LocalNetDial         string // --local-net-dial: host[:port] laptops dial for local-net
	InstanceID           string // random per process; sent on every register
	VirtiofsdBin         string
	VirtiofsdSandbox     string // --virtiofsd-sandbox: none | chroot | namespace; "" = chroot as root, none otherwise
	WorkspaceRoots       string // --workspace-root: where sandboxes' workspaces may live
	DiskDir              string
	LocalNetKeyDir       string        // --local-net-key-dir: the per-sandbox WireGuard node keys ("" = /var/lib/asp/local-net)
	StopGrace            time.Duration // --stop-grace
	DiskMinFreeMiB       int           // --disk-min-free-mib: -1 twice the base image, 0 not checked
	ReapLeftovers        string        // --reap-leftovers: on | report | off
	ReapOnly             bool          // --reap-only: clean up and exit, without registering
	ShowVersion          bool          // --version: print which build this is and exit
	ConfigFile           string        // --config: the settings file (default /etc/asp/agent.yaml, read when it exists)
	PrintConfig          bool          // --print-config: print the effective configuration and where each value came from, and exit
	PrintMeasurement     bool          // --print-measurement: print what this node would attest and exit
	Doctor               bool          // --doctor: check this host and this configuration, print the report and exit
	DoctorJSON           bool          // --doctor-json: the report as JSON
	GuestKernel          string        // --guest-kernel: the kernel every VM boots
	GuestRootFS          string        // --guest-rootfs: the base image every sandbox disk is copied from
	GuestVerify          string        // --guest-verify: auto | on | off
	VMConfine            string        // --vm-confine: auto | on | off
	VMSurviveRestart     bool          // --vm-survive-restart: confined VMs keep running when the agent restarts
	VMUnprivileged       string        // --vm-unprivileged: auto | on | off
	VMUIDBase            uint          // --vm-uid-base: the first user id of the VMs
	VMRunDir             string        // --vm-run-dir: parent of the per-VM directories
	AdoptedSandboxes     []string      // the VMs of a previous agent process this one took over; sent on register
	VMSlice              string        // --vm-slice
	VMMemoryOverheadMiB  int           // --vm-memory-overhead-mib
	VMCPUOverheadPercent int           // --vm-cpu-overhead-percent
	VMTasksMax           int           // --vm-tasks-max
}

func main() {
	cfg := loadConfig()
	if cfg.ShowVersion {
		fmt.Printf("node-agent %s\n", version.Get())
		return
	}
	if cfg.PrintMeasurement {
		os.Exit(printMeasurement(cfg, cfg.GuestKernel, cfg.GuestRootFS, os.Stdout, os.Stderr))
	}
	if cfg.Doctor {
		// Before anything is refused for a missing key or a socket directory: the doctor
		// is for the node that does not start.
		os.Exit(runDoctor(cfg, cfg.DoctorJSON))
	}
	if err := checkNodeKeyLocations(cfg); err != nil {
		slog.Error("refusing to start", "error", err)
		os.Exit(2)
	}
	if cfg.ReapOnly {
		os.Exit(reapOnly(cfg))
	}
	slog.Info("node-agent starting",
		"version", version.Short(),
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
		if cfg.EnrollToken == "" && cfg.BootstrapToken == "" {
			slog.Error("--enroll requires --enroll-token (ASP_NODE_ENROLL_TOKEN) or --bootstrap-token (ASP_NODE_BOOTSTRAP_TOKEN)")
			os.Exit(2)
		}
		nodeID, err := enrollNode(ctx, cfg, cpclient.New(cfg.EnrollURL, plain), time.Now())
		if err != nil {
			slog.Error("enrollment failed", "error", err)
			os.Exit(1)
		}
		cfg.NodeID = nodeID
	}

	httpClient, nodeCert, err := cpclient.LoadMTLSClientCert(cfg.CertDir, cfg.MTLS, cfg.ControlPlaneCA)
	if err != nil {
		slog.Error("load mTLS client", "error", err)
		os.Exit(1)
	}
	if nodeCert != nil {
		slog.Info("using mTLS client certs", "cert_dir", cfg.CertDir, "cert_not_after", nodeCert.Leaf().NotAfter)
		if w := controlPlaneTrustWarning(cfg, fileExists(filepath.Join(cfg.CertDir, "ca.crt"))); w != "" {
			slog.Warn(w)
		}
	}
	cp := cpclient.New(cfg.ControlPlaneURL, httpClient)
	nodeAPIKey, err := loadNodeAPIKey(cfg)
	if err != nil {
		slog.Error("node api key", "error", err)
		os.Exit(2)
	}
	cp.APIKey = nodeAPIKey
	if nodeCert != nil && isHTTPS(cfg.ControlPlaneURL) {
		// Renew a third of the lifetime ahead, authenticated by the current
		// certificate; new connections present the renewed one.
		renewer := &certrenew.Renewer{
			CP: cp, Cert: nodeCert, NodeID: cfg.NodeID, Logger: slog.Default(),
			OnRotate: httpClient.CloseIdleConnections,
		}
		go renewer.Run(ctx)
	}

	var engine vmm.VMM
	var vmConfine *vmm.Confinement
	if cfg.DryRun {
		engine = vmm.NewFakeVMM(slog.Default())
		slog.Info("using FakeVMM (dry-run)")
	} else if cfg.CHAPISocket != "" {
		// Legacy shared/debug: talk to a pre-started CH on one socket (no spawn).
		engine = vmm.NewCloudHypervisor(cfg.VMMBinary, cfg.CHAPISocket)
		slog.Info("using Cloud Hypervisor shared API socket", "socket", cfg.CHAPISocket, "binary", cfg.VMMBinary)
	} else {
		// Default: spawn one cloud-hypervisor per sandbox under --ch-socket-dir.
		ch := vmm.NewSpawningCloudHypervisor(cfg.VMMBinary, cfg.CHSocketDir)
		vmConfine, err = vmConfinement(cfg, unit.Available())
		if err != nil {
			slog.Error("--vm-confine", "error", err)
			os.Exit(2)
		}
		if vmConfine != nil {
			vmConfine.Unprivileged, err = vmUnprivileged(cfg, vmConfine, hostVMUserProbes())
			if err != nil {
				slog.Error("--vm-unprivileged", "error", err)
				os.Exit(2)
			}
		}
		ch.Confine = vmConfine
		engine = ch
		slog.Info("using Cloud Hypervisor per-sandbox spawn", "socket_dir", cfg.CHSocketDir, "binary", cfg.VMMBinary,
			"confined", vmConfine != nil, "unprivileged", vmConfine != nil && vmConfine.Unprivileged != nil)
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

	// Metrics: the registry every part below counts into. Served by --metrics-listen,
	// loopback unless --insecure-obs-listen (the endpoint has no authentication).
	obs := metrics.NewRegistry()
	obs.AddCollector(metrics.RuntimeCollector(time.Now()))
	egressMetrics := egress.NewMetrics(obs)
	obs.AddCollector(diskCollector(cfg.DiskDir))
	if cfg.MetricsListen != "" {
		if err := metrics.Serve(ctx, "metrics", cfg.MetricsListen, cfg.InsecureObsListen, obs.Mux()); err != nil {
			slog.Error("metrics listen", "error", err)
			os.Exit(2)
		}
	}
	if cfg.PprofListen != "" {
		if err := metrics.Serve(ctx, "pprof", cfg.PprofListen, cfg.InsecureObsListen, metrics.PprofHandler()); err != nil {
			slog.Error("pprof listen", "error", err)
			os.Exit(2)
		}
	}

	defaultAL := egress.NewAllowlistFromPolicy("deny-default", nil)
	policyCache := &egress.PolicyCache{}
	proxy := &execproxy.Server{
		Pod:              podClient,
		Registry:         pdRegistry,
		Logger:           slog.Default(),
		EgressEnforce:    cfg.EgressEnforce,
		DefaultAllowlist: defaultAL,
		PolicyCache:      policyCache,
		// The self-checks of the node, for `asp node doctor` through the control plane.
		Doctor: func(ctx context.Context) any {
			return doctor.Run(ctx, cfg.NodeID, doctorChecks(cfg, cp), 20*time.Second)
		},
	}
	agentToken, err := agentTokenFor(cfg)
	if err != nil {
		slog.Error("agent token", "error", err)
		os.Exit(1)
	}
	httpSrv, ln, err := execproxy.ListenAndServe(cfg.AgentListen, execproxy.RequireToken(agentToken, proxy.Handler()))
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
		var tlsCfg *tls.Config
		var err error
		if nodeCert != nil {
			// Read at each handshake: a renewed certificate is served at once.
			tlsCfg, err = execproxy.MTLSConfigFor(nodeCert.Current, filepath.Join(cfg.CertDir, "ca.crt"))
		} else {
			tlsCfg, err = execproxy.MTLSConfig(
				filepath.Join(cfg.CertDir, "client.crt"),
				filepath.Join(cfg.CertDir, "client.key"),
				filepath.Join(cfg.CertDir, "ca.crt"),
			)
		}
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
		guard, err := egressGuard(cfg)
		if err != nil {
			slog.Error("egress destination guard", "error", err)
			os.Exit(2)
		}
		fp := &egress.ForwardProxy{
			Default:   defaultAL,
			Cache:     policyCache,
			Logger:    slog.Default(),
			Enforce:   true, // proxy always deny-by-default when listening
			RateLimit: egress.NewTokenBucket(egress.DefaultRate, egress.DefaultBurst),
			Guard:     guard,
			Metrics:   egressMetrics,
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
			Metrics:   egressMetrics,
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
		if p := listenPort(cfg.EgressProxyListen); p > 0 {
			proxyPort = p
		}
		dnsSinkPort := 5353
		if p := listenPort(cfg.EgressDNSSink); p > 0 {
			dnsSinkPort = p
		}
		mode := nftredirect.ModeSoft
		if cfg.NFTEgressMode == nftModeEnforce {
			mode = nftredirect.ModeEnforce
		}
		applied, err := nftredirect.ApplyChecked(nftredirect.Config{
			GuestSubnet: cfg.GuestSubnet,
			ProxyPort:   proxyPort,
			DNSSinkPort: dnsSinkPort,
			HTTPPorts:   cfg.NFTHTTPPorts,
			DNSAction:   cfg.NFTDNSAction,
			Mode:        mode,
			Logger:      slog.Default(),
		})
		if err != nil {
			slog.Error("egress-nft-redirect", "error", err, "mode", mode)
			os.Exit(1)
		}
		cfg.EgressEnforced = egressEnforced(cfg, applied)
	}
	obs.AddCollector(buildInfoCollector())
	obs.AddCollector(func() []metrics.Sample {
		v := 0.0
		if cfg.EgressEnforced {
			v = 1
		}
		return []metrics.Sample{{Name: "asp_agent_egress_enforced", Type: "gauge",
			Help: "1 when this node forces its guests through its egress proxy (rules applied in enforce mode).", Value: v}}
	})
	if !cfg.DryRun && cfg.Reconcile && !cfg.EgressEnforced {
		slog.Warn("this node does not enforce egress: its guests are not forced through the egress proxy", "egress_proxy_listen", cfg.EgressProxyListen, "nft_redirect", cfg.EgressNFTRedirect, "nft_mode", cfg.NFTEgressMode)
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
		APIKey:             nodeAPIKey,
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

	// guestDigests is set once the node measures its guest files (not in dry-run).
	var guestDigests func() (kernel, image string)
	register := func(ctx context.Context) error {
		req := registerRequest(cfg)
		if guestDigests != nil {
			req.GuestKernelDigest, req.GuestImageDigest = guestDigests()
		}
		return cp.Register(ctx, req)
	}
	heartbeatInfo := func() cpclient.HeartbeatInfo {
		var info cpclient.HeartbeatInfo
		if !cfg.DryRun {
			if mib, ok := reconciler.DiskFreeMiB(cfg.DiskDir); ok {
				info.DiskFreeMiB = &mib
			}
		}
		return info
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
		rec.Metrics = reconciler.NewMetrics(obs)
		obs.AddCollector(rec.Collector())
		rec.KernelPath, rec.RootFSPath = cfg.GuestKernel, cfg.GuestRootFS
		rec.Workers = cfg.ReconcileWorkers
		if !cfg.DryRun {
			rec.GuestReadyTimeout = cfg.GuestReadyTimeout
		}
		if cfg.CHAPISocket != "" {
			// One shared Cloud Hypervisor API socket runs one VM at a time.
			rec.Workers = 1
		}
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
			rec.GuestDNS = guestDNS(cfg)
			rec.GuestProxyPort = guestProxyPort(cfg)
		}
		rec.Egress = policyCache
		rec.SSHAgentShared = cfg.SSHAgentBridge
		rec.SSHRegistry = sshRegistry
		rec.Attest = attestSigners(cfg, nodeCert)
		if !cfg.DryRun {
			measurer := measure.NewCache()
			rec.Measure = measurer.SHA256
			rec.Expected, rec.GuestVerify = measure.Expected, cfg.GuestVerify
			rec.VMMVersion = vmmVersion(ctx, cfg.VMMBinary)
			guestDigests = func() (string, string) { return measurer.Peek(rec.KernelPath), measurer.Peek(rec.RootFSPath) }
			// Hash the kernel and the base image now, in the background, so the
			// first boot does not wait for it. The control plane learns them from the
			// next register, which this makes once they are known.
			go func() {
				ok := true
				for _, path := range []string{rec.KernelPath, rec.RootFSPath} {
					if _, err := measurer.SHA256(path); err != nil {
						slog.Warn("boot attestation: file not measured", "path", path, "error", err)
						ok = false
					}
				}
				if ok {
					if err := register(ctx); err != nil {
						slog.Debug("register with the guest digests failed; the next register will carry them", "error", err)
					}
				}
			}()
		}
		rec.VirtiofsdBin = cfg.VirtiofsdBin
		roots, err := workspace.ParseRoots(cfg.WorkspaceRoots)
		if err != nil {
			slog.Error("--workspace-root", "error", err)
			os.Exit(2)
		}
		rec.WorkspaceRoots = roots
		rec.Confine = vmConfine
		rec.VirtiofsdSandbox, err = virtiofsSandbox(cfg.VirtiofsdSandbox, os.Geteuid())
		if err != nil {
			slog.Error("--virtiofsd-sandbox", "error", err)
			os.Exit(2)
		}
		if !cfg.DryRun {
			// Never boot the shared image writable: every VM gets its own copy.
			rec.DiskDir = cfg.DiskDir
			rec.StopGrace = cfg.StopGrace
			rec.MinFreeDiskMiB = diskMinFreeMiB(cfg.DiskMinFreeMiB, rec.RootFSPath)
		}
		if hvSvc != nil {
			rec.GuestHost = hvSvc
			slog.Info("reconciler will attach CH hybrid guest→host acceptors per sandbox",
				"ssh_scoped", cfg.SSHAgentSockTemplate != "" || cfg.MultiUser)
		}
		rec.StateDir = vmStateDir(cfg, vmConfine)
		// The VMs a previous agent process left running are taken over before the
		// node registers, so the register body can say which: the control plane does
		// not fail those as orphans of the restart.
		cfg.AdoptedSandboxes = rec.Adopt(ctx)
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

	if err := register(ctx); err != nil {
		slog.Error("control-plane registration failed", "error", err)
		os.Exit(1)
	}
	slog.Info("registered with control plane", "node_id", cfg.NodeID, "agent_endpoint", agentEndpointURL(cfg),
		"capacity_cpu", cfg.CapacityCPU, "capacity_mem_mib", cfg.CapacityMemMiB, "max_sandboxes", cfg.MaxSandboxes,
		"accepts_work", cfg.Reconcile, "local_net_dial", cfg.LocalNetDial, "adopted_vms", len(cfg.AdoptedSandboxes))
	if err := cp.Heartbeat(ctx, cfg.NodeID, heartbeatInfo()); err != nil {
		slog.Warn("initial heartbeat failed", "error", err)
	}
	if rec != nil {
		go rec.Run(ctx)
	}

	ticker := time.NewTicker(cfg.HeartbeatEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("node-agent shutting down")
			return
		case <-ticker.C:
			if err := cp.Heartbeat(ctx, cfg.NodeID, heartbeatInfo()); err != nil {
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

// settingsOf declares every flag of the node-agent, with the environment variable that
// sets it (see the settings package), on s.
func declareSettings(s *settings.Set, cfg *config) {
	s.String(&cfg.ControlPlaneURL, "control-plane-url", "http://127.0.0.1:8080", "control plane base URL", settings.Legacy("CONTROL_PLANE_URL"))
	s.String(&cfg.ControlPlaneCA, "control-plane-ca", "", "PEM CA that signed the control plane's TLS certificate (enroll and API calls); default: cert-dir/ca.crt, then system roots. cert-dir/ca.crt is the CA that signs every node certificate, so with a remote control plane the agent warns: pass a CA that signs only the control plane's certificate")
	s.String(&cfg.EnrollURL, "enroll-url", "", "control-plane URL for --enroll when it differs from --control-plane-url (ASP_MTLS_STRICT serves enroll on a separate listener)")
	s.String(&cfg.NodeID, "node-id", "", "node identifier", settings.Legacy("NODE_ID"))
	s.String(&cfg.CHAPISocket, "ch-api-socket", "", "optional shared CH --api-socket (legacy/debug); empty = per-sandbox spawn via --ch-socket-dir", settings.Legacy("CH_API_SOCKET"))
	s.String(&cfg.CHSocketDir, "ch-socket-dir", "/run/asp", "directory for per-sandbox CH API sockets (ch-{sandboxID}.sock). The agent creates it 0700 and tightens an existing one that is not a system directory (/run, /tmp…): the sockets inside give authority over every sandbox (root exec in the guest, identity tokens, virtiofsd)", settings.Legacy("CH_SOCKET_DIR"))
	s.String(&cfg.VMMBinary, "ch-binary", "cloud-hypervisor", "cloud-hypervisor binary path (spawned per sandbox when not using --ch-api-socket)", settings.Legacy("CLOUD_HYPERVISOR_BIN"))
	s.String(&cfg.WorkspaceRoots, "workspace-root", workspace.DefaultRoot, "comma-separated directories a sandbox's workspace may live under: a workspace must be inside <root>/<tenant>/ (symbolic links resolved). The workspace path comes from the sandbox spec, so without this any caller could export the node's disks and keys; with no root that exists, no sandbox can have a workspace", settings.Env("ASP_WORKSPACE_ROOTS"))
	s.String(&cfg.GuestKernel, "guest-kernel", reconciler.DefaultKernelPath, "kernel (an uncompressed vmlinux) every VM boots. A test with another guest needs no change to /opt/sandbox; the attestation (--print-measurement) measures this file and --guest-rootfs")
	s.String(&cfg.GuestVerify, "guest-verify", reconciler.GuestVerifyAuto, "check the guest kernel and base image against a SHA256SUMS next to them (asp image pull installs one): auto (refuse to boot from a file the sums list with another digest), on (also refuse one no sums list) or off. The sandbox fails with the reason; a digest is cached while the file does not change")
	s.String(&cfg.GuestRootFS, "guest-rootfs", reconciler.DefaultRootFSPath, "base rootfs image every sandbox's private disk is copied from; never booted itself")
	s.String(&cfg.VMConfine, "vm-confine", "auto", "run each microVM and its virtiofsd in a transient systemd service (asp-vm-<id>, asp-vm-<id>-fs) with its own cgroup and resource limits: auto (when this host can: root, systemd; otherwise as children of this process, and it says why), on (refuse to start if it cannot) or off (children of this process, as before)")
	s.Bool(&cfg.VMSurviveRestart, "vm-survive-restart", true, "a confined microVM keeps running when the agent stops or restarts, and the next agent process takes it over (default; each running VM leaves a record in <ch-socket-dir>/state/). =false binds each VM's service to the agent's, so systemd stops the VMs with it, as before")
	s.String(&cfg.VMUnprivileged, "vm-unprivileged", "auto", "run each microVM's Cloud Hypervisor as an unprivileged user of its own, with no capabilities and a service that cannot open IP sockets or write outside its own files: auto (when this host can, which it checks by opening, as that user, what the VMM needs; otherwise the VMM stays root and it says why), on (refuse to start if it cannot) or off (root, as before). Needs --vm-confine; virtiofsd stays root (--virtiofsd-sandbox)")
	s.Uint(&cfg.VMUIDBase, "vm-uid-base", uint(vmm.DefaultUIDBase), "first user id of the microVMs: the one with guest CID n runs as this plus n. Pick a range that no user, container tool or other agent uses")
	s.String(&cfg.VMRunDir, "vm-run-dir", "", "directory with one subdirectory per microVM, owned by the VM's user, where its VMM keeps its sockets; mode 0711. Default: --ch-socket-dir with -vm appended (/run/asp-vm)")
	s.String(&cfg.VMSlice, "vm-slice", vmm.DefaultSlice, "systemd slice of the microVM services")
	s.Int(&cfg.VMMemoryOverheadMiB, "vm-memory-overhead-mib", vmm.DefaultMemoryOverheadMiB, "memory added to the guest's for the VMM's own use, in the unit's MemoryMax")
	s.Int(&cfg.VMCPUOverheadPercent, "vm-cpu-overhead-percent", vmm.DefaultCPUOverheadPercent, "percent of one CPU added to the guest's vCPUs for the VMM's own threads, in the unit's CPUQuota")
	s.Int(&cfg.VMTasksMax, "vm-tasks-max", vmm.DefaultTasksMax, "most processes and threads of one microVM service")
	s.String(&cfg.VirtiofsdSandbox, "virtiofsd-sandbox", "", "virtiofsd --sandbox mode: chroot (confine the daemon to the workspace), namespace or none. Default chroot when running as root, none otherwise")
	s.String(&cfg.VirtiofsdBin, "virtiofsd-bin", "virtiofsd", "Rust virtiofsd binary; started per sandbox only when workspace_host_path is set", settings.Legacy("VIRTIOFSD_BIN"))
	s.String(&cfg.LocalNetKeyDir, "local-net-key-dir", "", "where the per-sandbox WireGuard node keys of on-demand local-net are kept (default /var/lib/asp/local-net; under the temp dir with --dry-run)")
	s.String(&cfg.DiskDir, "disk-dir", "/var/lib/asp/disks", "per-sandbox rootfs copies (rootfs-{id}.img). A stop keeps the copy when the control plane keeps stopped sandboxes (ADR-0012); a delete, or a control plane that does not, removes it. Copies no sandbox owns are removed after a poll. Ignored with --dry-run")
	s.Duration(&cfg.StopGrace, "stop-grace", 15*time.Second, "a stop asks the guest to power off (sync; systemctl poweroff, through pod-daemon) and waits up to this long for the VM to exit before stopping it hard (0: stop hard at once; ignored with --dry-run)")
	s.Int(&cfg.DiskMinFreeMiB, "disk-min-free-mib", -1, "refuse to clone or resume a sandbox disk when --disk-dir has less free space (MiB). -1: twice the base image's size; 0: do not check")
	s.Bool(&cfg.DryRun, "dry-run", false, "use FakeVMM and skip real CH", settings.Legacy("DRY_RUN"))
	s.String(&cfg.ReapLeftovers, "reap-leftovers", reapOn, "at start, remove what a previous node-agent left on this host: cloud-hypervisor and virtiofsd processes of --ch-socket-dir, its per-sandbox sockets, asp-* TAPs, wg-asp-* tunnels and their routing (not the rootfs copies in --disk-dir: the reconciler removes the ones no sandbox owns). on | report (log, remove nothing) | off; --dry-run only reports")
	s.Bool(&cfg.Doctor, "doctor", false, "check this host and this configuration (KVM, hypervisor, virtiofsd, guest images, disk, nftables, TAPs, clock, the control plane) and print what is wrong and how to fix it; exit 1 when a check failed", settings.NoEnv())
	s.Bool(&cfg.DoctorJSON, "doctor-json", false, "with --doctor, print the report as JSON", settings.NoEnv())
	s.Bool(&cfg.ShowVersion, "version", false, "print which build this is and exit", settings.NoEnv())
	s.String(&cfg.ConfigFile, "config", "", "settings file (YAML, keys named like the flags); its drop-ins are read from <file>.d/*.yaml. Default: "+defaultConfigFile+" when it exists. Flags and environment variables win over it")
	s.Bool(&cfg.PrintConfig, "print-config", false, "print the effective configuration, with where each value came from (flag, environment, file, default), and exit", settings.NoEnv())
	s.Bool(&cfg.PrintMeasurement, "print-measurement", false, "print the digests of the kernel and base image and the hypervisor version this node attests, as an entry for the control plane's ASP_ATTEST_ALLOWED_IMAGES, and exit", settings.NoEnv())
	s.Bool(&cfg.ReapOnly, "reap-only", false, "remove those leftovers and exit without registering (systemd ExecStopPost); refused while a node-agent runs with this --ch-socket-dir", settings.NoEnv())
	s.String(&cfg.Endpoint, "endpoint", "", "node callback endpoint advertised to control plane", settings.Legacy("NODE_ENDPOINT"))
	s.String(&cfg.AgentListen, "agent-listen", "127.0.0.1:9100", "listen addr of the local API: exec in any sandbox, the egress policy check and the SSH-agent approvals. Plain HTTP; every route but GET /healthz needs the bearer token of --agent-token-file. Outside loopback the agent does not start unless --insecure-agent-listen")
	s.String(&cfg.AgentTLSListen, "agent-tls-listen", "", "listen addr for the control plane's mTLS exec API (e.g. 0.0.0.0:9443) when the control plane runs on another host; uses the enrolled node certificate")
	s.Bool(&cfg.InsecureAgentListen, "insecure-agent-listen", false, "allow --agent-listen on a non-loopback address (plain HTTP: the bearer token crosses the network in the clear; lab only)")
	s.Bool(&cfg.Enroll, "enroll", false, "perform bootstrap enrollment before register")
	s.String(&cfg.BootstrapToken, "bootstrap-token", "", "shared bootstrap token for enrollment: enrolls a new node id or a revoked node, never re-keys an enrolled one", settings.Env("ASP_NODE_BOOTSTRAP_TOKEN"), settings.Secret())
	s.String(&cfg.EnrollToken, "enroll-token", "", "single-use enroll token from an admin (asp node enroll-token), used instead of --bootstrap-token; one pinned to this node re-keys it even when it is enrolled", settings.Env("ASP_NODE_ENROLL_TOKEN"), settings.Secret())
	s.String(&cfg.CertDir, "cert-dir", "/var/lib/asp/node-certs", "directory for node client certs")
	s.Bool(&cfg.MTLS, "mtls", false, "require mTLS client certs for control-plane calls")
	s.String(&cfg.PodDaemonSock, "pod-daemon-sock", "", "unix socket path for pod-daemon (dry-run / local fallback)")
	s.Uint(&cfg.PodDaemonPort, "pod-daemon-port", 26500, "guest vsock/TCP port for pod-daemon HTTP (CH hybrid CONNECT)")
	s.Bool(&cfg.EgressEnforce, "egress-enforce", false, "return 403 on /v1/internal/egress-check denials; the egress proxy denies either way, and without this flag the agent only warns when --egress-proxy-listen is set")
	s.String(&cfg.EgressProxyListen, "egress-proxy-listen", "", "optional HTTP forward proxy listen (e.g. :8888); guests set HTTP_PROXY to host TAP IP:port")
	s.String(&cfg.EgressDNSSink, "egress-dns-sink", "", "optional UDP DNS sink (e.g. :5353) that NXDOMAIN non-allowlisted names. With the nft redirect and --nft-dns-action=redirect it starts by itself on :5353 when not named")
	s.String(&cfg.EgressAllowCIDRs, "egress-allow-cidr", "", "comma-separated private networks (CIDR or address) the egress proxy may connect to, on top of the public internet. The proxy checks the address after resolving the name. Loopback, link-local, multicast, this node's own addresses and the guests' network are never reachable, whatever a tenant allows", settings.Env("ASP_EGRESS_ALLOW_CIDRS"))
	s.String(&cfg.APIKeyFile, "api-key-file", "", "file with the platform-scoped API key this node sends to the control plane (also env ASP_NODE_API_KEY). Needed when the control plane is reached over plain HTTP, where there is no client certificate; the control plane refuses every node call without a credential", settings.Env("ASP_NODE_API_KEY_FILE"))
	s.String(&cfg.AgentTokenFile, "agent-token-file", "", "file holding the secret that guards the local API on --agent-listen (bearer token). Created, 0600, if missing. A control plane on this host reads the same file (ASP_AGENT_TOKEN_FILE) and must be able to read it (if it does not run as root, create the file first, mode 0640 with its group). Default "+execproxy.DefaultTokenFile+", or a temporary file when that directory is not writable (dry-run labs)")
	s.String(&cfg.EgressMITMCA, "egress-mitm-ca", "", "optional path to MITM CA PEM (generate/load); used only with --egress-mitm / ASP_EGRESS_MITM=1")
	s.Bool(&cfg.EgressMITM, "egress-mitm", false, "ENABLE CONNECT TLS bump (corp caution; default off)")
	s.String(&cfg.SSHAgentBridge, "ssh-agent-bridge", "", "unix socket path for SSH agent bridge (proxies SSH_AUTH_SOCK or FakeAgent)")
	s.String(&cfg.IdentityListen, "identity-listen", "", "unix path (.sock) or TCP addr for guest OIDC identity proxy")
	s.String(&cfg.DefaultSandboxID, "default-sandbox-id", "", "sandbox for identity requests without X-ASP-Sandbox-ID on listeners not bound to a sandbox; only with --insecure-identity-sandbox-header (dry-run)", settings.Env("ASP_SANDBOX_ID"))
	s.Bool(&cfg.TrustSandboxHeader, "insecure-identity-sandbox-header", false, "let identity listeners not bound to a sandbox (--identity-listen, global --host-vsock) take the sandbox from the client's X-ASP-Sandbox-ID, then --default-sandbox-id; anyone who reaches them can mint any sandbox's token (lab only)")
	s.Bool(&cfg.Reconcile, "reconcile", false, "poll control-plane work and drive VMM lifecycle")
	s.Bool(&cfg.TapAuto, "tap-auto", false, "create and delete an asp-{shortid} TAP, with its own /30 of --guest-subnet, around VMM Start/Stop, and give the guest its hostname, resolver and HTTP_PROXY on its kernel command line. A TAP that cannot be created fails the start (only --dry-run goes without TAPs)")
	s.Bool(&cfg.HostVsock, "host-vsock", false, "guest→host SSH(26501)+identity(26502): AF_VSOCK/unix lab + per-sandbox CH hybrid {vsock}_{port}")
	s.String(&cfg.HostVsockDir, "host-vsock-dir", "", "if set, use unix sockets under this dir instead of AF_VSOCK (lab)")
	s.Tri(&cfg.sshAgentConfirm, "ssh-agent-confirm", "require POST /v1/internal/ssh-agent/approve with the signing sandbox's sandbox_id before each SignRequest (one-shot TTL); default on in multi-user")
	s.Bool(&cfg.SSHGlobalApprovals, "insecure-ssh-agent-global-approvals", false, "with --ssh-agent-confirm, accept approvals without sandbox_id; they unlock one sign on listeners that cannot tell guests apart (--ssh-agent-bridge, global --host-vsock), from whichever guest asks first (lab only)")
	s.String(&cfg.SSHAgentSockTemplate, "ssh-agent-sock-template", "", "per-sandbox SSH agent upstream path template ({owner_sub}/{sandbox_id}/{id}); missing → FakeAgent")
	s.Bool(&cfg.MultiUser, "multi-user", false, "multi-user profile: SSH confirm default-on + prefer scoped agent socks", settings.Legacy("ASP_IDP_REQUIRED"))
	s.Tri(&cfg.egressNFTRedirect, "egress-nft-redirect", "force guest HTTP(S)+DNS through the egress proxy and sink with nftables and drop the rest. Default: on when --egress-proxy-listen is set and this is not --dry-run; --egress-nft-redirect=false turns it off (HTTP_PROXY is then voluntary)", settings.LegacyFlag("nft-egress-redirect"), settings.Legacy("ASP_NFT_EGRESS_REDIRECT"))
	s.String(&cfg.NFTEgressMode, "nft-egress-mode", "", "what a node that cannot apply the nft rules does: enforce (refuse to start; the default) or soft (start without egress enforcement; the default with --dry-run). The node reports egress_enforced=true when it registers only with the proxy listening and the rules applied in enforce mode")
	s.String(&cfg.MetricsListen, "metrics-listen", "", "serve Prometheus metrics on this address (GET /metrics), e.g. 127.0.0.1:9102. No authentication, so loopback only unless --insecure-obs-listen. Off by default")
	s.String(&cfg.PprofListen, "pprof-listen", "", "serve Go runtime profiles on this address (/debug/pprof/), e.g. 127.0.0.1:6060. No authentication, so loopback only unless --insecure-obs-listen. Off by default")
	s.Bool(&cfg.InsecureObsListen, "insecure-obs-listen", false, "allow --metrics-listen and --pprof-listen on a non-loopback address (no authentication; put a proxy that authenticates in front)")
	s.String(&cfg.NFTDNSAction, "nft-dns-action", "redirect", "guest DNS handling: redirect (to --egress-dns-sink port) | drop")
	s.String(&cfg.NFTHTTPPorts, "nft-http-ports", "80,443", "comma-separated guest TCP ports redirected to egress proxy")
	s.Int(&cfg.CapacityCPU, "capacity-cpu", capacity.Detected, "CPU cores offered to sandboxes: -1 detects, 0 is not enforced (the control plane overcommits CPU)")
	s.Int(&cfg.CapacityMemMiB, "capacity-mem-mib", capacity.Detected, "memory offered to sandboxes in MiB: -1 detects MemTotal minus max(1 GiB, 10%), 0 is not enforced")
	s.Int(&cfg.MaxSandboxes, "max-sandboxes", 0, "maximum sandboxes on this node; 0 = no limit")
	s.String(&cfg.LocalNetDial, "local-net-dial", "", "host[:port] laptops dial for this node's local-net tunnels (default: the control plane's ASP_LOCAL_NET_DIAL)")
	s.String(&cfg.GuestSubnet, "guest-subnet", "10.200.0.0/16", "guest pool: each TAP gets its own /30 from it; also the nft --egress-nft-redirect match")
	s.Deprecated("guest-ssh-agent-auto", "ASP_GUEST_SSH_AGENT_AUTO", "it only logged what the guest image does; the image decides whether its ssh-agent-vsock service runs")
	s.Duration(&cfg.ReconcileEvery, "reconcile-interval", 2*time.Second, "reconciler poll interval")
	s.Int(&cfg.ReconcileWorkers, "reconcile-workers", reconciler.DefaultWorkers, "sandboxes the reconciler starts or stops at once (1 with --ch-api-socket)")
	s.Duration(&cfg.GuestReadyTimeout, "guest-ready-timeout", 120*time.Second, "wait up to this long for pod-daemon in a new VM to answer before reporting running; a guest that never does fails the start (0: report running as soon as the VMM is up; ignored with --dry-run)")
	s.Duration(&cfg.HeartbeatEvery, "heartbeat-interval", 30*time.Second, "control-plane heartbeat interval")
}

func loadConfig() config {
	var cfg config
	s := settings.New(flag.CommandLine, nil)
	declareSettings(s, &cfg)
	src, err := useConfigFile(s, os.Args[1:], nil)
	if err != nil {
		slog.Error("configuration", "error", err)
		os.Exit(2)
	}
	src.warnLooseSecrets(s, stderrWarn)
	if err := s.Parse(os.Args[1:]); err != nil {
		slog.Error("configuration", "error", err)
		os.Exit(2)
	}
	if cfg.PrintConfig {
		printConfig(os.Stdout, s, src)
		os.Exit(0)
	}

	// ADR-0007 phase 4: confirm is on in the multi-user profile and with a per-sandbox
	// agent socket template, unless the operator said otherwise (flag or variable).
	cfg.SSHAgentConfirm = sshAgentConfirm(cfg)

	if err := resolveEgress(&cfg); err != nil {
		slog.Error("egress configuration", "error", err)
		os.Exit(2)
	}
	if cfg.EnrollURL == "" {
		cfg.EnrollURL = cfg.ControlPlaneURL
	}
	cfg.GuestVerify = strings.ToLower(strings.TrimSpace(cfg.GuestVerify))
	switch cfg.GuestVerify {
	case reconciler.GuestVerifyAuto, reconciler.GuestVerifyOn, reconciler.GuestVerifyOff:
	default:
		slog.Error("--guest-verify takes auto, on or off", "value", cfg.GuestVerify)
		os.Exit(2)
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
	for _, k := range []string{"ASP_FENCE_ENDPOINT", "ASP_FENCE_TOKEN"} {
		if os.Getenv(k) != "" {
			slog.Warn(k+" is ignored: a node does not choose how it is powered off; an operator sets the fence target on the control plane (asp node fence set)", "variable", k)
		}
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
		ID:               cfg.NodeID,
		Name:             cfg.NodeID,
		Endpoint:         cfg.Endpoint,
		AgentEndpoint:    agentEndpointURL(cfg),
		VMMProfiles:      []string{"cloud-hypervisor"},
		CapacityCPU:      cfg.CapacityCPU,
		CapacityMemMiB:   cfg.CapacityMemMiB,
		MaxSandboxes:     cfg.MaxSandboxes,
		AcceptsWork:      &acceptsWork,
		LocalNetDial:     cfg.LocalNetDial,
		AgentInstanceID:  cfg.InstanceID,
		AgentVersion:     version.Short(),
		EgressEnforced:   cfg.EgressEnforced,
		AdoptedSandboxes: cfg.AdoptedSandboxes,
	}
}

// newInstanceID identifies this process to the control plane. A new id tells it
// that the agent restarted: it stops the sandboxes the new process did not adopt
// (their VMs are gone), and keeps the ones it lists in the register body.
func newInstanceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("pid-%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func enrollNode(ctx context.Context, cfg config, c *cpclient.Client, now time.Time) (string, error) {
	credential := cfg.EnrollToken
	if credential == "" {
		credential = cfg.BootstrapToken
	}
	resp, err := c.Enroll(ctx, credential, cpclient.EnrollRequest{
		ID:             cfg.NodeID,
		Name:           cfg.NodeID,
		Endpoint:       cfg.Endpoint,
		AgentEndpoint:  agentEndpointURL(cfg),
		VMMProfiles:    []string{"cloud-hypervisor"},
		CapacityCPU:    cfg.CapacityCPU,
		CapacityMemMiB: cfg.CapacityMemMiB,
	})
	if err != nil {
		if cpclient.IsConflict(err) && cfg.NodeID != "" && certValidFor(cfg.CertDir, cfg.NodeID, now) {
			slog.Info("node already enrolled: keeping the certificate in cert-dir (an admin re-keys it with asp node enroll-token --node-id)",
				"node_id", cfg.NodeID, "cert_dir", cfg.CertDir)
			return cfg.NodeID, nil
		}
		return "", err
	}
	if err := cpclient.WriteCerts(cfg.CertDir, resp.ClientCertPEM, resp.ClientKeyPEM, resp.CACertPEM); err != nil {
		return "", fmt.Errorf("write certs: %w", err)
	}
	nodeID := cfg.NodeID
	if resp.NodeID != "" {
		nodeID = resp.NodeID
	}
	slog.Info("enrolled", "node_id", nodeID, "cert_dir", cfg.CertDir, "fingerprint", resp.CertFingerprint)
	return nodeID, nil
}

// certValidFor reports whether cert-dir holds a node certificate for nodeID
// that has not expired.
func certValidFor(certDir, nodeID string, now time.Time) bool {
	cert := readNodeCert(certDir)
	return cert != nil && strings.TrimSpace(cert.Subject.CommonName) == nodeID && now.Before(cert.NotAfter)
}

func readNodeCert(certDir string) *x509.Certificate {
	raw, err := os.ReadFile(filepath.Join(certDir, "client.crt"))
	if err != nil {
		return nil
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	return cert
}

// attestSigners lists the keys that sign boot attestations, in the order
// they are tried. Over mTLS to an https control plane the node certificate's
// key comes first: the control plane verifies it with the certificate the
// request comes with (ASP_CLIENT_CA), so a node can only attest as itself.
// The key is read at signing time, so it follows certificate renewal.
// ASP_ATTEST_KEY follows; the control plane must trust its public key
// (shared in a single-host lab, or listed in ASP_ATTEST_TRUSTED_PUBS).
func attestSigners(cfg config, nodeCert *cpclient.NodeCert) []*attest.Signer {
	var out []*attest.Signer
	if nodeCert != nil && isHTTPS(cfg.ControlPlaneURL) {
		s := attest.NewKeySigner(func() *ecdsa.PrivateKey {
			k, _ := nodeCert.Current().PrivateKey.(*ecdsa.PrivateKey)
			return k
		})
		slog.Info("attestations signed with the node certificate key", "key_id", s.KeyID())
		out = append(out, s)
	}
	if prod, _ := nodeProdMode(cfg); prod && strings.TrimSpace(os.Getenv("ASP_ATTEST_KEY")) == "" {
		// No lab key in the temporary directory on a production node.
		slog.Info("ASP_ATTEST_KEY unset: no fallback attestation key (set it to a persistent path the control plane trusts to have one)")
		return out
	}
	s, err := attest.LoadOrCreate()
	if err != nil {
		slog.Warn("ASP_ATTEST_KEY unavailable", "error", err)
		return out
	}
	slog.Info("attestations can be signed with ASP_ATTEST_KEY: the control plane must trust its public key", "key_id", s.KeyID())
	return append(out, s)
}

func isHTTPS(rawURL string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(rawURL)), "https://")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// controlPlaneTrustWarning is the warning for an agent that verifies a remote
// control plane's TLS certificate against the enrollment CA (certDir/ca.crt,
// the CA that signs every node certificate) because --control-plane-ca is not
// set. Node certificates carry only their node id and the control plane reserves
// the names of its own certificate, so a node cannot pass for the control plane
// by name; pinning a CA that signs nothing but the control plane's certificate
// removes the question. Empty when there is nothing to say: a loopback control
// plane, plain HTTP, an explicit CA, or no enrollment CA in use.
func controlPlaneTrustWarning(cfg config, haveEnrollmentCA bool) string {
	if cfg.ControlPlaneCA != "" || !haveEnrollmentCA || !isHTTPS(cfg.ControlPlaneURL) {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(cfg.ControlPlaneURL))
	if err != nil || isLoopbackHost(u.Hostname()) {
		return ""
	}
	return "the control plane's TLS certificate is verified against the enrollment CA, which also signs every node certificate; " +
		"pass --control-plane-ca (ASP_CONTROL_PLANE_CA) with the CA that signed the control plane's certificate alone"
}

// certNodeID returns the CN of the enrolled node certificate in certDir, or "".
func certNodeID(certDir string) string {
	cert := readNodeCert(certDir)
	if cert == nil {
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
	return fmt.Errorf("--agent-listen %s is not a loopback address, and the plain exec API is HTTP: its bearer token would cross the network in the clear; use --agent-tls-listen for a control plane on another host, or --insecure-agent-listen (lab only)", addr)
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

// sshAgentConfirm resolves --ssh-agent-confirm: what the operator said, by flag or
// by ASP_SSH_AGENT_CONFIRM (the flag wins); unset, on in the multi-user profile and
// with a per-sandbox agent socket template.
func sshAgentConfirm(cfg config) bool {
	if cfg.sshAgentConfirm.set {
		return cfg.sshAgentConfirm.value
	}
	return cfg.MultiUser || cfg.SSHAgentSockTemplate != ""
}

// diskMinFreeMiB resolves --disk-min-free-mib: a negative value is twice the
// base image's size, enough for a clone to grow, or 0 when the image is not
// there to measure.
func diskMinFreeMiB(flagValue int, baseImage string) int64 {
	if flagValue >= 0 {
		return int64(flagValue)
	}
	fi, err := os.Stat(baseImage)
	if err != nil {
		return 0
	}
	return 2 * (fi.Size() >> 20)
}

// egressGuard builds the destination guard of the egress proxy: the operator's
// private allow list, and the guests' own network as never reachable.
func egressGuard(cfg config) (*egress.DialGuard, error) {
	allow, err := egress.ParseCIDRs(cfg.EgressAllowCIDRs)
	if err != nil {
		return nil, fmt.Errorf("--egress-allow-cidr: %w", err)
	}
	g := &egress.DialGuard{AllowCIDRs: allow}
	if sub := strings.TrimSpace(cfg.GuestSubnet); sub != "" {
		p, err := netip.ParsePrefix(sub)
		if err != nil {
			return nil, fmt.Errorf("--guest-subnet: %w", err)
		}
		g.Never = append(g.Never, p.Masked())
	}
	return g, nil
}

// agentTokenFor loads, or creates, the secret that guards the local API. With
// no --agent-token-file it uses the default path; if that directory is not
// writable (a lab run without root) it falls back to a file in the temporary
// directory and says where, so the control plane can be pointed at it.
func agentTokenFor(cfg config) (string, error) {
	path := strings.TrimSpace(cfg.AgentTokenFile)
	explicit := path != ""
	if !explicit {
		path = execproxy.DefaultTokenFile
	}
	tok, err := execproxy.LoadOrCreateToken(path)
	if err != nil && !explicit {
		alt := filepath.Join(os.TempDir(), "asp-agent.token")
		var altErr error
		if tok, altErr = execproxy.LoadOrCreateToken(alt); altErr == nil {
			slog.Warn("agent token file is not writable here; using a temporary one: point the control plane at it with ASP_AGENT_TOKEN_FILE",
				"wanted", path, "using", alt, "error", err)
			return tok, nil
		}
	}
	if err != nil {
		return "", err
	}
	slog.Info("local API requires the agent token", "token_file", path)
	return tok, nil
}

// virtiofsSandbox picks virtiofsd's --sandbox mode: the one asked for, or
// chroot when the agent is root (it confines the daemon to the workspace) and
// none when it is not, since chroot needs CAP_SYS_CHROOT.
func virtiofsSandbox(mode string, euid int) (string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if !virtiofs.ValidSandbox(mode) {
		return "", fmt.Errorf("%q: want chroot, namespace or none", mode)
	}
	if mode != "" {
		return mode, nil
	}
	if euid == 0 {
		return virtiofs.SandboxChroot, nil
	}
	return virtiofs.SandboxNone, nil
}

// loadNodeAPIKey returns the key this node sends to the control plane: the
// file named by --api-key-file, else ASP_NODE_API_KEY, else none (mutual TLS).
func loadNodeAPIKey(cfg config) (string, error) {
	if path := strings.TrimSpace(cfg.APIKeyFile); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("--api-key-file: %w", err)
		}
		key := strings.TrimSpace(string(b))
		if key == "" {
			return "", fmt.Errorf("--api-key-file %s is empty", path)
		}
		return key, nil
	}
	return strings.TrimSpace(os.Getenv("ASP_NODE_API_KEY")), nil
}
