package api

import "net/http"

// Routes is the control-plane HTTP route table. cmd/api serves it behind
// AuthMiddleware; tests use it directly, so a route cannot be missing from one.
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	mux.HandleFunc("GET /.well-known/openid-configuration", s.OpenIDConfiguration)
	mux.HandleFunc("GET /oidc/jwks.json", s.JWKS)
	mux.HandleFunc("POST /v1/internal/oidc/token", s.MintOIDCToken)
	mux.HandleFunc("POST /v1/sandboxes", s.CreateSandbox)
	mux.HandleFunc("GET /v1/sandboxes", s.ListSandboxes)
	mux.HandleFunc("GET /v1/sandboxes/{id}", s.GetSandbox)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", s.DestroySandbox)
	mux.HandleFunc("GET /v1/sandboxes/{id}/events", s.ListSandboxEvents)
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", s.Exec)
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec/stdin", s.ExecStdin)
	mux.HandleFunc("POST /v1/sandboxes/{id}/local-net/grant", s.IssueLocalNetGrant)
	mux.HandleFunc("POST /v1/sandboxes/{id}/local-net/node-public", s.RegisterLocalNetNode)
	mux.HandleFunc("POST /v1/sandboxes/{id}/local-net/heartbeat", s.HeartbeatLocalNet)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}/local-net/attach", s.DetachLocalNet)
	mux.HandleFunc("POST /v1/sandboxes/{id}/claim", s.ClaimSandbox)
	mux.HandleFunc("POST /v1/sandboxes/{id}/renew-lease", s.RenewSandboxLease)
	mux.HandleFunc("POST /v1/sandboxes/{id}/status", s.UpdateSandboxStatus)
	mux.HandleFunc("POST /v1/sandboxes/{id}/attest", s.StoreAttestation)
	mux.HandleFunc("GET /v1/sandboxes/{id}/attestation", s.GetAttestation)
	mux.HandleFunc("POST /v1/attestation/verify", s.VerifyAttestation)
	mux.HandleFunc("PUT /v1/tenants/{id}/egress", s.PutTenantEgress)
	mux.HandleFunc("GET /v1/tenants/{id}/egress", s.GetTenantEgress)
	mux.HandleFunc("POST /v1/tenants/{id}/egress/check", s.CheckTenantEgress)
	mux.HandleFunc("POST /v1/nodes/enroll", s.EnrollNode)
	mux.HandleFunc("POST /v1/nodes/register", s.RegisterNode)
	mux.HandleFunc("POST /v1/nodes/{id}/heartbeat", s.HeartbeatNode)
	mux.HandleFunc("POST /v1/nodes/{id}/rotate-cert", s.RotateNodeCert)
	mux.HandleFunc("POST /v1/nodes/{id}/revoke", s.RevokeNode)
	mux.HandleFunc("GET /v1/nodes/{id}/work", s.ListNodeWork)
	mux.HandleFunc("GET /v1/nodes", s.ListNodes)
	return mux
}
