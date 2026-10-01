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

// Proxy forwards POST /v1/tokens/oidc to the control-plane mint endpoint.
// Guest may only supply aud (+ optional nonce); sandbox/tenant come from context.
type Proxy struct {
	ControlPlaneURL string
	HTTP            *http.Client
	// DefaultSandboxID used when request/header/env omit it (dry-run).
	DefaultSandboxID string
	Logger           *slog.Logger
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
	sandboxID := strings.TrimSpace(r.Header.Get("X-ASP-Sandbox-ID"))
	if sandboxID == "" {
		sandboxID = strings.TrimSpace(os.Getenv("ASP_SANDBOX_ID"))
	}
	if sandboxID == "" {
		sandboxID = strings.TrimSpace(p.DefaultSandboxID)
	}
	if sandboxID == "" {
		writeErr(w, http.StatusBadRequest, "sandbox context required")
		return
	}
	if req.SandboxID != "" && req.SandboxID != sandboxID && p.Logger != nil {
		p.Logger.Warn("guest supplied sandbox_id ignored", "guest", req.SandboxID, "authoritative", sandboxID)
	}
	if (req.UserSub != "" || req.Act != nil) && p.Logger != nil {
		p.Logger.Warn("guest supplied user_sub/act ignored", "guest_user_sub", req.UserSub)
	}

	token, err := p.mint(r.Context(), sandboxID, aud, req.Nonce)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(token)
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
