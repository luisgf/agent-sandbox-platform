// Package execproxy exposes a localhost HTTP server that proxies exec to pod-daemon.
package execproxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/egress"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/poddaemon"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/sshagent"
)

// Server is the node-agent internal callback API for the control plane.
type Server struct {
	Pod           *poddaemon.Client // optional fallback when Registry has no entry
	Registry      *poddaemon.Registry
	Logger        *slog.Logger
	EgressEnforce bool
	// DefaultAllowlist used when request omits egress_allowlist (and for egress-check).
	DefaultAllowlist *egress.Allowlist
	// PolicyCache stores each sandbox's allowlist from its exec for the forward proxy and DNS sink.
	PolicyCache *egress.PolicyCache
	// SSHApprover optional one-shot SignRequest approvals (POST /v1/internal/ssh-agent/approve).
	SSHApprover *sshagent.Approver
	// Doctor runs the node's self-checks (GET /v1/internal/doctor). Nil answers 501.
	Doctor func(ctx context.Context) any
}

type egressRuleDTO struct {
	HostPattern string `json:"host_pattern"`
	Port        *int   `json:"port,omitempty"`
	Enabled     bool   `json:"enabled"`
}

type egressPolicyDTO struct {
	TenantID string          `json:"tenant_id"`
	Mode     string          `json:"mode"`
	Rules    []egressRuleDTO `json:"rules"`
}

type execBody struct {
	SandboxID       string            `json:"sandbox_id"`
	Cmd             []string          `json:"cmd"`
	Env             map[string]string `json:"env,omitempty"`
	Cwd             string            `json:"cwd,omitempty"`
	PTY             bool              `json:"pty,omitempty"`
	Rows            int               `json:"rows,omitempty"`
	Cols            int               `json:"cols,omitempty"`
	Stdin           string            `json:"stdin,omitempty"`
	StdinStream     bool              `json:"stdin_stream,omitempty"`
	AsRoot          bool              `json:"as_root,omitempty"`
	TimeoutSeconds  int               `json:"timeout_seconds,omitempty"`
	EgressAllowlist *egressPolicyDTO  `json:"egress_allowlist,omitempty"`
}

type stdinBody struct {
	SandboxID string `json:"sandbox_id"`
	ExecID    string `json:"exec_id"`
	Data      string `json:"data,omitempty"`
	Close     bool   `json:"close,omitempty"`
	Rows      int    `json:"rows,omitempty"`
	Cols      int    `json:"cols,omitempty"`
}

type egressCheckBody struct {
	Host            string           `json:"host"`
	Port            int              `json:"port,omitempty"`
	EgressAllowlist *egressPolicyDTO `json:"egress_allowlist,omitempty"`
	// SandboxID evaluates the policy the proxy applies to that sandbox now.
	SandboxID string `json:"sandbox_id,omitempty"`
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	mux.HandleFunc("POST /v1/internal/exec", s.handleExec)
	mux.HandleFunc("POST /v1/internal/exec/stdin", s.handleExecStdin)
	mux.HandleFunc("GET /v1/internal/doctor", s.handleDoctor)
	mux.HandleFunc("POST /v1/internal/egress-check", s.handleEgressCheck)
	mux.HandleFunc("POST /v1/internal/ssh-agent/approve", s.handleSSHAgentApprove)
	return mux
}

func (s *Server) clientFor(sandboxID string) (*poddaemon.Client, error) {
	if s.Registry != nil {
		c, err := s.Registry.ClientFor(sandboxID)
		if err == nil {
			return c, nil
		}
		// Fall through to Pod if registry miss and fallback client exists.
		if s.Pod == nil {
			return nil, err
		}
	}
	if s.Pod != nil {
		return s.Pod, nil
	}
	return nil, errPodUnavailable
}

var errPodUnavailable = &podErr{msg: "pod-daemon client not configured"}

// writeNoClient answers a request for a sandbox without a pod-daemon client:
// 404 when the sandbox is not running on this node (the control plane turns
// it into 409), 503 when exec is not configured at all.
func writeNoClient(w http.ResponseWriter, err error) {
	if errors.Is(err, poddaemon.ErrUnknownSandbox) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	msg := "pod-daemon client not configured"
	if err != nil {
		msg = err.Error()
	}
	writeErr(w, http.StatusServiceUnavailable, msg)
}

type podErr struct{ msg string }

func (e *podErr) Error() string { return e.msg }

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	var body execBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(body.Cmd) == 0 {
		writeErr(w, http.StatusBadRequest, "cmd required")
		return
	}
	client, err := s.clientFor(body.SandboxID)
	if err != nil || client == nil {
		writeNoClient(w, err)
		return
	}
	if body.EgressAllowlist != nil {
		// The control plane attaches the tenant policy to every exec; the
		// proxy applies it only to traffic from this sandbox's prefix.
		s.PolicyCache.Set(body.SandboxID, s.allowlistFromDTO(body.EgressAllowlist))
	}
	if wantsStream(r) {
		s.handleExecStream(w, r, client, body)
		return
	}
	out, err := client.Exec(r.Context(), execRequestFromBody(body))
	if err != nil {
		s.logExecError(r, "pod-daemon exec", err, "sandbox_id", body.SandboxID)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleEgressCheck(w http.ResponseWriter, r *http.Request) {
	var body egressCheckBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Host == "" {
		writeErr(w, http.StatusBadRequest, "host required")
		return
	}
	al := s.DefaultAllowlist
	switch {
	case body.EgressAllowlist != nil:
		// Evaluation only: the proxy policy is not changed.
		al = s.allowlistFromDTO(body.EgressAllowlist)
	case body.SandboxID != "":
		// What the proxy and the DNS sink apply to this sandbox's traffic: its
		// tenant's policy from the work poll, deny-default until there is one.
		al = s.PolicyCache.Get(body.SandboxID)
		if al == nil {
			al = egress.NewAllowlistFromPolicy("deny-default", nil)
		}
	}
	if al == nil {
		al = egress.NewAllowlistFromPolicy("deny-default", nil)
	}
	host, port := egress.ParseHostPort(body.Host)
	if body.Port > 0 {
		port = body.Port
	}
	err := al.CheckHostPort(host, port)
	allowed := err == nil
	status := http.StatusOK
	if s.EgressEnforce && !allowed {
		status = http.StatusForbidden
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"host":    host,
		"port":    port,
		"allowed": allowed,
		"error":   errString(err),
	})
}

func (s *Server) allowlistFromDTO(dto *egressPolicyDTO) *egress.Allowlist {
	// Effective policy from CP only includes enabled rules; still honor Enabled=false if present.
	rules := make([]egress.Rule, 0, len(dto.Rules))
	anyEnabledFlag := false
	for _, r := range dto.Rules {
		if r.Enabled {
			anyEnabledFlag = true
			break
		}
	}
	for _, r := range dto.Rules {
		if r.HostPattern == "" {
			continue
		}
		if anyEnabledFlag && !r.Enabled {
			continue
		}
		rules = append(rules, egress.Rule{HostPattern: r.HostPattern, Port: r.Port})
	}
	return egress.NewAllowlistFromPolicy(dto.Mode, rules)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func execRequestFromBody(body execBody) poddaemon.ExecRequest {
	return poddaemon.ExecRequest{
		Cmd:         body.Cmd,
		Env:         body.Env,
		Cwd:         body.Cwd,
		PTY:         body.PTY,
		Rows:        body.Rows,
		Cols:        body.Cols,
		Stdin:       body.Stdin,
		StdinStream: body.StdinStream,
		AsRoot:      body.AsRoot,
		TimeoutSecs: body.TimeoutSeconds,
	}
}

func (s *Server) handleExecStdin(w http.ResponseWriter, r *http.Request) {
	var body stdinBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(body.ExecID) == "" {
		writeErr(w, http.StatusBadRequest, "exec_id required")
		return
	}
	client, err := s.clientFor(body.SandboxID)
	if err != nil || client == nil {
		writeNoClient(w, err)
		return
	}
	if err := client.WriteStdin(r.Context(), poddaemon.StdinMessage{
		ExecID: body.ExecID,
		Data:   body.Data,
		Close:  body.Close,
		Rows:   body.Rows,
		Cols:   body.Cols,
	}); err != nil {
		s.logExecError(r, "pod-daemon exec stdin", err, "sandbox_id", body.SandboxID, "exec_id", body.ExecID)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func wantsStream(r *http.Request) bool {
	switch strings.TrimSpace(strings.ToLower(r.URL.Query().Get("stream"))) {
	case "1", "true", "yes":
		return true
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/x-ndjson")
}

func (s *Server) handleExecStream(w http.ResponseWriter, r *http.Request, client *poddaemon.Client, body execBody) {
	in := execRequestFromBody(body)
	resp, err := client.OpenExecStream(r.Context(), in)
	if err != nil {
		if errors.Is(err, poddaemon.ErrStreamUnsupported) {
			s.writeBufferedAsNDJSON(w, r, client, in, body.SandboxID)
			return
		}
		s.logExecError(r, "pod-daemon exec stream", err, "sandbox_id", body.SandboxID)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "application/json") && !strings.Contains(ct, "ndjson") {
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if rerr != nil {
			writeErr(w, http.StatusBadGateway, rerr.Error())
			return
		}
		var out poddaemon.ExecResponse
		if jerr := json.Unmarshal(raw, &out); jerr != nil {
			writeErr(w, http.StatusBadGateway, "invalid pod-daemon exec response")
			return
		}
		writeNDJSONResult(w, out)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	// Send the headers now: a command that stays silent must not look like an
	// agent that never answered to the control plane's response-header timeout.
	if flusher != nil {
		flusher.Flush()
	}
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr == io.EOF {
			return
		}
		if rerr != nil {
			s.logExecError(r, "pod-daemon exec stream copy", rerr, "sandbox_id", body.SandboxID)
			return
		}
	}
}

func (s *Server) writeBufferedAsNDJSON(w http.ResponseWriter, r *http.Request, client *poddaemon.Client, in poddaemon.ExecRequest, sandboxID string) {
	out, err := client.Exec(r.Context(), in)
	if err != nil {
		s.logExecError(r, "pod-daemon exec", err, "sandbox_id", sandboxID)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeNDJSONResult(w, out)
}

func writeNDJSONResult(w http.ResponseWriter, out poddaemon.ExecResponse) {
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

// ListenAndServe binds addr (e.g. 127.0.0.1:9100) and serves until ctx-like shutdown via returned server.
func ListenAndServe(addr string, h http.Handler) (*http.Server, net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		_ = srv.Serve(ln)
	}()
	return srv, ln, nil
}

// handleSSHAgentApprove issues a one-shot approval for the next SIGN_REQUEST
// from one sandbox (--ssh-agent-confirm). Approvals are bound to sandbox_id:
// only a sign arriving through that sandbox's host-vsock acceptor consumes
// it. Without sandbox_id → 400, unless --insecure-ssh-agent-global-approvals
// lets listeners that cannot tell guests apart use an unscoped approval.
func (s *Server) handleSSHAgentApprove(w http.ResponseWriter, r *http.Request) {
	if s.SSHApprover == nil {
		writeErr(w, http.StatusServiceUnavailable, "ssh-agent confirmation gate not enabled (--ssh-agent-confirm)")
		return
	}
	var body struct {
		TTLSeconds int    `json:"ttl_seconds"`
		ActorSub   string `json:"actor_sub"`
		SandboxID  string `json:"sandbox_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	ttl := 30 * time.Second
	if body.TTLSeconds > 0 {
		ttl = time.Duration(body.TTLSeconds) * time.Second
	}
	actor := body.ActorSub
	if actor == "" {
		actor = r.Header.Get("X-ASP-Actor-Sub")
	}
	id, exp, err := s.SSHApprover.Approve(body.SandboxID, ttl)
	if errors.Is(err, sshagent.ErrSandboxRequired) {
		writeErr(w, http.StatusBadRequest, "sandbox_id required: an approval only unlocks a sign from its sandbox (--insecure-ssh-agent-global-approvals allows unscoped approvals for listeners that cannot tell guests apart; lab only)")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log := s.Logger
	if log == nil {
		log = slog.Default()
	}
	log.Info("ssh-agent approve issued",
		"approval_id", id,
		"actor_sub", actor,
		"sandbox_id", body.SandboxID,
		"expires_at", exp.UTC().Format(time.RFC3339),
		"one_shot", true,
	)
	out := map[string]any{
		"approval_id": id,
		"expires_at":  exp.UTC().Format(time.RFC3339),
		"one_shot":    true,
	}
	if body.SandboxID != "" {
		out["sandbox_id"] = body.SandboxID
		out["note"] = "the next SignRequest from this sandbox consumes this approval; approval_id is an audit id (logged here and on the sign it unlocks), not a credential"
	} else {
		out["scope"] = "global"
		out["note"] = "the next SignRequest on a listener that cannot tell guests apart (--ssh-agent-bridge, global host-vsock) consumes this approval, whichever guest sends it; approval_id is an audit id, not a credential"
	}
	if actor != "" {
		out["actor_sub"] = actor
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// clientGone reports that err is explained by the client having left: its request
// was cancelled (a `| head`, a Ctrl-C, a timeout on its side) or its connection
// reset. That is an ordinary end of a stream, not a fault of the agent.
func clientGone(r *http.Request, err error) bool {
	return r.Context().Err() != nil || errors.Is(err, context.Canceled) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}

// logExecError logs a failure of an exec call: at error level when the agent or
// the guest failed, at info when the client simply went away.
func (s *Server) logExecError(r *http.Request, msg string, err error, attrs ...any) {
	if s.Logger == nil {
		return
	}
	args := append([]any{"error", err}, attrs...)
	if clientGone(r, err) {
		s.Logger.Info(msg+": the client went away", args...)
		return
	}
	s.Logger.Error(msg, args...)
}

// handleDoctor answers with the report of the node's self-checks. It runs the checks
// (a few seconds at most), so it is read-only on the node apart from a TAP device
// that is made and removed at once.
func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	if s.Doctor == nil {
		http.Error(w, `{"error":"this node-agent has no doctor"}`, http.StatusNotImplemented)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.Doctor(r.Context()))
}
