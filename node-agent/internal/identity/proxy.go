// Package identity exposes guest-facing OIDC token mint proxy.
package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// SandboxHeader names a sandbox in a guest request. It is never authority on
// its own: see Proxy.TrustSandboxHeader.
const SandboxHeader = "X-ASP-Sandbox-ID"

// Proxy forwards POST /v1/tokens/oidc to the control-plane mint endpoint.
// Guest may only supply aud (+ optional nonce); the sandbox comes from the
// connection (WithSandboxID) and the tenant from the control plane.
type Proxy struct {
	ControlPlaneURL string
	HTTP            *http.Client
	// TrustSandboxHeader (lab only) lets requests on connections without a
	// sandbox binding take the sandbox from SandboxHeader, then
	// DefaultSandboxID. Any client that reaches such a listener can then mint
	// any sandbox's token. Off: those requests get 403. Bound connections
	// ignore it.
	TrustSandboxHeader bool
	// DefaultSandboxID is the sandbox for unbound requests without
	// SandboxHeader when TrustSandboxHeader is set (dry-run).
	DefaultSandboxID string
	Logger           *slog.Logger
}

type sandboxKey struct{}

// WithSandboxID binds a request context to the sandbox whose guest opened the
// connection. Only host code that knows the peer may call it, such as the
// per-sandbox hybrid vsock acceptor; never derive sandboxID from the request.
func WithSandboxID(ctx context.Context, sandboxID string) context.Context {
	return context.WithValue(ctx, sandboxKey{}, sandboxID)
}

// SandboxIDFromContext returns the sandbox bound to the request's connection.
func SandboxIDFromContext(ctx context.Context) (string, bool) {
	id, _ := ctx.Value(sandboxKey{}).(string)
	return id, id != ""
}

type guestTokenRequest struct {
	Aud       string `json:"aud"`
	Nonce     string `json:"nonce,omitempty"`
	SandboxID string `json:"sandbox_id,omitempty"` // ignored for authority; logged if mismatch
	UserSub   string `json:"user_sub,omitempty"`  // ignored — CP derives from owner_sub
	Act       any    `json:"act,omitempty"`       // ignored — CP derives from owner_sub
}

type mintRequest struct {
	SandboxID string `json:"sandbox_id"`
	Aud       string `json:"aud"`
	Nonce     string `json:"nonce,omitempty"`
}

type mintResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
}

// Handler returns an HTTP mux suitable for unix or TCP listen.
func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	mux.HandleFunc("POST /v1/tokens/oidc", p.handleToken)
	return mux
}

func (p *Proxy) handleToken(w http.ResponseWriter, r *http.Request) {
	sandboxID, status, msg := p.sandboxFor(r)
	if status != 0 {
		writeErr(w, status, msg)
		return
	}
	var req guestTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	aud := strings.TrimSpace(req.Aud)
	if aud == "" {
		writeErr(w, http.StatusBadRequest, "aud required")
		return
	}
	if req.SandboxID != "" && req.SandboxID != sandboxID {
		p.warn("guest supplied sandbox_id ignored", "guest", req.SandboxID, "authoritative", sandboxID)
	}
	if req.UserSub != "" || req.Act != nil {
		p.warn("guest supplied user_sub/act ignored", "guest_user_sub", req.UserSub)
	}

	token, err := p.mint(r.Context(), sandboxID, aud, req.Nonce)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(token)
}

// sandboxFor returns the sandbox a token request acts for, or the status and
// message to refuse it with. A connection bound to a sandbox decides, and a
// request that names another sandbox is refused. Unbound connections only work
// with TrustSandboxHeader.
func (p *Proxy) sandboxFor(r *http.Request) (string, int, string) {
	if bound, ok := SandboxIDFromContext(r.Context()); ok {
		for _, v := range r.Header.Values(SandboxHeader) {
			if claimed := strings.TrimSpace(v); claimed != "" && claimed != bound {
				p.warn("guest asked for another sandbox's token; refused", "sandbox_id", bound, "claimed", claimed)
				return "", http.StatusForbidden, SandboxHeader + " does not match this connection's sandbox"
			}
		}
		return bound, 0, ""
	}
	claimed := strings.TrimSpace(r.Header.Get(SandboxHeader))
	if !p.TrustSandboxHeader {
		p.warn("token request on a connection without a sandbox binding refused",
			"claimed", claimed,
			"hint", "guests use their sandbox's hybrid vsock port 26502; labs can pass --insecure-identity-sandbox-header")
		return "", http.StatusForbidden, "connection is not bound to a sandbox"
	}
	if claimed != "" {
		return claimed, 0, ""
	}
	if id := strings.TrimSpace(p.DefaultSandboxID); id != "" {
		return id, 0, ""
	}
	return "", http.StatusBadRequest, "sandbox context required"
}

func (p *Proxy) warn(msg string, args ...any) {
	if p.Logger != nil {
		p.Logger.Warn(msg, args...)
	}
}

func (p *Proxy) mint(ctx context.Context, sandboxID, aud, nonce string) (mintResponse, error) {
	var out mintResponse
	body, _ := json.Marshal(mintRequest{SandboxID: sandboxID, Aud: aud, Nonce: nonce})
	url := strings.TrimRight(p.ControlPlaneURL, "/") + "/v1/internal/oidc/token"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	client := p.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return out, fmt.Errorf("oidc mint status %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	return out, nil
}

// ListenUnix serves the identity proxy on a unix socket.
func ListenUnix(path string, h http.Handler) (net.Listener, *http.Server, error) {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, nil, err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return ln, srv, nil
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
