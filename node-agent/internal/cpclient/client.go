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
	c, nc, err := LoadMTLSClientCert(certDir, force, cpCAFile)
	return c, nc != nil, err
}

// LoadMTLSClientCert is LoadMTLSClient that also returns the node certificate
// the client presents (nil without one). The client reads it at each
// handshake: after NodeCert.Reload and CloseIdleConnections, new connections
// present the renewed certificate.
func LoadMTLSClientCert(certDir string, force bool, cpCAFile string) (*http.Client, *NodeCert, error) {
	certPath := filepath.Join(certDir, "client.crt")
	keyPath := filepath.Join(certDir, "client.key")
	caPath := filepath.Join(certDir, "ca.crt")
	have := fileExists(certPath) && fileExists(keyPath)
	if !have && !force {
		c, err := NewEnrollHTTPClient(cpCAFile)
		return c, nil, err
	}
	if !have {
		return nil, nil, fmt.Errorf("mTLS required but certs missing in %s", certDir)
	}
	nc, err := LoadNodeCert(certDir)
	if err != nil {
		return nil, nil, err
	}
	tlsCfg := &tls.Config{
		GetClientCertificate: nc.GetClientCertificate,
		MinVersion:           tls.VersionTLS12,
	}
	rootsFile := cpCAFile
	if rootsFile == "" && fileExists(caPath) {
		rootsFile = caPath
	}
	if rootsFile != "" {
		pool, err := loadCertPool(rootsFile)
		if err != nil {
			return nil, nil, err
		}
		tlsCfg.RootCAs = pool
	}
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
		},
	}, nc, nil
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

// HeartbeatInfo is what a heartbeat may carry besides "I am alive".
type HeartbeatInfo struct {
	// DiskFreeMiB is the free space of --disk-dir, where the disks of stopped
	// sandboxes stay (ADR-0012). Nil is not reported.
	DiskFreeMiB *int64 `json:"disk_free_mib,omitempty"`
}

// Heartbeat tells the control plane the node is alive. The body is omitted when
// info reports nothing, so a control plane that predates it sees what it always did.
func (c *Client) Heartbeat(ctx context.Context, nodeID string, info HeartbeatInfo) error {
	url := fmt.Sprintf("%s/v1/nodes/%s/heartbeat", c.BaseURL, nodeID)
	var body io.Reader
	if info.DiskFreeMiB != nil {
		raw, _ := json.Marshal(info)
		body = bytes.NewReader(raw)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return err
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
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
// Each file is replaced atomically (temporary file and rename); the key is
// written before the certificate, so the window in which they do not match
// is the gap between two renames.
func WriteCerts(certDir, clientCert, clientKey, caCert string) error {
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(certDir, "ca.crt"), []byte(caCert), 0o644); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(certDir, "client.key"), []byte(clientKey), 0o600); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(certDir, "client.crt"), []byte(clientCert), 0o644)
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
	// BootCount is how many times the sandbox has been started: 1 for the first
	// boot, more after each resume (ADR-0012). 0 from a control plane that
	// predates the field, read as a first boot.
	BootCount int `json:"boot_count,omitempty"`
}

type workResponse struct {
	Sandboxes []Sandbox   `json:"sandboxes"`
	Assigned  *[]string   `json:"assigned"`
	Retained  *[]string   `json:"retained"`
	Egress    *WorkEgress `json:"egress"`
}

// WorkEgress carries the egress policy of every sandbox assigned to the node.
type WorkEgress struct {
	// Tenants maps each assigned sandbox to its tenant.
	Tenants map[string]string `json:"tenants"`
	// Policies holds each tenant's effective policy.
	Policies map[string]EgressPolicy `json:"policies"`
}

// EgressPolicy is a tenant's effective egress policy. Version changes when
// what the policy allows changes.
type EgressPolicy struct {
	TenantID string       `json:"tenant_id"`
	Mode     string       `json:"mode"`
	Rules    []EgressRule `json:"rules"`
	Version  string       `json:"version"`
}

// EgressRule is one allowlist entry.
type EgressRule struct {
	HostPattern string `json:"host_pattern"`
	Port        *int   `json:"port,omitempty"`
	Enabled     bool   `json:"enabled"`
}

// Work is one poll of GET /v1/nodes/{id}/work.
type Work struct {
	// Sandboxes need this node's action: claim, start, stop, local-net.
	Sandboxes []Sandbox
	// Assigned lists every sandbox the control plane places on this node. Nil
	// when the control plane predates the field: then nothing may be stopped
	// for being missing from it.
	Assigned []string
	// Retained lists the stopped sandboxes on this node whose disks must stay
	// (ADR-0012). Nil when the control plane predates the field: then stopping a
	// sandbox still deletes its disk, as it always did.
	Retained []string
	// Egress is nil when the control plane does not send policies (older
	// version, or it could not read them this time).
	Egress *WorkEgress
}

func (c *Client) ListWork(ctx context.Context, nodeID string) (Work, error) {
	url := fmt.Sprintf("%s/v1/nodes/%s/work", c.BaseURL, nodeID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Work{}, err
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return Work{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return Work{}, httpError("work status", resp.StatusCode, raw)
	}
	var out workResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return Work{}, err
	}
	work := Work{Sandboxes: out.Sandboxes, Egress: out.Egress}
	if work.Sandboxes == nil {
		work.Sandboxes = []Sandbox{}
	}
	if out.Assigned != nil {
		work.Assigned = append([]string{}, *out.Assigned...)
	}
	if out.Retained != nil {
		work.Retained = append([]string{}, *out.Retained...)
	}
	return work, nil
}

// Retains reports whether the control plane keeps the disks of stopped
// sandboxes: it sent the retained list, even an empty one.
func (w Work) Retains() bool { return w.Retained != nil }

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
// LocalNetTunnel is what the node allocated for a local-net session: the UDP
// port of its WireGuard device and the two ends of the tunnel /30.
type LocalNetTunnel struct {
	ListenPort int    `json:"listen_port"`
	NodeAddr   string `json:"node_tunnel_addr"`
	ClientAddr string `json:"client_tunnel_addr"`
}

// PublishLocalNetNode gives the control plane the node device's public key
// and the session's tunnel parameters, which the grant hands to the laptop.
func (c *Client) PublishLocalNetNode(ctx context.Context, sandboxID, publicKey string, tun LocalNetTunnel) error {
	body, _ := json.Marshal(map[string]any{
		"public_key":         publicKey,
		"listen_port":        tun.ListenPort,
		"node_tunnel_addr":   tun.NodeAddr,
		"client_tunnel_addr": tun.ClientAddr,
	})
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
