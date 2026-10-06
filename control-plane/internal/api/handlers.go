package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/attest"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/fence"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/oidc"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/pki"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/sched"
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
	// Client calls same-host agents over plain HTTP; Agents builds mTLS clients for
	// https:// agent endpoints (nil: https endpoints are refused). Neither has an
	// overall timeout, so a streamed exec is not cut (see agent_client.go).
	Client *http.Client
	Agents *AgentDialer
	// BufferedExecTimeout bounds a whole buffered call to an agent, body included:
	// exec without ?stream=1, and exec/stdin. Zero means no limit.
	BufferedExecTimeout time.Duration
	// Sched mirrors the store's placement config, for the node view.
	Sched sched.Config

	// fencedOutage remembers, per node, the last sign of life of the outage it
	// was fenced for, so the monitor fences once per outage.
	fenceMu      sync.Mutex
	fencedOutage map[string]time.Time
}

func NewServer(s store.Store) *Server {
	return &Server{
		Store:               s,
		Client:              newAgentHTTPClient(defaultAgentTimeouts()),
		BufferedExecTimeout: 30 * time.Second,
		Sched:               sched.DefaultConfig(),
	}
}

type listSandboxesResponse struct {
	Sandboxes []store.Sandbox `json:"sandboxes"`
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
	// PTY asks the guest pod-daemon to run the command under a pseudoterminal.
	PTY bool `json:"pty,omitempty"`
	// Rows and Cols are the initial PTY window. Zero lets the guest pick 24x80.
	Rows int `json:"rows,omitempty"`
	Cols int `json:"cols,omitempty"`
	// Stdin is one-shot input for the buffered JSON exec.
	Stdin string `json:"stdin,omitempty"`
	// StdinStream keeps a guest pipe open on the non-PTY stream path.
	StdinStream bool `json:"stdin_stream,omitempty"`
}

type execStdinRequest struct {
	ExecID string `json:"exec_id"`
	Data   string `json:"data,omitempty"`
	Close  bool   `json:"close,omitempty"`
	Rows   int    `json:"rows,omitempty"`
	Cols   int    `json:"cols,omitempty"`
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
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	keys, err := jsonObjectKeys(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if k := forbiddenLocalNetKey(keys); k != "" {
		writeError(w, http.StatusBadRequest, "local_net does not accept "+k+" in v1")
		return
	}
	if guestCaller(r) {
		if _, ok := keys["local_net"]; ok {
			writeError(w, http.StatusForbidden, "guest cannot set local_net")
			return
		}
	}
	var input store.CreateSandboxInput
	if err := json.Unmarshal(body, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	input.TenantID = strings.TrimSpace(input.TenantID)
	if input.TenantID == "" {
		input.TenantID = createTenant(r)
	}
	if !authorizeTenant(w, r, input.TenantID) {
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
		if writePlacementError(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sb)
}

// placementErrorResponse explains a refused placement; reasons counts why each
// node was skipped (e.g. {"insufficient_memory": 2, "cordoned": 1}).
type placementErrorResponse struct {
	Error   string         `json:"error"`
	Reasons map[string]int `json:"reasons,omitempty"`
}

// writePlacementError maps scheduler refusals: no room → 503 with Retry-After
// (capacity frees as sandboxes stop), a pinned node that cannot run sandboxes → 409.
func writePlacementError(w http.ResponseWriter, err error) bool {
	var nc *store.NoCapacityError
	if errors.As(err, &nc) {
		reasons := make(map[string]int, len(nc.Reasons))
		for r, n := range nc.Reasons {
			reasons[string(r)] = n
		}
		w.Header().Set("Retry-After", "30")
		writeJSON(w, http.StatusServiceUnavailable, placementErrorResponse{Error: err.Error(), Reasons: reasons})
		return true
	}
	if errors.Is(err, store.ErrNodeUnavailable) {
		writeError(w, http.StatusConflict, err.Error())
		return true
	}
	return false
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
	if !sandboxVisible(w, r, sb) {
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
	if own, ok := CallerTenant(r.Context()); ok {
		// A caller confined to a tenant lists that tenant, whatever it asks.
		if tenantID != "" && !authorizeTenant(w, r, tenantID) {
			return
		}
		tenantID = own
	}
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
	if !sandboxVisible(w, r, sb) {
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
	auth, ok := enrollAuth(w, r)
	if !ok {
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
	if err := ValidateAgentEndpoint(effectiveAgentEndpoint(input.AgentEndpoint, input.Endpoint), s.allowInsecureAgentHTTP()); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	nodeID := strings.TrimSpace(input.ID)
	if nodeID == "" {
		nodeID = strings.TrimSpace(input.Name)
	}
	if nodeID == "" {
		// The certificate's CN needs the id before the store sees the node.
		nodeID = newNodeIDFallback()
	}
	input.ID = nodeID
	if err := pki.ValidNodeID(nodeID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Refuse before issuing a certificate. EnrollNode checks again atomically;
	// if it refuses after all, the certificate is discarded and its private
	// key never leaves this process.
	if err := s.Store.CheckEnroll(nodeID, auth); err != nil {
		writeEnrollError(w, nodeID, err)
		return
	}
	issued, err := s.CA.IssueNodeCert(nodeID, pki.EndpointHosts(input.AgentEndpoint, input.Endpoint), pki.DefaultNodeTTL)
	if err != nil {
		if errors.Is(err, pki.ErrInvalidNodeID) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "issue cert: "+err.Error())
		return
	}
	node, err := s.Store.EnrollNode(input, store.CertMeta{
		Fingerprint: issued.Fingerprint,
		Serial:      issued.Serial,
		NotAfter:    issued.NotAfter,
	}, auth)
	if err != nil {
		writeEnrollError(w, nodeID, err)
		return
	}
	slog.Info("node enrolled", "node_id", node.ID, "auth", enrollAuthLabel(auth), "cert_fingerprint", issued.Fingerprint)
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

// RotateNodeCert issues a replacement node certificate. It takes the node's
// own, still valid certificate over mTLS (node agents renew before expiry),
// an IdP admin, a platform-scoped API key or the node bootstrap token.
func (s *Server) RotateNodeCert(w http.ResponseWriter, r *http.Request) {
	if certID, ok := NodeIdentityFromContext(r.Context()); ok {
		// A node certificate only renews itself.
		if certID != strings.TrimSpace(r.PathValue("id")) {
			writeError(w, http.StatusForbidden, "node "+certID+" cannot rotate the certificate of node "+r.PathValue("id"))
			return
		}
	} else if !checkBootstrapToken(r) && !authorizeNodeAdmin(w, r, "rotate node certificates",
		"an idp admin token, a platform api key, the node bootstrap token or the node's own certificate (mTLS) is required to rotate node certificates") {
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
	existing, err := s.Store.GetNode(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "node not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	issued, err := s.CA.IssueNodeCert(id, pki.EndpointHosts(existing.AgentEndpoint, existing.Endpoint), pki.DefaultNodeTTL)
	if err != nil {
		if errors.Is(err, pki.ErrInvalidNodeID) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "issue cert: "+err.Error())
		return
	}
	node, err := s.Store.RotateNodeCert(id, store.CertMeta{
		Fingerprint: issued.Fingerprint,
		Serial:      issued.Serial,
		NotAfter:    issued.NotAfter,
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

// RevokeNode marks a node and its current client cert as revoked. It takes an
// IdP admin or a platform-scoped API key; the bootstrap token every node holds
// is not enough.
func (s *Server) RevokeNode(w http.ResponseWriter, r *http.Request) {
	if !authorizeNodeAdmin(w, r, "revoke nodes", "") {
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

// RegisterNode records a node-agent registration.
func (s *Server) RegisterNode(w http.ResponseWriter, r *http.Request) {
	var input store.RegisterNodeInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// With mTLS a node registers only itself; otherwise it could re-point another
	// node's agent_endpoint and receive that node's exec traffic.
	if _, ok := NodeIdentityFromContext(r.Context()); ok {
		input.ID = actingNodeID(r, input.ID)
		if !authorizeNodeID(w, r, input.ID) {
			return
		}
	}
	if err := ValidateAgentEndpoint(effectiveAgentEndpoint(input.AgentEndpoint, input.Endpoint), s.allowInsecureAgentHTTP()); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	node, err := s.Store.RegisterNode(input)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, err.Error())
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
	if !authorizeNodeID(w, r, id) {
		return
	}
	node, err := s.Store.HeartbeatNode(id)
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
	writeJSON(w, http.StatusOK, node)
}

// Exec authorizes and proxies an execution request to the sandbox's node-agent.
func (s *Server) Exec(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "sandbox id required")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	keys, err := jsonObjectKeys(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if _, ok := keys["local_net"]; ok || guestCaller(r) && forbiddenLocalNetKey(keys) != "" {
		writeError(w, http.StatusForbidden, "guest cannot set local_net")
		return
	}
	if k := forbiddenLocalNetKey(keys); k != "" {
		writeError(w, http.StatusForbidden, "guest cannot set local_net")
		return
	}
	var req execRequest
	if err := json.Unmarshal(body, &req); err != nil {
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
	if !sandboxVisible(w, r, sb) {
		return
	}
	if p, ok := IdPPrincipalFromContext(r.Context()); ok {
		if !canExec(p, sb) {
			forbid(w, "forbidden: exec requires owner, admin, or operator")
			return
		}
	}
	if msg := execBlock(sb); msg != "" {
		writeError(w, http.StatusConflict, msg)
		return
	}
	agentURL, client, status, msg := s.agentTarget(sb)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	egressPol := s.effectiveEgress(sb.TenantID)
	payload, _ := json.Marshal(map[string]any{
		"sandbox_id":       sb.ID,
		"cmd":              req.Cmd,
		"env":              req.Env,
		"cwd":              req.Cwd,
		"pty":              req.PTY,
		"rows":             req.Rows,
		"cols":             req.Cols,
		"stdin":            req.Stdin,
		"stdin_stream":     req.StdinStream,
		"egress_allowlist": egressPol,
	})
	stream := wantsExecStream(r)
	url := agentURL + "/v1/internal/exec"
	if stream {
		url += "?stream=1"
	}
	ctx, cancel := s.agentCallContext(r, stream)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if stream {
		httpReq.Header.Set("Accept", "application/x-ndjson")
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "node-agent unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		writeAgentError(w, resp.StatusCode, body)
		return
	}
	if stream && isNDJSON(resp.Header.Get("Content-Type")) {
		if err := proxyExecStream(w, resp.Body); err != nil {
			// Headers may already be flushed. Do not touch activity: the proxy did not finish.
			return
		}
		_ = s.Store.EmitEvent(store.EmitEventInput{
			SandboxID: sb.ID,
			TenantID:  sb.TenantID,
			EventType: "sandbox.exec",
			Actor:     "api",
			ActorSub:  actorSub,
			Payload:   mustJSON(map[string]any{"argc": len(req.Cmd), "stream": true}),
		})
		_ = s.Store.TouchSandboxActivity(sb.ID)
		return
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadGateway, "read node-agent exec response: "+err.Error())
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
	// Successful exec is activity (including non-zero guest exit). Proxy failures return above.
	// Streaming exec counts only after the NDJSON body is copied (see above).
	_ = s.Store.TouchSandboxActivity(sb.ID)
	if stream {
		// Upstream spoke buffered JSON (old node-agent). Re-emit one NDJSON burst
		// so clients that asked for a stream still see the same event shape.
		writeExecNDJSON(w, out)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ExecStdin proxies keystrokes or a pipe close to a streaming exec that
// already returned {"type":"ready","exec_id":...}. It does not start a command.
// A successful post refreshes idle activity so a live PTY is not reaped between
// commands that have not exited yet.
func (s *Server) ExecStdin(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "sandbox id required")
		return
	}
	var req execStdinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.ExecID) == "" {
		writeError(w, http.StatusBadRequest, "exec_id required")
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
	if !sandboxVisible(w, r, sb) {
		return
	}
	if p, ok := IdPPrincipalFromContext(r.Context()); ok {
		if !canExec(p, sb) {
			forbid(w, "forbidden: exec requires owner, admin, or operator")
			return
		}
	}
	if msg := execBlock(sb); msg != "" {
		writeError(w, http.StatusConflict, msg)
		return
	}
	agentURL, client, status, msg := s.agentTarget(sb)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"sandbox_id": sb.ID,
		"exec_id":    req.ExecID,
		"data":       req.Data,
		"close":      req.Close,
		"rows":       req.Rows,
		"cols":       req.Cols,
	})
	ctx, cancel := s.agentCallContext(r, false)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, agentURL+"/v1/internal/exec/stdin", bytes.NewReader(payload))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "node-agent unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		writeAgentError(w, resp.StatusCode, body)
		return
	}
	_ = s.Store.TouchSandboxActivity(sb.ID)
	w.Header().Set("Content-Type", "application/json")
	if len(bytes.TrimSpace(body)) == 0 {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// writeAgentError reports a non-2xx answer from the node agent. 404 means the
// agent has no guest for this sandbox (the sandbox is not running there): a
// conflict with the sandbox's state for the caller, not a gateway failure.
func writeAgentError(w http.ResponseWriter, status int, body []byte) {
	msg := strings.TrimSpace(string(body))
	if status == http.StatusNotFound {
		var ae errorResponse
		if json.Unmarshal(body, &ae) == nil && ae.Error != "" {
			msg = ae.Error
		}
		writeError(w, http.StatusConflict, "sandbox is not running on its node: "+msg)
		return
	}
	writeError(w, http.StatusBadGateway, fmt.Sprintf("node-agent status %d: %s", status, msg))
}

// agentCallContext is the context of a call to a sandbox's agent. It derives from
// the caller's request, so a disconnect cancels the call. A buffered call is also
// bounded by BufferedExecTimeout; a stream is not, it lasts as long as the command.
func (s *Server) agentCallContext(r *http.Request, stream bool) (context.Context, context.CancelFunc) {
	if stream || s.BufferedExecTimeout <= 0 {
		return r.Context(), func() {}
	}
	return context.WithTimeout(r.Context(), s.BufferedExecTimeout)
}

// wantsExecStream is true when the client asked for NDJSON chunks on the
// existing POST /exec (query stream=1 or Accept: application/x-ndjson).
// Omitting both keeps the buffered JSON body used by smokes.
func wantsExecStream(r *http.Request) bool {
	switch strings.TrimSpace(strings.ToLower(r.URL.Query().Get("stream"))) {
	case "1", "true", "yes":
		return true
	}
	accept := strings.ToLower(r.Header.Get("Accept"))
	return strings.Contains(accept, "application/x-ndjson")
}

func isNDJSON(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.Contains(ct, "application/x-ndjson") || strings.Contains(ct, "ndjson")
}

func proxyExecStream(w http.ResponseWriter, body io.Reader) error {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush() // the caller sees the stream start before the first output
	}
	buf := make([]byte, 4096)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func writeExecNDJSON(w http.ResponseWriter, out execResponse) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	if out.Stdout != "" {
		_ = enc.Encode(map[string]any{"type": "stdout", "data": out.Stdout})
		if flusher != nil {
			flusher.Flush()
		}
	}
	if out.Stderr != "" {
		_ = enc.Encode(map[string]any{"type": "stderr", "data": out.Stderr})
		if flusher != nil {
			flusher.Flush()
		}
	}
	_ = enc.Encode(map[string]any{"type": "exit", "exit_code": out.ExitCode})
	if flusher != nil {
		flusher.Flush()
	}
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
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
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
	if !authorizeTenant(w, r, id) {
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
	if !authorizeTenant(w, r, id) {
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
	if !authorizeTenant(w, r, id) {
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
	// UserSub / Act are accepted only to be ignored: guest/node must never set human claims.
	UserSub string `json:"user_sub,omitempty"`
	Act     any    `json:"act,omitempty"`
}

// MintOIDCToken issues a short-lived JWT; tenant/sub/user_sub come from the store
// (node/guest cannot override). ADR-0007 phase 5: user_sub/act from sandbox.owner_sub.
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
	// Explicitly discard guest/node-supplied human claims (never trusted).
	_ = req.UserSub
	_ = req.Act

	sb, err := s.Store.GetSandbox(req.SandboxID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !authorizeSandboxNode(w, r, sb) {
		return
	}
	if !sandboxVisible(w, r, sb) {
		return
	}
	attClaim := s.attestationClaim(sb.ID)
	userSub := strings.TrimSpace(sb.OwnerSub) // authoritative; empty OK in lab
	token, claims, err := s.OIDC.MintWithAttestation(sb.TenantID, sb.ID, req.Aud, req.Nonce, userSub, attClaim)
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
	if claims.UserSub != "" {
		outClaims["user_sub"] = claims.UserSub
	}
	if claims.Act != nil {
		outClaims["act"] = claims.Act
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
	// Sandboxes need the node's action (claim, start, stop, local-net).
	Sandboxes []store.Sandbox `json:"sandboxes"`
	// Assigned lists every sandbox placed on the node that still holds it; the
	// node stops any VM it runs that is not listed (failed over, destroyed).
	Assigned []string `json:"assigned"`
}

// ListNodeWork returns the sandboxes this node should act on and the set of
// sandboxes assigned to it.
func (s *Server) ListNodeWork(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "node id required")
		return
	}
	if !authorizeNodeID(w, r, id) {
		return
	}
	// Polling for work is a liveness signal (the heartbeat is only every 30s).
	if err := s.Store.TouchNodePoll(id, time.Now().UTC()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "node not registered")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	work, err := s.Store.ListNodeWork(id)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if work.Sandboxes == nil {
		work.Sandboxes = []store.Sandbox{}
	}
	if work.Assigned == nil {
		work.Assigned = []string{}
	}
	writeJSON(w, http.StatusOK, listWorkResponse{Sandboxes: work.Sandboxes, Assigned: work.Assigned})
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
	nodeID := actingNodeID(r, req.NodeID)
	if !authorizeNodeID(w, r, nodeID) {
		return
	}
	sb, err := s.Store.ClaimSandbox(id, nodeID)
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

// RenewSandboxLease is gone: the work poll's assigned set replaces lease
// renewals (a node stops the VMs the control plane no longer assigns to it).
// It answers 410 for one release, so an old node agent says why in its log.
func (s *Server) RenewSandboxLease(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusGone, "lease renewal was removed: GET /v1/nodes/{id}/work lists the sandboxes assigned to the node; update the node agent")
}

// UpdateSandboxStatus records agent-observed lifecycle state.
func (s *Server) UpdateSandboxStatus(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "sandbox id required")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	keys, err := jsonObjectKeys(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if _, ok := keys["local_net"]; ok || forbiddenLocalNetKey(keys) != "" {
		writeError(w, http.StatusForbidden, "guest cannot set local_net")
		return
	}
	var req statusRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !s.authorizeSandboxNodeByID(w, r, id) {
		return
	}
	state := store.SandboxState(strings.TrimSpace(req.State))
	sb, err := s.Store.UpdateSandboxStatus(id, state, strings.TrimSpace(req.Detail))
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
	if !sandboxVisible(w, r, existing) {
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
