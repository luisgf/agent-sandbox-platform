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
	// BootCount is how many times the sandbox has been started; above 1 it was resumed.
	BootCount int `json:"boot_count,omitempty"`
	// StoppedAt is when the node reported it stopped (RFC 3339), while it is.
	StoppedAt string `json:"stopped_at,omitempty"`
	// StatusDetail is why the last start or resume could not finish.
	StatusDetail string `json:"status_detail,omitempty"`
	// WorkspaceHostPath is the host directory stored on the sandbox spec, if any.
	WorkspaceHostPath string `json:"workspace_host_path,omitempty"`
	LocalNet          bool   `json:"local_net"`
	LocalNetState     string `json:"local_net_state"`
}

// Node mirrors the control plane's node view (GET /v1/nodes).
type Node struct {
	ID            string `json:"id"`
	State         string `json:"state"`
	AgentEndpoint string `json:"agent_endpoint,omitempty"`
	Cordoned      bool   `json:"cordoned"`
	// FenceConfigured says the control plane can power the node off if it loses
	// it; the target itself is never served.
	FenceConfigured bool       `json:"fence_configured"`
	AcceptsWork     bool       `json:"accepts_work"`
	CapacityCPU     int        `json:"capacity_cpu"`
	CapacityMemMiB  int        `json:"capacity_mem_mib"`
	MaxSandboxes    int        `json:"max_sandboxes"`
	LastSeenAt      *time.Time `json:"last_seen_at"`
	CertNotAfter    *time.Time `json:"cert_not_after,omitempty"`
	Allocated       NodeUsage  `json:"allocated"`
	Allocatable     NodeUsage  `json:"allocatable"` // 0 = not enforced
	// StoppedSandboxes counts the stopped sandboxes on the node: each keeps a
	// disk there and holds no CPU or memory.
	StoppedSandboxes int64 `json:"stopped_sandboxes"`
	// DiskFreeMiB is the free space of the node's disk directory, as it last reported it.
	DiskFreeMiB         *int64 `json:"disk_free_mib,omitempty"`
	Schedulable         bool   `json:"schedulable"`
	UnschedulableReason string `json:"unschedulable_reason,omitempty"`
}

// NodeUsage is what is placed on (or offered by) a node.
type NodeUsage struct {
	CPUMillis int64 `json:"cpu_millis"`
	MemoryMiB int64 `json:"memory_mib"`
	Sandboxes int64 `json:"sandboxes"`
}

// StopReasonIdle matches control-plane store.StopReasonIdle.
const StopReasonIdle = "idle_timeout"

// IdleReaped reports that the control plane stopped this sandbox for inactivity.
func (s Sandbox) IdleReaped() bool {
	return s.StopReason == StopReasonIdle
}

// Stop reasons for sandboxes lost with their node (control-plane store).
const (
	StopReasonNodeLost       = "node_lost"
	StopReasonAgentRestarted = "node_agent_restarted"
)

// LostWithNode reports that the sandbox's node was lost or its agent restarted
// and that it did not come out of it stopped: a failed sandbox has no disk to come
// back to. A stopped one (the VM died, the disk is kept) is not lost: it can be
// resumed, see StoppedByNodeEvent.
func (s Sandbox) LostWithNode() bool {
	if s.State == "stopped" || s.State == "stopping" {
		return false
	}
	return s.StopReason == StopReasonNodeLost || s.StopReason == StopReasonAgentRestarted
}

// StoppedByNodeEvent reports a stopped sandbox whose VM ended because its node
// was lost or its agent restarted. Its disk is kept on the node.
func (s Sandbox) StoppedByNodeEvent() bool {
	return (s.State == "stopped" || s.State == "stopping") &&
		(s.StopReason == StopReasonNodeLost || s.StopReason == StopReasonAgentRestarted)
}

// CreateInput is POST /v1/sandboxes body.
type CreateInput struct {
	// TenantID empty lets the control plane use the caller's tenant (API key or
	// IdP token), or its default tenant.
	TenantID   string `json:"tenant_id,omitempty"`
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
	// AsRoot runs the command as root in the guest. Without it the guest's
	// pod-daemon runs it as the owner of the workspace, or as its default user.
	AsRoot bool `json:"as_root,omitempty"`
	// TimeoutSeconds is how long a buffered exec (Exec, not the stream) may run.
	// 0 leaves it to the control plane's limit (ASP_BUFFERED_EXEC_TIMEOUT, 10
	// minutes by default); the control plane also caps what is asked.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
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

// ListNodes GETs the node inventory (admin or operator with an IdP).
func (c *Client) ListNodes(ctx context.Context) ([]Node, error) {
	var out struct {
		Nodes []Node `json:"nodes"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/v1/nodes", nil, http.StatusOK, &out)
	return out.Nodes, err
}

// SetNodeCordoned cordons (no new sandboxes) or uncordons a node (admin).
func (c *Client) SetNodeCordoned(ctx context.Context, id string, cordoned bool) (Node, error) {
	action := "uncordon"
	if cordoned {
		action = "cordon"
	}
	var out Node
	err := c.doJSON(ctx, http.MethodPost, "/v1/nodes/"+url.PathEscape(id)+"/"+action, nil, http.StatusOK, &out)
	return out, err
}

// APIKey is an API key as the control plane serves it: never its secret,
// except in the response to create and rotate, once.
type APIKey struct {
	ID         string     `json:"id"`
	TenantID   string     `json:"tenant_id"`
	Name       string     `json:"name"`
	Scope      string     `json:"scope"`
	KeyPrefix  string     `json:"key_prefix"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	Secret     string     `json:"secret,omitempty"`
}

// CreateAPIKeyInput is what create asks for. Empty fields take the control
// plane's defaults: the caller's tenant, scope tenant, no expiry.
type CreateAPIKeyInput struct {
	TenantID string `json:"tenant_id,omitempty"`
	Name     string `json:"name"`
	Scope    string `json:"scope,omitempty"`
	TTL      string `json:"ttl,omitempty"`
}

// CreateAPIKey makes a key (platform key or IdP admin). The secret is in the
// result and nowhere else.
func (c *Client) CreateAPIKey(ctx context.Context, in CreateAPIKeyInput) (APIKey, error) {
	var out APIKey
	err := c.doJSON(ctx, http.MethodPost, "/v1/api-keys", in, http.StatusCreated, &out)
	return out, err
}

// ListAPIKeys lists keys, revoked ones included; tenant "" is every tenant.
func (c *Client) ListAPIKeys(ctx context.Context, tenant string) ([]APIKey, error) {
	path := "/v1/api-keys"
	if tenant != "" {
		path += "?tenant_id=" + url.QueryEscape(tenant)
	}
	var out struct {
		Keys []APIKey `json:"keys"`
	}
	err := c.doJSON(ctx, http.MethodGet, path, nil, http.StatusOK, &out)
	return out.Keys, err
}

// RevokeAPIKey stops a key from authenticating.
func (c *Client) RevokeAPIKey(ctx context.Context, id string) (APIKey, error) {
	var out APIKey
	err := c.doJSON(ctx, http.MethodDelete, "/v1/api-keys/"+url.PathEscape(id), nil, http.StatusOK, &out)
	return out, err
}

// RotateAPIKey gives a key a new secret; the old one stops working at once.
func (c *Client) RotateAPIKey(ctx context.Context, id string) (APIKey, error) {
	var out APIKey
	err := c.doJSON(ctx, http.MethodPost, "/v1/api-keys/"+url.PathEscape(id)+"/rotate", nil, http.StatusOK, &out)
	return out, err
}

// SetNodeFence sets the power-off target the control plane uses when it declares
// a node lost (admin). token is the credential or a reference the control plane
// resolves ("env:NAME", "file:/abs/path"). It is never returned by any call.
func (c *Client) SetNodeFence(ctx context.Context, id, endpoint, token string) error {
	body := map[string]string{"endpoint": endpoint}
	if token != "" {
		body["token"] = token
	}
	return c.doJSON(ctx, http.MethodPut, "/v1/nodes/"+url.PathEscape(id)+"/fence", body, http.StatusNoContent, nil)
}

// ClearNodeFence removes a node's fence target (admin).
func (c *Client) ClearNodeFence(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/v1/nodes/"+url.PathEscape(id)+"/fence", nil, http.StatusNoContent, nil)
}

// EnrollToken is a single-use node enrollment token.
type EnrollToken struct {
	Token     string    `json:"token"`
	NodeID    string    `json:"node_id,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	Note      string    `json:"note,omitempty"`
}

// CreateEnrollToken asks for a single-use node enrollment token (IdP admin or
// platform API key). A non-empty nodeID pins it to that node; ttl 0 uses the
// control plane's default.
func (c *Client) CreateEnrollToken(ctx context.Context, nodeID string, ttl time.Duration) (EnrollToken, error) {
	body := map[string]any{}
	if nodeID != "" {
		body["node_id"] = nodeID
	}
	if ttl > 0 {
		body["ttl_seconds"] = int(ttl / time.Second)
	}
	var out EnrollToken
	err := c.doJSON(ctx, http.MethodPost, "/v1/nodes/enroll-tokens", body, http.StatusCreated, &out)
	return out, err
}

// GetSandbox GETs one sandbox by id.
func (c *Client) GetSandbox(ctx context.Context, id string) (Sandbox, error) {
	var out Sandbox
	err := c.doJSON(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(id), nil, http.StatusOK, &out)
	return out, err
}

// ListSandboxes GETs sandboxes, optionally filtered by tenant. Deleted ones are not listed.
func (c *Client) ListSandboxes(ctx context.Context, tenantID string) ([]Sandbox, error) {
	return c.ListSandboxesAll(ctx, tenantID, false)
}

// ListSandboxesAll is ListSandboxes, with the deleted sandboxes (history) when asked.
func (c *Client) ListSandboxesAll(ctx context.Context, tenantID string, includeDeleted bool) ([]Sandbox, error) {
	q := url.Values{}
	if t := strings.TrimSpace(tenantID); t != "" {
		q.Set("tenant_id", t)
	}
	if includeDeleted {
		q.Set("include_deleted", "1")
	}
	path := "/v1/sandboxes"
	if len(q) > 0 {
		path += "?" + q.Encode()
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

// DeleteSandbox DELETEs a sandbox: its VM and its disk go (state deleting, then deleted).
func (c *Client) DeleteSandbox(ctx context.Context, id string) (Sandbox, error) {
	var out Sandbox
	err := c.doJSON(ctx, http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(id), nil, http.StatusOK, &out)
	return out, err
}

// StopSandbox stops a sandbox and keeps its disk (state stopping, then stopped).
func (c *Client) StopSandbox(ctx context.Context, id string) (Sandbox, error) {
	var out Sandbox
	err := c.doJSON(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/stop", nil, http.StatusOK, &out)
	return out, err
}

// StartSandbox resumes a stopped sandbox on its disk, on the node that holds it.
func (c *Client) StartSandbox(ctx context.Context, id string) (Sandbox, error) {
	var out Sandbox
	err := c.doJSON(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/start", nil, http.StatusOK, &out)
	return out, err
}

// ExecWait is how long Exec waits for the control plane when the request names no
// timeout: a little more than the control plane's own default limit (10 minutes),
// so its answer for a command that took too long arrives before the client gives up.
const ExecWait = 11 * time.Minute

// execMargin is what Exec waits beyond the time the request gives the command.
var execMargin = 30 * time.Second

// Exec runs a command in the sandbox via the control-plane proxy.
// The response is buffered JSON (stdout/stderr/exit_code). Smokes use this path.
// A buffered call is not cut at the 60 s of the client's JSON calls: it waits as
// long as the command may run (TimeoutSeconds, else ExecWait).
func (c *Client) Exec(ctx context.Context, id string, req ExecRequest) (ExecResult, error) {
	wait := ExecWait
	if req.TimeoutSeconds > 0 {
		wait = time.Duration(req.TimeoutSeconds)*time.Second + execMargin
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	long := *c
	base := c.HTTPClient
	if base == nil {
		base = http.DefaultClient
	}
	hc := *base
	hc.Timeout = 0 // the context bounds the call
	long.HTTPClient = &hc
	var out ExecResult
	err := long.doJSON(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/exec", req, http.StatusOK, &out)
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
