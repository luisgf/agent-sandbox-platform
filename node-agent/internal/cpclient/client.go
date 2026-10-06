// Package cpclient is the node-agent HTTP client for the control plane.
package cpclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Client struct {
	BaseURL string
	HTTP    *http.Client
}

type EnrollRequest struct {
	ID             string   `json:"id,omitempty"`
	Name           string   `json:"name,omitempty"`
	Endpoint       string   `json:"endpoint,omitempty"`
	AgentEndpoint  string   `json:"agent_endpoint,omitempty"`
	VMMProfiles    []string `json:"vmm_profiles,omitempty"`
	CapacityCPU    int      `json:"capacity_cpu"`
	CapacityMemMiB int      `json:"capacity_mem_mib"`
}

type EnrollResponse struct {
	NodeID          string `json:"node_id"`
	ClientCertPEM   string `json:"client_cert_pem"`
	ClientKeyPEM    string `json:"client_key_pem"`
	CACertPEM       string `json:"ca_cert_pem"`
	CertFingerprint string `json:"cert_fingerprint"`
}

type RegisterRequest struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Endpoint       string   `json:"endpoint"`
	AgentEndpoint  string   `json:"agent_endpoint"`
	VMMProfiles    []string `json:"vmm_profiles"`
	CapacityCPU    int      `json:"capacity_cpu"`
	CapacityMemMiB int      `json:"capacity_mem_mib"`
	MaxSandboxes   int      `json:"max_sandboxes"`
	AcceptsWork    *bool    `json:"accepts_work,omitempty"`
	LocalNetDial   string   `json:"local_net_dial,omitempty"`
	// AgentInstanceID is random per process: a new one tells the control plane
	// this agent restarted and lost track of its running VMs.
	AgentInstanceID string `json:"agent_instance_id,omitempty"`
	FenceEndpoint   string `json:"fence_endpoint,omitempty"`
	FenceToken      string `json:"fence_token,omitempty"`
}

func New(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: httpClient}
}

// LoadMTLSClient builds an HTTP client using certs from certDir when present
// or when force is true (ASP_MTLS=1). The control plane's TLS certificate is
// verified against cpCAFile when set (--control-plane-ca), else against the
// enrollment CA in certDir/ca.crt when present, else the system roots.
func LoadMTLSClient(certDir string, force bool, cpCAFile string) (*http.Client, bool, error) {
	certPath := filepath.Join(certDir, "client.crt")
	keyPath := filepath.Join(certDir, "client.key")
	caPath := filepath.Join(certDir, "ca.crt")
	have := fileExists(certPath) && fileExists(keyPath)
	if !have && !force {
		c, err := NewEnrollHTTPClient(cpCAFile)
		return c, false, err
	}
	if !have {
		return nil, false, fmt.Errorf("mTLS required but certs missing in %s", certDir)
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, false, err
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	rootsFile := cpCAFile
	if rootsFile == "" && fileExists(caPath) {
		rootsFile = caPath
	}
	if rootsFile != "" {
		pool, err := loadCertPool(rootsFile)
		if err != nil {
			return nil, false, err
		}
		tlsCfg.RootCAs = pool
	}
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
		},
	}, true, nil
}

// NewEnrollHTTPClient is the client for calls made before the node has a
// certificate (enrollment). With cpCAFile it trusts only that CA for the
// control plane's TLS certificate, so a node can enroll against a control plane
// on another host whose certificate is not in the system roots.
func NewEnrollHTTPClient(cpCAFile string) (*http.Client, error) {
	c := &http.Client{Timeout: 15 * time.Second}
	if cpCAFile == "" {
		return c, nil
	}
	pool, err := loadCertPool(cpCAFile)
	if err != nil {
		return nil, err
	}
	c.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	return c, nil
}

func loadCertPool(path string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("no CA certs in %s", path)
	}
	return pool, nil
}

func (c *Client) Enroll(ctx context.Context, bootstrapToken string, req EnrollRequest) (EnrollResponse, error) {
	var out EnrollResponse
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/nodes/enroll", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+bootstrapToken)
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return out, httpError("enroll status", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	return out, nil
}

func (c *Client) Register(ctx context.Context, req RegisterRequest) error {
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/nodes/register", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return httpError("register status", resp.StatusCode, raw)
	}
	return nil
}

func (c *Client) Heartbeat(ctx context.Context, nodeID string) error {
	url := fmt.Sprintf("%s/v1/nodes/%s/heartbeat", c.BaseURL, nodeID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return httpError("heartbeat status", resp.StatusCode, raw)
	}
	return nil
}

// HTTPError is a non-2xx answer from the control plane.
type HTTPError struct {
	Op         string // e.g. "heartbeat status", "work status"
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s %d: %s", e.Op, e.StatusCode, e.Body)
}

func httpError(op string, status int, body []byte) error {
	return &HTTPError{Op: op, StatusCode: status, Body: string(bytes.TrimSpace(body))}
}

// IsNotFound reports a 404 from the control plane (e.g. it forgot this node).
func IsNotFound(err error) bool { return statusIs(err, http.StatusNotFound) }

// IsConflict reports a 409 from the control plane (e.g. the sandbox is no longer ours).
func IsConflict(err error) bool { return statusIs(err, http.StatusConflict) }

func statusIs(err error, code int) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.StatusCode == code
}

// WriteCerts persists enrollment PEMs into certDir.
func WriteCerts(certDir, clientCert, clientKey, caCert string) error {
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(certDir, "client.crt"), []byte(clientCert), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(certDir, "client.key"), []byte(clientKey), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(certDir, "ca.crt"), []byte(caCert), 0o644); err != nil {
		return err
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Sandbox is a minimal control-plane sandbox view for the reconciler.
type Sandbox struct {
	ID         string  `json:"id"`
	TenantID   string  `json:"tenant_id"`
	NodeID     *string `json:"node_id"`
	State      string  `json:"state"`
	ImageRef   string  `json:"image_ref"`
	CPUMillis  int     `json:"cpu_millis"`
	MemoryMiB  int     `json:"memory_mib"`
	VMMProfile string  `json:"vmm_profile"`
	// OwnerSub is the IdP subject of the sandbox creator (ADR-0007). Used to
	// expand ASP_SSH_AGENT_SOCK_TEMPLATE for per-user SSH agent upstreams.
	OwnerSub string `json:"owner_sub,omitempty"`
	// WorkspaceHostPath is the host directory from the sandbox spec. Empty if
	// the session did not ask for a share. The reconciler starts virtiofsd
	// for a non-empty path; the guest mounts tag "workspace" itself.
	WorkspaceHostPath string `json:"workspace_host_path,omitempty"`
	// LocalNet selects the full-tunnel default route (ADR-0010). When true the
	// node must not install the public proxy default for this sandbox.
	LocalNet bool `json:"local_net"`
	// LocalNetState is off | pending | up | withdrawn.
	LocalNetState string `json:"local_net_state"`
	// LocalNetClientPublic is the local agent's WG public key once up.
	LocalNetClientPublic string `json:"local_net_client_public,omitempty"`
}

type workResponse struct {
	Sandboxes []Sandbox `json:"sandboxes"`
}

func (c *Client) ListWork(ctx context.Context, nodeID string) ([]Sandbox, error) {
	url := fmt.Sprintf("%s/v1/nodes/%s/work", c.BaseURL, nodeID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, httpError("work status", resp.StatusCode, raw)
	}
	var out workResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.Sandboxes == nil {
		return []Sandbox{}, nil
	}
	return out.Sandboxes, nil
}

func (c *Client) Claim(ctx context.Context, sandboxID, nodeID string) (Sandbox, error) {
	var out Sandbox
	body, _ := json.Marshal(map[string]string{"node_id": nodeID})
	url := fmt.Sprintf("%s/v1/sandboxes/%s/claim", c.BaseURL, sandboxID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return out, httpError("claim status", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	return out, nil
}

func (c *Client) ReportStatus(ctx context.Context, sandboxID, state, detail string) (Sandbox, error) {
	var out Sandbox
	payload := map[string]string{"state": state}
	if detail != "" {
		payload["detail"] = detail
	}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/v1/sandboxes/%s/status", c.BaseURL, sandboxID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return out, httpError("status update", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	return out, nil
}

// PublishLocalNetNode registers this sandbox's WireGuard public key.
// The private key is not sent. Failure to publish does not change egress by itself.
func (c *Client) PublishLocalNetNode(ctx context.Context, sandboxID, publicKey string) error {
	body, _ := json.Marshal(map[string]string{"public_key": publicKey})
	url := fmt.Sprintf("%s/v1/sandboxes/%s/local-net/node-public", c.BaseURL, sandboxID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return httpError("local-net node-public", resp.StatusCode, raw)
	}
	return nil
}

func (c *Client) RenewLease(ctx context.Context, sandboxID, nodeID string) (Sandbox, error) {
	var out Sandbox
	body, _ := json.Marshal(map[string]string{"node_id": nodeID})
	url := fmt.Sprintf("%s/v1/sandboxes/%s/renew-lease", c.BaseURL, sandboxID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return out, httpError("renew-lease status", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	return out, nil
}

func (c *Client) Attest(ctx context.Context, sandboxID string, evidence any) error {
	body, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/v1/sandboxes/%s/attest", c.BaseURL, sandboxID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return httpError("attest status", resp.StatusCode, raw)
	}
	return nil
}
