package api

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/attest"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/fence"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/oidc"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/pki"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

type errorResponse struct {
	Error string `json:"error"`
}

// Server holds dependencies for HTTP handlers.
type Server struct {
	Store    store.Store
	CA       *pki.CA
	OIDC     *oidc.Signer
	Attestor *attest.SoftwareAttestor
	Fence    fence.FenceProvider
	Client   *http.Client
}

func NewServer(s store.Store) *Server {
	return &Server{
		Store:  s,
		Client: &http.Client{Timeout: 30 * time.Second},
	}
}

type listSandboxesResponse struct {
	Sandboxes []store.Sandbox `json:"sandboxes"`
}

type listNodesResponse struct {
	Nodes []store.Node `json:"nodes"`
}

type listEventsResponse struct {
	Events []store.SandboxEvent `json:"events"`
}

type enrollResponse struct {
	NodeID          string     `json:"node_id"`
	ClientCertPEM   string     `json:"client_cert_pem"`
	ClientKeyPEM    string     `json:"client_key_pem"`
	CACertPEM       string     `json:"ca_cert_pem"`
	CertFingerprint string     `json:"cert_fingerprint"`
	CertSerial      string     `json:"cert_serial,omitempty"`
	Node            store.Node `json:"node"`
}

type execRequest struct {
	Cmd []string          `json:"cmd"`
	Env map[string]string `json:"env,omitempty"`
	Cwd string            `json:"cwd,omitempty"`
	// ActorSub optional; X-ASP-Actor-Sub header wins (ADR-0007 phase 1).
	ActorSub string `json:"actor_sub,omitempty"`
}

// HeaderASPActorSub carries the human/service actor for lab when no IdP JWT is present (ADR-0007).
const HeaderASPActorSub = "X-ASP-Actor-Sub"

// resolveActorSub prefers IdP JWT sub, then header, then body, then optional create-time owner fallback.
func resolveActorSub(r *http.Request, bodyActor, ownerFallback string) string {
	if p, ok := IdPPrincipalFromContext(r.Context()); ok && strings.TrimSpace(p.Sub) != "" {
		return strings.TrimSpace(p.Sub)
	}
	if v := strings.TrimSpace(r.Header.Get(HeaderASPActorSub)); v != "" {
		return v
	}
	if v := strings.TrimSpace(bodyActor); v != "" {
		return v
	}
	return strings.TrimSpace(ownerFallback)
}

type execResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// CreateSandbox validates a request and persists a sandbox.
// With ASP_AUTO_PROVISION=1 the sync stub moves it to running; otherwise it stays requested.
// When an IdP JWT is present (ADR-0007 phase 2–3), owner_sub/actor_sub come from the token sub;
// a forged body owner_sub that disagrees is rejected. Token email fills owner_email when present.
// Phase 3: create requires admin or operator role from IdP groups/roles claims.
func (s *Server) CreateSandbox(w http.ResponseWriter, r *http.Request) {
	var input store.CreateSandboxInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	input.OwnerSub = strings.TrimSpace(input.OwnerSub)
	input.OwnerEmail = strings.TrimSpace(input.OwnerEmail)
	if p, ok := IdPPrincipalFromContext(r.Context()); ok && strings.TrimSpace(p.Sub) != "" {
		if !canCreate(p) {
			forbid(w, "forbidden: create requires admin or operator role")
			return
		}
		sub := strings.TrimSpace(p.Sub)
		if input.OwnerSub != "" && input.OwnerSub != sub {
			writeError(w, http.StatusForbidden, "owner_sub does not match IdP token sub")
			return
		}
		input.OwnerSub = sub
		if email := strings.TrimSpace(p.Email); email != "" {
			input.OwnerEmail = email
		}
		input.ActorSub = sub
	} else {
		input.ActorSub = resolveActorSub(r, input.ActorSub, input.OwnerSub)
	}
	sb, err := s.Store.CreateSandbox(input)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sb)
}

// GetSandbox returns the current sandbox and its observed state.
func (s *Server) GetSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "sandbox id required")
		return
	}
	sb, err := s.Store.GetSandbox(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p, ok := IdPPrincipalFromContext(r.Context()); ok {
		if !canGet(p, sb) {
			forbid(w, "forbidden: insufficient role to get sandbox")
			return
		}
	}
	writeJSON(w, http.StatusOK, sb)
}

// ListSandboxes returns sandboxes, optionally filtered by tenant_id.
// With IdP JWT (phase 3): requires viewer+; list is tenant-wide for admin/operator/viewer
// (see filterSandboxesForList / ADR-0007 phase-3 choice).
func (s *Server) ListSandboxes(w http.ResponseWriter, r *http.Request) {
	if p, ok := IdPPrincipalFromContext(r.Context()); ok {
		if !canList(p) {
			forbid(w, "forbidden: list requires viewer, operator, or admin role")
			return
		}
	}
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenant_id"))
	list, err := s.Store.ListSandboxes(tenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []store.Sandbox{}
	}
	if p, ok := IdPPrincipalFromContext(r.Context()); ok {
		list = filterSandboxesForList(p, list)
	}
	writeJSON(w, http.StatusOK, listSandboxesResponse{Sandboxes: list})
}

// ListSandboxEvents returns the audit trail for a sandbox.
func (s *Server) ListSandboxEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "sandbox id required")
		return
	}
	sb, err := s.Store.GetSandbox(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p, ok := IdPPrincipalFromContext(r.Context()); ok {
		if !canGet(p, sb) {
			forbid(w, "forbidden: insufficient role to list events")
			return
		}
	}
	events, err := s.Store.ListEvents(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if events == nil {
		events = []store.SandboxEvent{}
	}
	writeJSON(w, http.StatusOK, listEventsResponse{Events: events})
}

// EnrollNode issues a client certificate after validating the bootstrap token.
func (s *Server) EnrollNode(w http.ResponseWriter, r *http.Request) {
	if !checkBootstrapToken(r) {
		writeError(w, http.StatusUnauthorized, "invalid or missing bootstrap token")
		return
	}
	if s.CA == nil {
		writeError(w, http.StatusServiceUnavailable, "enrollment CA not configured")
		return
	}
	var input store.EnrollNodeInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	nodeID := strings.TrimSpace(input.ID)
	if nodeID == "" {
		nodeID = strings.TrimSpace(input.Name)
	}
	if nodeID == "" {
		// allocate id via store by leaving empty — but we need it for CN first
		nodeID = newNodeIDFallback()
		input.ID = nodeID
	} else {
		input.ID = nodeID
	}
	issued, err := s.CA.IssueNodeClient(nodeID, pki.DefaultNodeTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "issue cert: "+err.Error())
		return
	}
	node, err := s.Store.EnrollNode(input, store.CertMeta{
		Fingerprint: issued.Fingerprint,
		Serial:      issued.Serial,
	})
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, enrollResponse{
		NodeID:          node.ID,
		ClientCertPEM:   string(issued.CertPEM),
		ClientKeyPEM:    string(issued.KeyPEM),
		CACertPEM:       string(s.CA.CertPEM),
		CertFingerprint: issued.Fingerprint,
		CertSerial:      issued.Serial,
		Node:            node,
	})
}

// RotateNodeCert issues a replacement client certificate (bootstrap token or admin API key).
func (s *Server) RotateNodeCert(w http.ResponseWriter, r *http.Request) {
	if !authorizeNodeCertAdmin(r, s.Store) {
		writeError(w, http.StatusUnauthorized, "bootstrap token or admin api key required")
		return
	}
	if s.CA == nil {
		writeError(w, http.StatusServiceUnavailable, "enrollment CA not configured")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "node id required")
		return
	}
	if _, err := s.Store.GetNode(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "node not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	issued, err := s.CA.IssueNodeClient(id, pki.DefaultNodeTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "issue cert: "+err.Error())
		return
	}
	node, err := s.Store.RotateNodeCert(id, store.CertMeta{
		Fingerprint: issued.Fingerprint,
		Serial:      issued.Serial,
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "node not found")
			return
		}
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, enrollResponse{
		NodeID:          node.ID,
		ClientCertPEM:   string(issued.CertPEM),
		ClientKeyPEM:    string(issued.KeyPEM),
		CACertPEM:       string(s.CA.CertPEM),
		CertFingerprint: issued.Fingerprint,
		CertSerial:      issued.Serial,
		Node:            node,
	})
}

// RevokeNode marks a node and its current client cert as revoked.
func (s *Server) RevokeNode(w http.ResponseWriter, r *http.Request) {
	if !authorizeNodeCertAdmin(r, s.Store) {
		writeError(w, http.StatusUnauthorized, "bootstrap token or admin api key required")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "node id required")
		return
	}
	node, err := s.Store.RevokeNode(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "node not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, node)
}

// authorizeNodeCertAdmin accepts the node bootstrap token or a valid API key.
func authorizeNodeCertAdmin(r *http.Request, st store.Store) bool {
	if checkBootstrapToken(r) {
		return true
	}
	raw := bearerToken(r.Header.Get("Authorization"))
	if raw == "" {
		return false
	}
	hash := store.HashAPIKeySecret(raw)
	if _, err := st.LookupAPIKeyByHash(hash); err != nil {
		return false
	}
	return true
}

// RegisterNode records a node-agent registration.
func (s *Server) RegisterNode(w http.ResponseWriter, r *http.Request) {
	var input store.RegisterNodeInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	node, err := s.Store.RegisterNode(input)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, node)
}

// HeartbeatNode updates last_seen for a registered/enrolled node.
func (s *Server) HeartbeatNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "node id required")
		return
	}
	node, err := s.Store.HeartbeatNode(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "node not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, node)
}

// ListNodes returns registered nodes.
func (s *Server) ListNodes(w http.ResponseWriter, _ *http.Request) {
	list, err := s.Store.ListNodes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []store.Node{}
	}
	writeJSON(w, http.StatusOK, listNodesResponse{Nodes: list})
}

// Exec authorizes and proxies an execution request to the sandbox's node-agent.
func (s *Server) Exec(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "sandbox id required")
		return
	}
	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.Cmd) == 0 {
		writeError(w, http.StatusBadRequest, "cmd required")
		return
	}
	actorSub := resolveActorSub(r, req.ActorSub, "")
	sb, err := s.Store.GetSandbox(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p, ok := IdPPrincipalFromContext(r.Context()); ok {
		if !canExec(p, sb) {
			forbid(w, "forbidden: exec requires owner, admin, or operator")
			return
		}
	}
	if sb.NodeID == nil || *sb.NodeID == "" {
		writeError(w, http.StatusConflict, "sandbox has no assigned node")
		return
	}
	node, err := s.Store.GetNode(*sb.NodeID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusConflict, "assigned node not registered")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	agentURL := strings.TrimRight(node.AgentEndpoint, "/")
	if agentURL == "" {
		agentURL = strings.TrimRight(node.Endpoint, "/")
	}
	if agentURL == "" || strings.HasPrefix(agentURL, "local://") {
		writeError(w, http.StatusBadGateway, "node has no agent_endpoint for exec")
		return
	}
	egressPol := s.effectiveEgress(sb.TenantID)
	payload, _ := json.Marshal(map[string]any{
		"sandbox_id":       sb.ID,
		"cmd":              req.Cmd,
		"env":              req.Env,
		"cwd":              req.Cwd,
		"egress_allowlist": egressPol,
	})
	url := agentURL + "/v1/internal/exec"
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "node-agent unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("node-agent status %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))
		return
	}
	var out execResponse
	if err := json.Unmarshal(body, &out); err != nil {
		writeError(w, http.StatusBadGateway, "invalid node-agent exec response")
		return
	}
	_ = s.Store.EmitEvent(store.EmitEventInput{
		SandboxID: sb.ID,
		TenantID:  sb.TenantID,
		EventType: "sandbox.exec",
		Actor:     "api",
		ActorSub:  actorSub,
		Payload:   mustJSON(map[string]any{"argc": len(req.Cmd), "exit_code": out.ExitCode}),
	})
	writeJSON(w, http.StatusOK, out)
}

func checkBootstrapToken(r *http.Request) bool {
	expected := strings.TrimSpace(os.Getenv("ASP_NODE_BOOTSTRAP_TOKEN"))
	if expected == "" {
		return false
	}
	got := bearerToken(r.Header.Get("Authorization"))
	if got == "" {
		got = strings.TrimSpace(r.Header.Get("X-ASP-Bootstrap-Token"))
	}
	return got != "" && got == expected
}

func newNodeIDFallback() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

type egressRuleRequest struct {
	HostPattern string `json:"host_pattern"`
	Port        *int   `json:"port,omitempty"`
	Enabled     *bool  `json:"enabled,omitempty"`
}

type putEgressRequest struct {
	Rules []egressRuleRequest `json:"rules"`
}

type egressRulesResponse struct {
	TenantID string             `json:"tenant_id"`
	Rules    []store.EgressRule `json:"rules"`
	Policy   store.EgressPolicy `json:"policy"`
}

type egressCheckRequest struct {
	Host string `json:"host"`
	Port int    `json:"port,omitempty"`
}

// GetTenantEgress returns stored rules + effective policy.
func (s *Server) GetTenantEgress(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "tenant id required")
		return
	}
	if p, ok := IdPPrincipalFromContext(r.Context()); ok {
		if !canManageEgress(p) {
			forbid(w, "forbidden: egress policy requires admin role")
			return
		}
	}
	rules, err := s.Store.ListEgressRules(id)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rules == nil {
		rules = []store.EgressRule{}
	}
	writeJSON(w, http.StatusOK, egressRulesResponse{
		TenantID: id,
		Rules:    rules,
		Policy:   store.EffectiveEgress(id, rules),
	})
}

// PutTenantEgress replaces the tenant egress allowlist.
func (s *Server) PutTenantEgress(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "tenant id required")
		return
	}
	if p, ok := IdPPrincipalFromContext(r.Context()); ok {
		if !canManageEgress(p) {
			forbid(w, "forbidden: egress policy requires admin role")
			return
		}
	}
	var req putEgressRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	rules := make([]store.EgressRule, 0, len(req.Rules))
	for _, rr := range req.Rules {
		enabled := true
		if rr.Enabled != nil {
			enabled = *rr.Enabled
		}
		rules = append(rules, store.EgressRule{
			HostPattern: rr.HostPattern,
			Port:        rr.Port,
			Enabled:     enabled,
		})
	}
	out, err := s.Store.PutEgressRules(id, rules)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, egressRulesResponse{
		TenantID: id,
		Rules:    out,
		Policy:   store.EffectiveEgress(id, out),
	})
}

// CheckTenantEgress is a CP test helper: evaluate host against effective policy.
func (s *Server) CheckTenantEgress(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "tenant id required")
		return
	}
	if p, ok := IdPPrincipalFromContext(r.Context()); ok {
		if !canManageEgress(p) {
			forbid(w, "forbidden: egress policy requires admin role")
			return
		}
	}
	var req egressCheckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Host) == "" {
		writeError(w, http.StatusBadRequest, "host required")
		return
	}
	pol := s.effectiveEgress(id)
	allowed := evaluateEgress(pol, req.Host, req.Port)
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant_id": id,
		"host":      req.Host,
		"port":      req.Port,
		"allowed":   allowed,
		"policy":    pol,
	})
}

func (s *Server) effectiveEgress(tenantID string) store.EgressPolicy {
	rules, err := s.Store.ListEgressRules(tenantID)
	if err != nil {
		return store.EffectiveEgress(tenantID, nil)
	}
	return store.EffectiveEgress(tenantID, rules)
}

// evaluateEgress mirrors node-agent allowlist matching (exact + *.suffix).
func evaluateEgress(pol store.EgressPolicy, host string, port int) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexAny(host, "/:"); i >= 0 {
		host = host[:i]
	}
	if host == "" {
		return false
	}
	if pol.Mode == store.EgressModeAllowAll && len(pol.Rules) == 0 {
		return true
	}
	if len(pol.Rules) == 0 {
		return false
	}
	for _, r := range pol.Rules {
		if !r.Enabled {
			continue
		}
		pat := strings.ToLower(r.HostPattern)
		match := host == pat
		if !match && strings.HasPrefix(pat, "*.") {
			suffix := pat[1:]
			match = strings.HasSuffix(host, suffix) || host == pat[2:]
		}
		if !match {
			continue
		}
		if r.Port == nil || port <= 0 || *r.Port == port {
			return true
		}
	}
	return false
}

// OpenIDConfiguration serves OIDC discovery.
func (s *Server) OpenIDConfiguration(w http.ResponseWriter, _ *http.Request) {
	if s.OIDC == nil {
		writeError(w, http.StatusServiceUnavailable, "oidc not configured")
		return
	}
	b, err := s.OIDC.OpenIDConfiguration()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(b, '\n'))
}

// JWKS serves the signing public keys.
func (s *Server) JWKS(w http.ResponseWriter, _ *http.Request) {
	if s.OIDC == nil {
		writeError(w, http.StatusServiceUnavailable, "oidc not configured")
		return
	}
	b, err := s.OIDC.JWKS()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(b, '\n'))
}

type oidcTokenRequest struct {
	SandboxID string `json:"sandbox_id"`
	Aud       string `json:"aud"`
	Nonce     string `json:"nonce,omitempty"`
}

// MintOIDCToken issues a short-lived JWT; tenant/sub come from the store (node cannot override).
func (s *Server) MintOIDCToken(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		writeError(w, http.StatusServiceUnavailable, "oidc not configured")
		return
	}
	var req oidcTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.SandboxID) == "" {
		writeError(w, http.StatusBadRequest, "sandbox_id required")
		return
	}
	if strings.TrimSpace(req.Aud) == "" {
		writeError(w, http.StatusBadRequest, "aud required")
		return
	}
	sb, err := s.Store.GetSandbox(req.SandboxID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	attClaim := s.attestationClaim(sb.ID)
	token, claims, err := s.OIDC.MintWithAttestation(sb.TenantID, sb.ID, req.Aud, req.Nonce, attClaim)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	expIn := claims.Expiry - claims.IssuedAt
	if expIn < 0 {
		expIn = 0
	}
	outClaims := map[string]any{
		"sub":        claims.Subject,
		"tenant_id":  claims.TenantID,
		"sandbox_id": claims.SandboxID,
		"aud":        claims.Audience,
		"iss":        claims.Issuer,
	}
	if claims.XAspAttestation != nil {
		outClaims["x_asp_attestation"] = claims.XAspAttestation
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": token,
		"token_type":   "Bearer",
		"expires_in":   expIn,
		"claims":       outClaims,
	})
}

type claimRequest struct {
	NodeID string `json:"node_id"`
}

type statusRequest struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

type listWorkResponse struct {
	Sandboxes []store.Sandbox `json:"sandboxes"`
}

// ListNodeWork returns sandboxes this node should claim or stop.
func (s *Server) ListNodeWork(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "node id required")
		return
	}
	list, err := s.Store.ListNodeWork(id)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []store.Sandbox{}
	}
	writeJSON(w, http.StatusOK, listWorkResponse{Sandboxes: list})
}

// ClaimSandbox atomically assigns a requested sandbox to a node.
func (s *Server) ClaimSandbox(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "sandbox id required")
		return
	}
	var req claimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if prev, err := s.Store.GetSandbox(id); err == nil {
		s.maybeFenceOnReclaim(r.Context(), prev)
	}
	sb, err := s.Store.ClaimSandbox(id, strings.TrimSpace(req.NodeID))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if errors.Is(err, store.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

// RenewSandboxLease extends the owning node's soft lease (multi-node fencing).
func (s *Server) RenewSandboxLease(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "sandbox id required")
		return
	}
	var req claimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	sb, err := s.Store.RenewSandboxLease(id, strings.TrimSpace(req.NodeID))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if errors.Is(err, store.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

// UpdateSandboxStatus records agent-observed lifecycle state.
func (s *Server) UpdateSandboxStatus(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "sandbox id required")
		return
	}
	var req statusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	state := store.SandboxState(strings.TrimSpace(req.State))
	sb, err := s.Store.UpdateSandboxStatus(id, state, strings.TrimSpace(req.Detail))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		if errors.Is(err, store.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

// DestroySandbox marks a sandbox stopping (reconciler cleans up) or stopped if never assigned.
func (s *Server) DestroySandbox(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "sandbox id required")
		return
	}
	actorSub := resolveActorSub(r, "", "")
	existing, err := s.Store.GetSandbox(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p, ok := IdPPrincipalFromContext(r.Context()); ok {
		if !canDestroy(p, existing) {
			forbid(w, "forbidden: destroy requires owner, admin, or operator with destroy-any")
			return
		}
	}
	sb, err := s.Store.MarkSandboxStopping(id, actorSub)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}
