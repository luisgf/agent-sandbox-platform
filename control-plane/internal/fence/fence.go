// Package fence provides STONITH / power-fence providers.
// Real STONITH requires out-of-band BMC access; software leases alone are insufficient.
package fence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Target describes the node to power-fence.
type Target struct {
	NodeID   string
	Endpoint string // fence_endpoint (webhook URL, Redfish base, or IPMI host)
	Token    string // fence_token / basic password / IPMI pass
	User     string // optional; Redfish/IPMI username (ASP_FENCE_USER)
}

// FenceProvider power-offs a dead node so another node may safely reclaim sandboxes.
type FenceProvider interface {
	Name() string
	Fence(ctx context.Context, t Target) error
}

// Noop never fences (default / lab).
type Noop struct{}

func (Noop) Name() string                        { return "noop" }
func (Noop) Fence(context.Context, Target) error { return nil }

// HTTPWebhook POSTs a JSON power-off request to fence_endpoint.
// Auth: Bearer fence_token when set.
type HTTPWebhook struct {
	Client *http.Client
}

func (h *HTTPWebhook) Name() string { return "http_webhook" }

func (h *HTTPWebhook) Fence(ctx context.Context, t Target) error {
	if strings.TrimSpace(t.Endpoint) == "" {
		return errors.New("fence_endpoint required for http_webhook")
	}
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	body, _ := json.Marshal(map[string]any{
		"action":  "power_off",
		"node_id": t.NodeID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if t.Token != "" {
		req.Header.Set("Authorization", "Bearer "+t.Token)
		req.Header.Set("X-ASP-Fence-Token", t.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("fence webhook status %d", resp.StatusCode)
	}
	return nil
}

// Redfish is a stub that POSTs ComputerSystem.Reset (ForceOff) with HTTP basic auth.
type Redfish struct {
	Client *http.Client
}

func (r *Redfish) Name() string { return "redfish" }

func (r *Redfish) Fence(ctx context.Context, t Target) error {
	if strings.TrimSpace(t.Endpoint) == "" {
		return errors.New("fence_endpoint required for redfish")
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	base := strings.TrimRight(t.Endpoint, "/")
	url := base + "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset"
	body := []byte(`{"ResetType":"ForceOff"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	user := t.User
	if user == "" {
		user = strings.TrimSpace(os.Getenv("ASP_FENCE_USER"))
	}
	if user == "" {
		user = "admin"
	}
	if t.Token != "" {
		req.SetBasicAuth(user, t.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("redfish reset status %d", resp.StatusCode)
	}
	return nil
}

// SoftFailError marks a soft failure (e.g. ipmitool missing).
type SoftFailError struct {
	Msg string
}

func (e SoftFailError) Error() string { return e.Msg }

// IPMI stubs fencing via `ipmitool` when present; SoftFail if binary missing.
type IPMI struct {
	// Exec looks up and runs a command; defaults to exec.CommandContext.
	Exec func(ctx context.Context, name string, args ...string) *exec.Cmd
	// LookPath defaults to exec.LookPath.
	LookPath func(file string) (string, error)
}

func (i *IPMI) Name() string { return "ipmi" }

func (i *IPMI) Fence(ctx context.Context, t Target) error {
	look := i.LookPath
	if look == nil {
		look = exec.LookPath
	}
	bin, err := look("ipmitool")
	if err != nil {
		return SoftFailError{Msg: "ipmitool not present (soft-fail)"}
	}
	host := strings.TrimSpace(t.Endpoint)
	if host == "" {
		return errors.New("fence_endpoint (IPMI host) required")
	}
	user := t.User
	if user == "" {
		user = strings.TrimSpace(os.Getenv("ASP_FENCE_USER"))
	}
	if user == "" {
		user = "ADMIN"
	}
	pass := t.Token
	if pass == "" {
		pass = strings.TrimSpace(os.Getenv("ASP_FENCE_PASS"))
	}
	run := i.Exec
	if run == nil {
		run = exec.CommandContext
	}
	// -E reads the password from IPMI_PASSWORD: with -P it would be in the
	// argument list, which every user of the host can read with ps.
	cmd := run(ctx, bin, "-I", "lanplus", "-H", host, "-U", user, "-E", "chassis", "power", "off")
	cmd.Env = append(os.Environ(), "IPMI_PASSWORD="+pass)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ipmitool: %w (%s)", err, bytes.TrimSpace(out))
	}
	return nil
}

// FromEnv builds a FenceProvider from ASP_FENCE_PROVIDER.
// Values: noop (default), http_webhook|webhook, redfish, ipmi.
func FromEnv() FenceProvider {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ASP_FENCE_PROVIDER"))) {
	case "", "noop", "none":
		return Noop{}
	case "http_webhook", "webhook", "http":
		return &HTTPWebhook{}
	case "redfish":
		return &Redfish{}
	case "ipmi":
		return &IPMI{}
	default:
		return Noop{}
	}
}

// Enabled reports whether a provider that can power a node off is configured.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ASP_FENCE_PROVIDER"))) {
	case "http_webhook", "webhook", "http", "redfish", "ipmi":
		return true
	default:
		return false
	}
}

// Validate refuses a value of ASP_FENCE_PROVIDER that names no provider. FromEnv answers the
// no-op provider for such a value, and a control plane that thinks fencing is on while the provider
// does nothing records "node.fenced" for a node nobody powered off: a typo (redfsh) must stop the
// start, not turn fencing off in silence.
func Validate() error {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("ASP_FENCE_PROVIDER")))
	switch v {
	case "", "noop", "none", "http_webhook", "webhook", "http", "redfish", "ipmi":
		return nil
	}
	return fmt.Errorf("ASP_FENCE_PROVIDER=%q is not a fence provider: use noop, http_webhook, redfish or ipmi", v)
}
