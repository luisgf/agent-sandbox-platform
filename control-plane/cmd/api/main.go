package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/api"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/attest"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/authn/idp"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/fence"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/oidc"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/pki"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
	"github.com/luisgf/agent-sandbox-platform/control-plane/migrations"
)

func main() {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	ctx := context.Background()
	var st store.Store
	storeName := "memory"

	if dbURL := os.Getenv("DATABASE_URL"); dbURL != "" {
		pool, err := pgxpool.New(ctx, dbURL)
		if err != nil {
			slog.Error("connect postgres", "error", err)
			os.Exit(1)
		}
		defer pool.Close()
		if err := pool.Ping(ctx); err != nil {
			slog.Error("ping postgres", "error", err)
			os.Exit(1)
		}
		if err := store.ApplyMigrations(ctx, pool, migrations.FS, "."); err != nil {
			slog.Error("migrations", "error", err)
			os.Exit(1)
		}
		pg := store.NewPostgresStore(pool)
		if err := pg.EnsureBootstrapNode(ctx); err != nil {
			slog.Error("bootstrap node", "error", err)
			os.Exit(1)
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

	if secret := os.Getenv("ASP_BOOTSTRAP_API_KEY"); secret != "" {
		key, err := api.BootstrapAPIKey(st, secret)
		if err != nil {
			slog.Error("bootstrap api key", "error", err)
			os.Exit(1)
		}
		if key.ID != "" {
			slog.Info("bootstrap api key ready", "tenant", key.TenantID, "name", key.Name, "prefix", key.KeyPrefix)
		}
	}

	ca, err := pki.LoadOrCreateDevCA(pki.PathsFromEnv())
	if err != nil {
		slog.Error("load/create CA", "error", err)
		os.Exit(1)
	}
	slog.Info("enrollment CA ready", "fingerprint", pki.Fingerprint(ca.Cert))

	issuer := strings.TrimSpace(os.Getenv("ASP_OIDC_ISSUER"))
	if issuer == "" {
		issuer = "http://127.0.0.1" + addr
		if strings.HasPrefix(addr, ":") {
			issuer = "http://127.0.0.1" + addr
		}
	}
	oidcSigner, err := oidc.LoadOrCreate(issuer)
	if err != nil {
		slog.Error("oidc signer", "error", err)
		os.Exit(1)
	}
	slog.Info("oidc signer ready", "issuer", oidcSigner.Issuer, "kid", oidcSigner.KID(), "key_path", oidcSigner.KeyPath, "prev_key", oidcSigner.PrevPath != "")

	srv := api.NewServer(st)
	srv.CA = ca
	srv.OIDC = oidcSigner
	attestor, err := attest.LoadOrCreate()
	if err != nil {
		slog.Error("attestor", "error", err)
		os.Exit(1)
	}
	srv.Attestor = attestor
	srv.Fence = fence.FromEnv()
	slog.Info("attestor ready", "name", attestor.Name(), "kid", attestor.KID(), "fence", srv.Fence.Name())
	authCfg := api.AuthConfigFromEnv()
	idpVal, idpCfg, err := idp.FromEnv()
	if err != nil {
		slog.Error("idp config", "error", err)
		os.Exit(1)
	}
	authCfg.IdP = idpVal
	authCfg.IdPRequired = idpCfg.Required
	if idpCfg.Enabled() {
		slog.Info("idp jwt validation ready",
			"issuer", idpCfg.Issuer,
			"audience", idpCfg.Audience,
			"jwks_url", idpCfg.JWKSURL,
			"required", idpCfg.Required)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	mux.HandleFunc("GET /.well-known/openid-configuration", srv.OpenIDConfiguration)
	mux.HandleFunc("GET /oidc/jwks.json", srv.JWKS)
	mux.HandleFunc("POST /v1/internal/oidc/token", srv.MintOIDCToken)
	mux.HandleFunc("POST /v1/sandboxes", srv.CreateSandbox)
	mux.HandleFunc("GET /v1/sandboxes", srv.ListSandboxes)
	mux.HandleFunc("GET /v1/sandboxes/{id}", srv.GetSandbox)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", srv.DestroySandbox)
	mux.HandleFunc("GET /v1/sandboxes/{id}/events", srv.ListSandboxEvents)
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", srv.Exec)
	mux.HandleFunc("POST /v1/sandboxes/{id}/claim", srv.ClaimSandbox)
	mux.HandleFunc("POST /v1/sandboxes/{id}/renew-lease", srv.RenewSandboxLease)
	mux.HandleFunc("POST /v1/sandboxes/{id}/status", srv.UpdateSandboxStatus)
	mux.HandleFunc("POST /v1/sandboxes/{id}/attest", srv.StoreAttestation)
	mux.HandleFunc("GET /v1/sandboxes/{id}/attestation", srv.GetAttestation)
	mux.HandleFunc("POST /v1/attestation/verify", srv.VerifyAttestation)
	mux.HandleFunc("PUT /v1/tenants/{id}/egress", srv.PutTenantEgress)
	mux.HandleFunc("GET /v1/tenants/{id}/egress", srv.GetTenantEgress)
	mux.HandleFunc("POST /v1/tenants/{id}/egress/check", srv.CheckTenantEgress)
	mux.HandleFunc("POST /v1/nodes/enroll", srv.EnrollNode)
	mux.HandleFunc("POST /v1/nodes/register", srv.RegisterNode)
	mux.HandleFunc("POST /v1/nodes/{id}/heartbeat", srv.HeartbeatNode)
	mux.HandleFunc("POST /v1/nodes/{id}/rotate-cert", srv.RotateNodeCert)
	mux.HandleFunc("POST /v1/nodes/{id}/revoke", srv.RevokeNode)
	mux.HandleFunc("GET /v1/nodes/{id}/work", srv.ListNodeWork)
	mux.HandleFunc("GET /v1/nodes", srv.ListNodes)

	handler := api.AuthMiddleware(st, authCfg)(requestLog(mux))

	tlsCert := strings.TrimSpace(os.Getenv("ASP_TLS_CERT"))
	tlsKey := strings.TrimSpace(os.Getenv("ASP_TLS_KEY"))
	clientCAPath := strings.TrimSpace(os.Getenv("ASP_CLIENT_CA"))
	mtlsStrict := api.EnvTruthy("ASP_MTLS_STRICT")

	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	if tlsCert != "" && tlsKey != "" {
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if clientCAPath != "" {
			pemBytes, err := os.ReadFile(clientCAPath)
			if err != nil {
				slog.Error("read ASP_CLIENT_CA", "error", err)
				os.Exit(1)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pemBytes) {
				slog.Error("ASP_CLIENT_CA has no certificates")
				os.Exit(1)
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
			handler = api.AuthMiddleware(st, authCfg)(requestLog(mux))
			server.Handler = handler
		}
		server.TLSConfig = tlsCfg

		if mtlsStrict {
			enrollAddr := strings.TrimSpace(os.Getenv("ASP_ENROLL_LISTEN"))
			if enrollAddr == "" {
				enrollAddr = "127.0.0.1:8081"
			}
			go serveEnrollPlaintext(enrollAddr, st, authCfg, mux)
		}

		slog.Info("control-plane API listening (TLS)", "addr", addr, "store", storeName,
			"auth_require", authCfg.Require, "idp_required", authCfg.IdPRequired, "client_ca", clientCAPath != "",
			"mtls_strict", mtlsStrict)
		if err := server.ListenAndServeTLS(tlsCert, tlsKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("API stopped", "error", err)
			os.Exit(1)
		}
		return
	}

	if mtlsStrict {
		slog.Warn("ASP_MTLS_STRICT ignored without ASP_TLS_CERT/ASP_TLS_KEY")
	}

	slog.Info("control-plane API listening", "addr", addr, "store", storeName,
		"auth_require", authCfg.Require, "idp_required", authCfg.IdPRequired)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("API stopped", "error", err)
		os.Exit(1)
	}
}

// serveEnrollPlaintext binds a localhost-only plaintext listener for bootstrap
// enrollment when the main TLS listener requires client certs (ASP_MTLS_STRICT).
// Only /healthz and POST /v1/nodes/enroll are exposed.
func serveEnrollPlaintext(addr string, st store.Store, authCfg api.AuthConfig, fullMux http.Handler) {
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
	handler := api.AuthMiddleware(st, authCfg)(requestLog(gate))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("enroll plaintext listen", "addr", addr, "error", err)
		return
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	slog.Info("enroll plaintext listener (ASP_MTLS_STRICT)", "addr", addr,
		"note", "localhost-only recommended; use for bootstrap enroll / re-enroll")
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("enroll plaintext stopped", "error", err)
	}
}

func requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(started))
	})
}
