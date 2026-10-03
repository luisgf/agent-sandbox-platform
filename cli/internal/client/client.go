// Package client is a thin HTTP client for the ASP control-plane sandbox API.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// Sandbox mirrors control-plane store.Sandbox JSON (subset used by the CLI).
type Sandbox struct {
	ID             string  `json:"id"`
	TenantID       string  `json:"tenant_id"`
	NodeID         *string `json:"node_id"`
	State          string  `json:"state"`
	ImageRef       string  `json:"image_ref"`
	CPUMillis      int     `json:"cpu_millis"`
	MemoryMiB      int     `json:"memory_mib"`
	VMMProfile     string  `json:"vmm_profile"`
	LastActivityAt string  `json:"last_activity_at,omitempty"`
	// StopReason is "idle_timeout" when the control-plane reaper stopped this sandbox.
	StopReason string `json:"stop_reason,omitempty"`
	// WorkspaceHostPath is the host directory stored on the sandbox spec, if any.
	WorkspaceHostPath string `json:"workspace_host_path,omitempty"`
	LocalNet          bool   `json:"local_net"`
	LocalNetState     string `json:"local_net_state"`
}

// StopReasonIdle matches control-plane store.StopReasonIdle.
const StopReasonIdle = "idle_timeout"

// IdleReaped reports that the control plane stopped this sandbox for inactivity.
func (s Sandbox) IdleReaped() bool {
	return s.StopReason == StopReasonIdle
}

// CreateInput is POST /v1/sandboxes body.
type CreateInput struct {
	TenantID   string `json:"tenant_id"`
	ImageRef   string `json:"image_ref"`
	CPUMillis  int    `json:"cpu_millis"`
	MemoryMiB  int    `json:"memory_mib"`
	VMMProfile string `json:"vmm_profile,omitempty"`
	NodeID     string `json:"node_id,omitempty"`
	// WorkspaceHostPath is an absolute host directory to record on the sandbox spec.
	WorkspaceHostPath string `json:"workspace_host_path,omitempty"`
	// LocalNet opts into the full-tunnel default route. Nil omits the field (off).
	LocalNet *bool `json:"local_net,omitempty"`
}

// ExecRequest is POST /v1/sandboxes/{id}/exec body.
type ExecRequest struct {
	Cmd []string          `json:"cmd"`
	Env map[string]string `json:"env,omitempty"`
	Cwd string            `json:"cwd,omitempty"`
	// PTY asks the guest to allocate a pseudoterminal. Streaming only.
	PTY bool `json:"pty,omitempty"`
	// Rows and Cols are the initial PTY size (0 = guest default 24x80).
	Rows int `json:"rows,omitempty"`
	Cols int `json:"cols,omitempty"`
	// Stdin is one-shot input for the buffered JSON exec.
	Stdin string `json:"stdin,omitempty"`
	// StdinStream keeps a pipe open when PTY is false so piped stdin can EOF.
	StdinStream bool `json:"stdin_stream,omitempty"`
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
// The response is buffered JSON (stdout/stderr/exit_code). Smokes use this path.
func (c *Client) Exec(ctx context.Context, id string, req ExecRequest) (ExecResult, error) {
	var out ExecResult
	err := c.doJSON(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/exec", req, http.StatusOK, &out)
	return out, err
}

// streamEvent is one NDJSON line from POST /exec?stream=1.
type streamEvent struct {
	Type     string `json:"type"`
	Stream   string `json:"stream"`
	Data     string `json:"data"`
	ExitCode *int   `json:"exit_code"`
	ExecID   string `json:"exec_id"`
}

func (e streamEvent) kind() string {
	if strings.TrimSpace(e.Type) != "" {
		return e.Type
	}
	return e.Stream
}

// ExecStream POSTs ?stream=1 and writes stdout/stderr as NDJSON events arrive.
// It returns the guest exit code. A buffered JSON response (old control plane)
// is still accepted and written only after the body is complete.
func (c *Client) ExecStream(ctx context.Context, id string, req ExecRequest, stdout, stderr io.Writer) (int, error) {
	return c.ExecStreamIO(ctx, id, req, stdout, stderr, nil)
}

// ExecStreamIO is ExecStream plus an optional stdin reader. When stdin is
// non-nil the guest must emit {"type":"ready","exec_id":"..."} (PTY or
// stdin_stream). Bytes are posted to /exec/stdin. EOF sends close:true.
// A *os.File stdin is polled so restoring the terminal does not block the
// process after the guest exits. The stream request itself has no client
// timeout; each stdin POST uses the client's normal timeout.
func (c *Client) ExecStreamIO(ctx context.Context, id string, req ExecRequest, stdout, stderr io.Writer, stdin io.Reader) (int, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return 0, err
	}
	path := "/v1/sandboxes/" + url.PathEscape(id) + "/exec?stream=1"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/x-ndjson")
	if ah := c.authHeader(); ah != "" {
		httpReq.Header.Set("Authorization", ah)
	}
	base := c.HTTPClient
	if base == nil {
		base = http.DefaultClient
	}
	hc := *base
	// A PTY or a long stream must not die at the JSON client's 60s cap.
	hc.Timeout = 0
	resp, err := hc.Do(httpReq)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var ae apiError
		_ = json.Unmarshal(raw, &ae)
		msg := strings.TrimSpace(ae.Error)
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		if msg == "" {
			msg = resp.Status
		}
		return 0, &HTTPError{StatusCode: resp.StatusCode, Message: msg}
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "application/json") && !strings.Contains(ct, "ndjson") {
		var out ExecResult
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return 0, fmt.Errorf("decode exec response: %w", err)
		}
		if stdout != nil && out.Stdout != "" {
			if _, err := io.WriteString(stdout, out.Stdout); err != nil {
				return 0, err
			}
		}
		if stderr != nil && out.Stderr != "" {
			if _, err := io.WriteString(stderr, out.Stderr); err != nil {
				return 0, err
			}
		}
		return out.ExitCode, nil
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	sawExit := false
	exitCode := 0
	var pumpStop chan struct{}
	var pumpErr chan error
	if stdin != nil {
		pumpStop = make(chan struct{})
		pumpErr = make(chan error, 1)
	}
	defer func() {
		if pumpStop != nil {
			close(pumpStop)
		}
	}()
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return 0, fmt.Errorf("exec stream: %w", err)
		}
		switch ev.kind() {
		case "ready":
			if stdin != nil && ev.ExecID != "" && pumpErr != nil {
				execID := ev.ExecID
				ch := pumpErr
				go func() {
					ch <- c.pumpExecStdin(ctx, id, execID, stdin, pumpStop)
				}()
				pumpErr = nil // single start; ch keeps the buffered channel
			}
		case "stdout":
			if stdout != nil && ev.Data != "" {
				if _, err := io.WriteString(stdout, ev.Data); err != nil {
					return 0, err
				}
			}
		case "stderr":
			if stderr != nil && ev.Data != "" {
				if _, err := io.WriteString(stderr, ev.Data); err != nil {
					return 0, err
				}
			}
		case "exit":
			if ev.ExitCode == nil {
				return 0, fmt.Errorf("exec stream: exit event without exit_code")
			}
			exitCode = *ev.ExitCode
			sawExit = true
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	if !sawExit {
		return 0, fmt.Errorf("exec stream: missing exit event")
	}
	return exitCode, nil
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

func (c *Client) pumpExecStdin(ctx context.Context, sandboxID, execID string, stdin io.Reader, stop <-chan struct{}) error {
	send := func(data string, closeStdin bool) error {
		body := map[string]any{"exec_id": execID}
		if data != "" {
			body["data"] = data
		}
		if closeStdin {
			body["close"] = true
		}
		path := "/v1/sandboxes/" + url.PathEscape(sandboxID) + "/exec/stdin"
		return c.doJSON(ctx, http.MethodPost, path, body, http.StatusOK, nil)
	}
	buf := make([]byte, 1024)
	for {
		if stop != nil {
			select {
			case <-stop:
				return nil
			default:
			}
		}
		n, err := readStdin(stdin, buf, stop)
		if n > 0 {
			if serr := send(string(buf[:n]), false); serr != nil {
				return serr
			}
		}
		if err == io.EOF {
			return send("", true)
		}
		if err == errStdinStopped {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// readStdin reads one chunk. *os.File is non-blocking so stop unblocks a TTY.
// A nil error and n==0 means "try again" (EAGAIN). io.EOF means the writer closed.
func readStdin(stdin io.Reader, buf []byte, stop <-chan struct{}) (int, error) {
	f, ok := stdin.(*os.File)
	if !ok {
		return stdin.Read(buf)
	}
	fd := int(f.Fd())
	if err := syscall.SetNonblock(fd, true); err != nil {
		return stdin.Read(buf)
	}
	defer syscall.SetNonblock(fd, false)
	for {
		if stop != nil {
			select {
			case <-stop:
				return 0, errStdinStopped
			default:
			}
		}
		n, err := syscall.Read(fd, buf)
		if n > 0 {
			return n, nil
		}
		if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		if err != nil {
			return 0, err
		}
		return 0, io.EOF
	}
}

var errStdinStopped = fmt.Errorf("stdin stopped")

// LocalNetGrant is the one-shot tunnel grant. Do not write it into the session file.
type LocalNetGrant struct {
	Grant            string `json:"grant"`
	Dial             string `json:"dial"`
	ExpiresAt        string `json:"expires_at"`
	Iface            string `json:"tunnel_iface"`
	Transport        string `json:"transport"`
	NodePublicKey    string `json:"node_public_key"`
	ListenPort       int    `json:"listen_port"`
	NodeTunnelAddr   string `json:"node_tunnel_addr"`
	ClientTunnelAddr string `json:"client_tunnel_addr"`
}

// IssueLocalNetGrant asks the control plane for a short-lived grant.
func (c *Client) IssueLocalNetGrant(ctx context.Context, id string) (LocalNetGrant, error) {
	var out LocalNetGrant
	err := c.doJSON(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/local-net/grant", map[string]any{}, http.StatusOK, &out)
	return out, err
}

// HeartbeatLocalNet marks the tunnel up. clientPublic is a WireGuard public key.
func (c *Client) HeartbeatLocalNet(ctx context.Context, id, grant, clientPublic string) (Sandbox, error) {
	var out Sandbox
	body := map[string]string{"grant": grant, "client_public_key": clientPublic}
	err := c.doJSON(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/local-net/heartbeat", body, http.StatusOK, &out)
	return out, err
}

// DetachLocalNet withdraws the tunnel. The control plane must not restore public egress.
func (c *Client) DetachLocalNet(ctx context.Context, id string) (Sandbox, error) {
	var out Sandbox
	err := c.doJSON(ctx, http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(id)+"/local-net/attach", nil, http.StatusOK, &out)
	return out, err
}
