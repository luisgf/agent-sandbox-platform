// Package cpclient is the node-agent HTTP client for the control plane.
package cpclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
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
	FenceEndpoint  string   `json:"fence_endpoint,omitempty"`
	FenceToken     string   `json:"fence_token,omitempty"`
}

func New(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: httpClient}
}

// LoadMTLSClient builds an HTTP client using certs from certDir when present
// or when force is true (ASP_MTLS=1).
func LoadMTLSClient(certDir string, force bool) (*http.Client, bool, error) {
	certPath := filepath.Join(certDir, "client.crt")
	keyPath := filepath.Join(certDir, "client.key")
	caPath := filepath.Join(certDir, "ca.crt")
	have := fileExists(certPath) && fileExists(keyPath)
	if !have && !force {
		return &http.Client{Timeout: 15 * time.Second}, false, nil
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
	if fileExists(caPath) {
		pemBytes, err := os.ReadFile(caPath)
		if err != nil {
			return nil, false, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, false, fmt.Errorf("no CA certs in %s", caPath)
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
		return out, fmt.Errorf("enroll status %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
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
		return fmt.Errorf("register status %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
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
		return fmt.Errorf("heartbeat status %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
	}
	return nil
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
		return nil, fmt.Errorf("work status %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
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
		return out, fmt.Errorf("claim status %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
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
		return out, fmt.Errorf("status update %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	return out, nil
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
		return out, fmt.Errorf("renew-lease status %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
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
		return fmt.Errorf("attest status %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
	}
	return nil
}
