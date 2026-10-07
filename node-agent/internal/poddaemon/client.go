// Package poddaemon talks to the guest pod-daemon over Unix, CH hybrid vsock, or AF_VSOCK.
package poddaemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// ExecRequest is forwarded to pod-daemon POST /v1/exec.
type ExecRequest struct {
	Cmd []string          `json:"cmd"`
	Env map[string]string `json:"env,omitempty"`
	Cwd string            `json:"cwd,omitempty"`
	// PTY asks the guest to run the command on a pseudoterminal.
	PTY bool `json:"pty,omitempty"`
	// Rows and Cols are the initial PTY size. Zero means the guest default (24x80).
	Rows int `json:"rows,omitempty"`
	Cols int `json:"cols,omitempty"`
	// Stdin is one-shot input for the buffered JSON exec. Ignored by the stream
	// path; streaming stdin uses POST /v1/exec/stdin after the ready event.
	Stdin string `json:"stdin,omitempty"`
	// StdinStream keeps a pipe open on the non-PTY stream path so the client
	// can POST stdin and then close it (real EOF). PTY sessions do not need it.
	StdinStream bool `json:"stdin_stream,omitempty"`
	// TimeoutSecs is how long a buffered exec may run in the guest (pod-daemon
	// caps it); 0 uses the daemon's default. A stream ignores it.
	TimeoutSecs int `json:"timeout_secs,omitempty"`
	// AsRoot runs the command as root in the guest. Without it pod-daemon runs it
	// as the owner of the workspace, or as its default exec user. A pod-daemon
	// that predates this ignores the field and runs everything as root.
	AsRoot bool `json:"as_root,omitempty"`
}

// StdinMessage is POST /v1/exec/stdin. Data is a JSON string (not an opaque
// byte pipe). Close delivers EOF: the write end of a pipe, or Ctrl-D on a
// canonical PTY. Rows/Cols resize a PTY when both are non-zero.
type StdinMessage struct {
	ExecID string `json:"exec_id"`
	Data   string `json:"data,omitempty"`
	Close  bool   `json:"close,omitempty"`
	Rows   int    `json:"rows,omitempty"`
	Cols   int    `json:"cols,omitempty"`
}

// ExecResponse is the sync MVP result from pod-daemon.
type ExecResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// Client dials pod-daemon via an injectable Dialer (Unix / hybrid vsock / AF_VSOCK).
type Client struct {
	Dialer Dialer
	// HTTP has no overall timeout: it would also cover reading the body and cut
	// a streamed exec or a PTY session while output is still flowing.
	HTTP *http.Client
	// Socket is retained for backward-compatible logging when using UnixDialer.
	Socket string
	// BufferedTimeout bounds a buffered call (exec without stream, stdin,
	// healthz), response included. A stream has no deadline here: it lasts as
	// long as the command, and the caller cancelling its context ends it.
	BufferedTimeout time.Duration
}

// DefaultBufferedTimeout is the limit of a buffered call to pod-daemon.
const DefaultBufferedTimeout = 60 * time.Second

// NewClient builds a Unix-socket client (dry-run / --pod-daemon-sock).
func NewClient(socket string) *Client {
	c := NewClientFromDialer(&UnixDialer{Path: socket})
	c.Socket = socket
	return c
}

// NewClientFromDialer builds a Client around any Dialer (tests inject fakes).
func NewClientFromDialer(d Dialer) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.Dial(ctx)
		},
		// pod-daemon keeps buffered connections open: reuse them instead of
		// redialling (and repeating the hybrid vsock CONNECT) for every call.
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{
		Dialer:          d,
		HTTP:            &http.Client{Transport: transport},
		BufferedTimeout: DefaultBufferedTimeout,
	}
}

// CloseIdleConnections drops the kept-alive connections to pod-daemon.
func (c *Client) CloseIdleConnections() {
	c.HTTP.CloseIdleConnections()
}

// buffered bounds a call whose response arrives in one piece.
func (c *Client) buffered(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.BufferedTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.BufferedTimeout)
}

// bufferedFor is buffered for an exec that may run for command: the deadline is
// the longer of the client's own and command plus a margin.
func (c *Client) bufferedFor(ctx context.Context, command time.Duration) (context.Context, context.CancelFunc) {
	limit := c.BufferedTimeout
	if command > 0 && limit > 0 && command+bufferedMargin > limit {
		limit = command + bufferedMargin
	}
	if limit <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, limit)
}

func (c *Client) Healthz(ctx context.Context) error {
	ctx, cancel := c.buffered(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://pod-daemon/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz status %d", resp.StatusCode)
	}
	return nil
}

// bufferedMargin is what a buffered exec's deadline here exceeds the time the
// guest gives the command, so the guest's own timeout (exit 124 with the output
// so far) answers before this client gives up.
const bufferedMargin = 15 * time.Second

func (c *Client) Exec(ctx context.Context, in ExecRequest) (ExecResponse, error) {
	ctx, cancel := c.bufferedFor(ctx, time.Duration(in.TimeoutSecs)*time.Second)
	defer cancel()
	body, err := json.Marshal(in)
	if err != nil {
		return ExecResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://pod-daemon/v1/exec", bytes.NewReader(body))
	if err != nil {
		return ExecResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return ExecResponse{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return ExecResponse{}, fmt.Errorf("exec status %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
	}
	var out ExecResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return ExecResponse{}, err
	}
	return out, nil
}

// ErrStreamUnsupported means the guest pod-daemon has no NDJSON exec.
var ErrStreamUnsupported = fmt.Errorf("pod-daemon exec stream not supported")

// OpenExecStream POSTs /v1/exec?stream=1 and returns the response on success.
// The caller must Close the body. A 404 is ErrStreamUnsupported so the proxy
// can fall back to the buffered JSON exec. No timeout applies: the stream lasts
// as long as ctx, which the control plane cancels when its caller goes away.
func (c *Client) OpenExecStream(ctx context.Context, in ExecRequest) (*http.Response, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://pod-daemon/v1/exec?stream=1", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrStreamUnsupported
	}
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, fmt.Errorf("exec stream status %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
	}
	return resp, nil
}

// WriteStdin delivers bytes or EOF to a streaming exec identified by ExecID.
func (c *Client) WriteStdin(ctx context.Context, in StdinMessage) error {
	ctx, cancel := c.buffered(ctx)
	defer cancel()
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://pod-daemon/v1/exec/stdin", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("exec stdin status %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
	}
	return nil
}
