// Package client is a thin HTTP client for the ASP control-plane sandbox API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Sandbox mirrors control-plane store.Sandbox JSON (subset used by the CLI).
type Sandbox struct {
	ID         string  `json:"id"`
	TenantID   string  `json:"tenant_id"`
	NodeID     *string `json:"node_id"`
	State      string  `json:"state"`
	ImageRef   string  `json:"image_ref"`
	CPUMillis  int     `json:"cpu_millis"`
	MemoryMiB  int     `json:"memory_mib"`
	VMMProfile string  `json:"vmm_profile"`
}

// CreateInput is POST /v1/sandboxes body.
type CreateInput struct {
	TenantID   string `json:"tenant_id"`
	ImageRef   string `json:"image_ref"`
	CPUMillis  int    `json:"cpu_millis"`
	MemoryMiB  int    `json:"memory_mib"`
	VMMProfile string `json:"vmm_profile,omitempty"`
	NodeID     string `json:"node_id,omitempty"`
}

// ExecRequest is POST /v1/sandboxes/{id}/exec body.
type ExecRequest struct {
	Cmd []string          `json:"cmd"`
	Env map[string]string `json:"env,omitempty"`
	Cwd string            `json:"cwd,omitempty"`
}

// ExecResult is the exec response from the control plane.
type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

type listSandboxesResponse struct {
	Sandboxes []Sandbox `json:"sandboxes"`
}

type apiError struct {
	Error string `json:"error"`
}

// Client talks to the control-plane HTTP API.
type Client struct {
	BaseURL    string
	APIKey     string // legacy / service key; used if Bearer empty
	Bearer     string // IdP access token (preferred Authorization value)
	HTTPClient *http.Client
}

// New returns a Client with a sensible default timeout.
// apiKey is used as Authorization Bearer when no IdP token is set via SetBearer.
func New(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:  strings.TrimSpace(apiKey),
		HTTPClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// SetBearer sets the preferred Authorization Bearer (IdP JWT). Empty clears it.
func (c *Client) SetBearer(token string) {
	c.Bearer = strings.TrimSpace(token)
}

// authHeader returns the value for Authorization, if any.
func (c *Client) authHeader() string {
	if t := strings.TrimSpace(c.Bearer); t != "" {
		return "Bearer " + t
	}
	if k := strings.TrimSpace(c.APIKey); k != "" {
		return "Bearer " + k
	}
	return ""
}

// CreateSandbox POSTs a new sandbox.
func (c *Client) CreateSandbox(ctx context.Context, in CreateInput) (Sandbox, error) {
	var out Sandbox
	err := c.doJSON(ctx, http.MethodPost, "/v1/sandboxes", in, http.StatusCreated, &out)
	return out, err
}

// GetSandbox GETs one sandbox by id.
func (c *Client) GetSandbox(ctx context.Context, id string) (Sandbox, error) {
	var out Sandbox
	err := c.doJSON(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(id), nil, http.StatusOK, &out)
	return out, err
}

// ListSandboxes GETs sandboxes, optionally filtered by tenant.
func (c *Client) ListSandboxes(ctx context.Context, tenantID string) ([]Sandbox, error) {
	path := "/v1/sandboxes"
	if t := strings.TrimSpace(tenantID); t != "" {
		path += "?tenant_id=" + url.QueryEscape(t)
	}
	var out listSandboxesResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, http.StatusOK, &out); err != nil {
		return nil, err
	}
	if out.Sandboxes == nil {
		return []Sandbox{}, nil
	}
	return out.Sandboxes, nil
}

// DeleteSandbox DELETEs (marks stopping/stopped) a sandbox.
func (c *Client) DeleteSandbox(ctx context.Context, id string) (Sandbox, error) {
	var out Sandbox
	err := c.doJSON(ctx, http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(id), nil, http.StatusOK, &out)
	return out, err
}

// Exec runs a command in the sandbox via the control-plane proxy.
func (c *Client) Exec(ctx context.Context, id string, req ExecRequest) (ExecResult, error) {
	var out ExecResult
	err := c.doJSON(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/exec", req, http.StatusOK, &out)
	return out, err
}

func (c *Client) doJSON(ctx context.Context, method, path string, body any, wantStatus int, dest any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ah := c.authHeader(); ah != "" {
		req.Header.Set("Authorization", ah)
	}
	hc := c.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != wantStatus {
		var ae apiError
		_ = json.Unmarshal(raw, &ae)
		msg := strings.TrimSpace(ae.Error)
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		if msg == "" {
			msg = resp.Status
		}
		return &HTTPError{StatusCode: resp.StatusCode, Message: msg}
	}
	if dest == nil {
		return nil
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// HTTPError is a non-2xx control-plane response.
type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("control-plane HTTP %d: %s", e.StatusCode, e.Message)
}
