package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/api"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/attest"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/authn/idp"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/fence"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/oidc"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/pki"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/sched"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
	"github.com/luisgf/agent-sandbox-platform/control-plane/migrations"
)

// EnvShutdownTimeout bounds how long SIGTERM waits for in-flight requests.
const EnvShutdownTimeout = "ASP_SHUTDOWN_TIMEOUT"

const defaultShutdownTimeout = 30 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		slog.Error("control plane stopped", "error", err)
		stop()
		var cfgErr configError
		if errors.As(err, &cfgErr) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// run starts the control plane and serves until ctx is cancelled (SIGINT or
// SIGTERM in main), then shuts down: the listeners stop accepting, in-flight
// requests (streamed execs included) get up to ASP_SHUTDOWN_TIMEOUT to finish,
// the background loops stop with ctx, and the Postgres pool closes last.
func run(ctx context.Context, args []string) error {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		// Loopback unless the operator says otherwise: an API that can create
		// sandboxes and run commands in them is not something to offer to the
		// network by default.
		addr = "127.0.0.1:8080"
	}
	shutdownTimeout, err := shutdownTimeoutFromEnv()
	if err != nil {
		return err
	}
	if err := checkKeyLocations(); err != nil {
		return err
	}

	var st store.Store
	storeName := "memory"

	if dbURL := os.Getenv("DATABASE_URL"); dbURL != "" {
		pool, err := pgxpool.New(ctx, dbURL)
		if err != nil {
			return fmt.Errorf("connect postgres: %w", err)
		}
		defer pool.Close()
		if err := pool.Ping(ctx); err != nil {
			return fmt.Errorf("ping postgres: %w", err)
		}
		if err := store.ApplyMigrations(ctx, pool, migrations.FS, "."); err != nil {
			return fmt.Errorf("migrations: %w", err)
		}
		pg := store.NewPostgresStore(pool)
		if store.AutoProvisionEnabled() {
			// Only the stub provisioner uses the local-dev row.
			if err := pg.EnsureBootstrapNode(ctx); err != nil {
				return fmt.Errorf("bootstrap node: %w", err)
			}
		}
		st = pg
		storeName = "postgres"
		slog.Info("using Postgres store", "migrations", "ok")
	} else {
		st = store.NewMemoryStore()
		// Memory-dev: empty egress = allow-all unless ASP_EGRESS_DENY_DEFAULT=1.
		if !store.EnvTruthy("ASP_EGRESS_DENY_DEFAULT") && os.Getenv("ASP_EGRESS_DEFAULT_ALLOW") == "" {
			_ = os.Setenv("ASP_EGRESS_DEFAULT_ALLOW", "1")
		}
	}

	schedCfg, err := sched.ConfigFromEnv()
	if err != nil {
		return fmt.Errorf("scheduler config: %w", err)
	}
	if sc, ok := st.(interface{ SetSchedConfig(sched.Config) }); ok {
		sc.SetSchedConfig(schedCfg)
	}
	slog.Info("scheduler ready", "policy", schedCfg.Policy, "cpu_overcommit", schedCfg.CPUOvercommit,
		"node_stale_after", schedCfg.StaleAfter.String(), "auto_provision", store.AutoProvisionEnabled())

	if secret := os.Getenv("ASP_BOOTSTRAP_API_KEY"); secret != "" {
		key, err := api.BootstrapAPIKey(st, secret)
		if err != nil {
			return fmt.Errorf("bootstrap api key: %w", err)
		}
		if key.ID != "" {
			slog.Info("bootstrap api key ready", "tenant", key.TenantID, "name", key.Name, "prefix", key.KeyPrefix)
		}
	}

	ca, err := pki.LoadOrCreateDevCA(pki.PathsFromEnv())
	if err != nil {
		return fmt.Errorf("load/create CA: %w", err)
	}
	slog.Info("enrollment CA ready", "fingerprint", pki.Fingerprint(ca.Cert))

	issuer := strings.TrimSpace(os.Getenv("ASP_OIDC_ISSUER"))
	if issuer == "" {
		issuer = "http://127.0.0.1" + addr
	}
	oidcSigner, err := oidc.LoadOrCreate(issuer)
	if err != nil {
		return fmt.Errorf("oidc signer: %w", err)
	}
	slog.Info("oidc signer ready", "issuer", oidcSigner.Issuer, "kid", oidcSigner.KID(), "key_path", oidcSigner.KeyPath, "prev_key", oidcSigner.PrevPath != "")

	srv := api.NewServer(st)
	bufferedExec, err := bufferedExecTimeoutFromEnv(os.Getenv(api.EnvBufferedExecTimeout))
	if err != nil {
		return configError{err}
	}
	srv.BufferedExecTimeout = bufferedExec
	srv.CA = ca
	srv.Sched = schedCfg
	srv.Agents = api.NewAgentDialer(ca, api.EnvTruthy(api.EnvInsecureAgentHTTP))
	if srv.Agents.AllowInsecureHTTP {
		slog.Warn(api.EnvInsecureAgentHTTP + "=1: plain HTTP agent endpoints on other hosts are allowed; exec traffic is unauthenticated (lab only)")
	}
	srv.OIDC = oidcSigner
	attestor, err := attest.LoadOrCreate()
	if err != nil {
		return fmt.Errorf("attestor: %w", err)
	}
	srv.Attestor = attestor
	if path := strings.TrimSpace(os.Getenv(attest.EnvAllowedImages)); path != "" {
		allow, err := attest.LoadAllowlist(path)
		if err != nil {
			return configError{fmt.Errorf("%s: %w", attest.EnvAllowedImages, err)}
		}
		srv.AttestAllow = allow
		slog.Info("boot evidence is accepted only for allowlisted images", "path", path)
	} else {
		slog.Info("no image allowlist: boot evidence is accepted for any kernel and base image the node reports; set " + attest.EnvAllowedImages + " to restrict it")
	}
	srv.Fence = fence.FromEnv()
	slog.Info("attestor ready", "name", attestor.Name(), "kid", attestor.KID(), "fence", srv.Fence.Name())
	authCfg := api.AuthConfigFromEnv()
	idpVal, idpCfg, err := idp.FromEnv()
	if err != nil {
		return fmt.Errorf("idp config: %w", err)
	}
	if err := checkIdPAudience(idpCfg); err != nil {
		return err
	}
	authCfg.IdP = idpVal
	authCfg.IdPRequired = idpCfg.Required
	if err := checkAuthConfigured(st, idpVal != nil, authCfg); err != nil {
		return err
	}
	if idpVal != nil {
		go idpVal.Run(ctx)
	}

	idleTimeout, err := store.ResolveIdleTimeout(os.Getenv(store.EnvSandboxIdleTimeout), idleTimeoutFlag(args))
	if err != nil {
		return fmt.Errorf("idle timeout: %w", err)
	}
	srv.IdleTimeout = idleTimeout
	if idleTimeout > 0 {
		sweep := store.IdleSweepInterval(os.Getenv(store.EnvSandboxIdleSweep))
		go srv.RunIdleReaper(ctx, idleTimeout, sweep)
		slog.Info("sandbox idle reaper enabled", "timeout", idleTimeout.String(), "sweep", sweep.String(),
			"note", "activity = create, transition to running, an exec while it starts and runs, and when it ends; lease renew is not activity")
	} else {
		slog.Info("sandbox idle reaper disabled",
			"hint", "set ASP_SANDBOX_IDLE_TIMEOUT=2h (or 1h) or -idle-timeout 2h; 0/off disables")
	}
	retCfg, err := api.RetentionConfigFromEnv()
	if err != nil {
		return fmt.Errorf("retention config: %w", err)
	}
	if retCfg.Enabled() {
		go srv.RunRetentionReaper(ctx, retCfg)
		slog.Info("stopped sandbox retention enabled", "ttl", retCfg.TTL.String(),
			"max_stopped_per_tenant", retCfg.MaxStoppedPerTenant, "sweep", retCfg.Interval.String(),
			"note", "a stopped sandbox keeps its disk on its node until it is deleted or this expires")
		if store.IsMemory(st) {
			slog.Warn("stopped sandboxes are kept in memory: restarting the control plane forgets them, "+
				"and each node then removes their disks. Set DATABASE_URL (Postgres) to keep them across restarts",
				"store", storeName)
		}
	} else {
		slog.Info("stopped sandbox retention disabled: stopped sandboxes keep their disks until deleted",
			"hint", "ASP_STOPPED_SANDBOX_TTL defaults to 7d; 0/off keeps them for ever")
	}
	monCfg, err := api.NodeMonitorConfigFromEnv(schedCfg.StaleAfter)
	if err != nil {
		return fmt.Errorf("node monitor config: %w", err)
	}
	go srv.RunNodeMonitor(ctx, monCfg)
	slog.Info("node monitor enabled", "interval", monCfg.Interval.String(), "stale_after", monCfg.StaleAfter.String(),
		"failover_after", monCfg.FailoverAfter.String(), "note", "a lost node's sandboxes fail; they are not moved")
	if idpCfg.Enabled() {
		slog.Info("idp jwt validation ready",
			"issuer", idpCfg.Issuer,
			"audience", idpCfg.Audience,
			"jwks_url", idpCfg.JWKSURL,
			"required", idpCfg.Required,
			"role_claim", idpCfg.RoleClaim,
			"role_prefix", idpCfg.RolePrefix,
			"rbac", true)
	}

	mux := srv.Routes()

	tlsCert := strings.TrimSpace(os.Getenv("ASP_TLS_CERT"))
	tlsKey := strings.TrimSpace(os.Getenv("ASP_TLS_KEY"))
	clientCAPath := strings.TrimSpace(os.Getenv("ASP_CLIENT_CA"))
	mtlsStrict := api.EnvTruthy("ASP_MTLS_STRICT")
	useTLS := tlsCert != "" && tlsKey != ""
	if useTLS {
		names, err := tlsCertNames(tlsCert)
		if err != nil {
			return fmt.Errorf("read ASP_TLS_CERT: %w", err)
		}
		srv.ReservedNodeNames = names
	}

	var tlsCfg *tls.Config
	if useTLS {
		tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12}
		if clientCAPath != "" {
			pemBytes, err := os.ReadFile(clientCAPath)
			if err != nil {
				return fmt.Errorf("read ASP_CLIENT_CA: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pemBytes) {
				return errors.New("ASP_CLIENT_CA has no certificates")
			}
			tlsCfg.ClientCAs = pool
			if mtlsStrict {
				// RequireAndVerifyClientCert rejects anonymous TLS hits on all routes
				// of this listener. Enrollment must use ASP_ENROLL_LISTEN (plaintext
				// localhost) or rotate-cert with an existing client cert + admin key.
				tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
				slog.Info("ASP_MTLS_STRICT=1: RequireAndVerifyClientCert on TLS listener")
			} else {
				// VerifyClientCertIfGiven keeps /v1/nodes/enroll usable with
				// server-auth only; AuthMiddleware requires peer certs on
				// register/heartbeat/oidc mint.
				tlsCfg.ClientAuth = tls.VerifyClientCertIfGiven
			}
			authCfg.RequireNodeClientCert = true
			authCfg.RejectRevokedCerts = true
		}
	} else if mtlsStrict {
		slog.Warn("ASP_MTLS_STRICT ignored without ASP_TLS_CERT/ASP_TLS_KEY")
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	server := newHTTPServer(api.AuthMiddleware(st, authCfg)(api.RequestLog(mux)))
	server.TLSConfig = tlsCfg
	servers := []*trackedServer{server}
	serveErr := make(chan error, 2)
	go func() {
		if useTLS {
			serveErr <- server.ServeTLS(ln, tlsCert, tlsKey)
		} else {
			serveErr <- server.Serve(ln)
		}
	}()

	if useTLS && mtlsStrict {
		enrollAddr := strings.TrimSpace(os.Getenv("ASP_ENROLL_LISTEN"))
		if enrollAddr == "" {
			enrollAddr = "127.0.0.1:8081"
		}
		enroll, err := startEnrollPlaintext(enrollAddr, st, authCfg, mux, serveErr)
		if err != nil {
			slog.Error("enroll plaintext listen", "addr", enrollAddr, "error", err)
		} else {
			servers = append(servers, enroll)
		}
	}

	slog.Info("control-plane API listening", "addr", ln.Addr().String(), "tls", useTLS, "store", storeName,
		"insecure_open", authCfg.InsecureOpen, "idp_required", authCfg.IdPRequired, "client_ca", clientCAPath != "",
		"mtls_strict", mtlsStrict, "idle_timeout", idleTimeout.String(), "shutdown_timeout", shutdownTimeout.String())

	select {
	case err := <-serveErr:
		shutdownAll(servers, shutdownTimeout)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("API stopped: %w", err)
		}
	case <-ctx.Done():
		slog.Info("shutting down: waiting for in-flight requests", "timeout", shutdownTimeout.String())
		shutdownAll(servers, shutdownTimeout)
		slog.Info("control-plane API stopped")
	}
	return nil
}

// shutdownTimeoutFromEnv reads ASP_SHUTDOWN_TIMEOUT (default 30s).
func shutdownTimeoutFromEnv() (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(EnvShutdownTimeout))
	if v == "" {
		return defaultShutdownTimeout, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s=%q: want a duration greater than 0 (e.g. 30s)", EnvShutdownTimeout, v)
	}
	return d, nil
}

// trackedServer is an http.Server that counts its open connections, so a
// shutdown that runs out of time can say how many it is cutting.
type trackedServer struct {
	*http.Server
	open atomic.Int64
}

func newHTTPServer(h http.Handler) *trackedServer {
	s := &trackedServer{Server: &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}}
	s.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			s.open.Add(1)
		case http.StateClosed, http.StateHijacked:
			s.open.Add(-1)
		}
	}
	return s
}

// shutdownAll stops accepting on every server and waits up to timeout for the
// requests in flight; whatever is still open then is closed.
func shutdownAll(servers []*trackedServer, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	done := make(chan struct{}, len(servers))
	for _, s := range servers {
		go func(s *trackedServer) {
			defer func() { done <- struct{}{} }()
			if err := s.Shutdown(ctx); err != nil {
				slog.Warn("shutdown timeout reached; closing the connections still open",
					"open_connections", s.open.Load(), "error", err)
				_ = s.Close()
			}
		}(s)
	}
	for range servers {
		<-done
	}
}

// startEnrollPlaintext binds a localhost-only plaintext listener for bootstrap
// enrollment when the main TLS listener requires client certs (ASP_MTLS_STRICT).
// Only /healthz and POST /v1/nodes/enroll are exposed.
func startEnrollPlaintext(addr string, st store.Store, authCfg api.AuthConfig, fullMux http.Handler, serveErr chan<- error) (*trackedServer, error) {
	gate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/healthz":
			fullMux.ServeHTTP(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/nodes/enroll":
			fullMux.ServeHTTP(w, r)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"enroll listener only serves /healthz and POST /v1/nodes/enroll"}` + "\n"))
		}
	})
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := newHTTPServer(api.AuthMiddleware(st, authCfg)(api.RequestLog(gate)))
	slog.Info("enroll plaintext listener (ASP_MTLS_STRICT)", "addr", ln.Addr().String(),
		"note", "localhost-only recommended; use for bootstrap enroll / re-enroll")
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("enroll plaintext stopped", "error", err)
			serveErr <- err
		}
	}()
	return srv, nil
}

// idleTimeoutFlag returns the value of -idle-timeout / --idle-timeout, or "".
// Unknown args are ignored; the API process is otherwise env-driven.
func idleTimeoutFlag(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-idle-timeout" || a == "--idle-timeout" {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		for _, prefix := range []string{"--idle-timeout=", "-idle-timeout="} {
			if strings.HasPrefix(a, prefix) {
				return strings.TrimPrefix(a, prefix)
			}
		}
	}
	return ""
}
