package cpclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
)

// NodeCert holds the node's current certificate and key from cert-dir and
// hands them to TLS clients and servers at handshake time, so a renewed
// certificate takes effect on the next connection without a restart.
type NodeCert struct {
	dir string
	cur atomic.Pointer[tls.Certificate] // Leaf is set
}

// LoadNodeCert reads cert-dir/client.crt and client.key.
func LoadNodeCert(dir string) (*NodeCert, error) {
	n := &NodeCert{dir: dir}
	if err := n.Reload(); err != nil {
		return nil, err
	}
	return n, nil
}

// Reload reads the certificate and key again. On error the current pair stays.
func (n *NodeCert) Reload() error {
	pair, err := tls.LoadX509KeyPair(filepath.Join(n.dir, "client.crt"), filepath.Join(n.dir, "client.key"))
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse node certificate: %w", err)
	}
	pair.Leaf = leaf
	n.cur.Store(&pair)
	return nil
}

// Dir is the cert-dir the certificate comes from.
func (n *NodeCert) Dir() string { return n.dir }

// Current returns the certificate in use.
func (n *NodeCert) Current() *tls.Certificate { return n.cur.Load() }

// Leaf returns the parsed certificate in use.
func (n *NodeCert) Leaf() *x509.Certificate { return n.cur.Load().Leaf }

// GetClientCertificate is a tls.Config hook for calls to the control plane.
func (n *NodeCert) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return n.cur.Load(), nil
}

// GetCertificate is a tls.Config hook for the node's TLS listener.
func (n *NodeCert) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return n.cur.Load(), nil
}

// RotateCert asks the control plane for a new certificate for nodeID. The
// request is authenticated by the current certificate (mTLS), so c must be the
// node's mTLS client.
func (c *Client) RotateCert(ctx context.Context, nodeID string) (EnrollResponse, error) {
	var out EnrollResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/nodes/"+url.PathEscape(nodeID)+"/rotate-cert", nil)
	if err != nil {
		return out, err
	}
	if err := c.doJSON(req, &out); err != nil {
		return out, fmt.Errorf("rotate-cert: %w", err)
	}
	if out.ClientCertPEM == "" || out.ClientKeyPEM == "" {
		return out, errors.New("rotate-cert: response without a certificate")
	}
	return out, nil
}

// doJSON sends req and decodes a 2xx JSON response into dest.
func (c *Client) doJSON(req *http.Request, dest any) error {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return httpError(req.Method+" "+req.URL.Path, resp.StatusCode, raw)
	}
	return json.Unmarshal(raw, dest)
}

// writeFileAtomic writes data to path through a temporary file in the same
// directory and a rename, so a reader never sees a half-written file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
