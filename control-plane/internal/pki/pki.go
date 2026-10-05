// Package pki provides an ephemeral/dev CA and node client-certificate issuance.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	DefaultDevCADir  = "/tmp/asp-dev-ca"
	DefaultCACertRel = "ca.crt"
	DefaultCAKeyRel  = "ca.key"
	DefaultNodeTTL   = 365 * 24 * time.Hour

	// OUNodes marks node-agent certificates; the control plane binds their CN to the node id.
	OUNodes = "nodes"
	// ControlPlaneCN / OUControlPlane name the control plane's client certificate,
	// the only identity node agents accept on their control-plane listener.
	ControlPlaneCN = "asp-control-plane"
	OUControlPlane = "control-plane"
)

// CA holds a loaded certificate authority used to issue node client certs.
type CA struct {
	Cert    *x509.Certificate
	Key     *ecdsa.PrivateKey
	CertPEM []byte
	KeyPEM  []byte
}

// PathsFromEnv resolves CA material paths from ASP_CA_CERT / ASP_CA_KEY or the
// default /tmp/asp-dev-ca directory.
func PathsFromEnv() (certPath, keyPath string) {
	certPath = strings.TrimSpace(os.Getenv("ASP_CA_CERT"))
	keyPath = strings.TrimSpace(os.Getenv("ASP_CA_KEY"))
	if certPath == "" {
		certPath = filepath.Join(DefaultDevCADir, DefaultCACertRel)
	}
	if keyPath == "" {
		keyPath = filepath.Join(DefaultDevCADir, DefaultCAKeyRel)
	}
	return certPath, keyPath
}

// LoadOrCreateDevCA loads an existing CA from paths, or creates a new ephemeral
// ECDSA P-256 CA and persists it (dev/lab only).
func LoadOrCreateDevCA(certPath, keyPath string) (*CA, error) {
	if certPath == "" || keyPath == "" {
		certPath, keyPath = PathsFromEnv()
	}
	if fileExists(certPath) && fileExists(keyPath) {
		return LoadCA(certPath, keyPath)
	}
	ca, err := GenerateCA("ASP Dev CA", 10*365*24*time.Hour)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir ca dir: %w", err)
	}
	if err := os.WriteFile(certPath, ca.CertPEM, 0o644); err != nil {
		return nil, fmt.Errorf("write ca cert: %w", err)
	}
	if err := os.WriteFile(keyPath, ca.KeyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("write ca key: %w", err)
	}
	return ca, nil
}

// LoadCA reads PEM-encoded CA cert and key from disk.
func LoadCA(certPath, keyPath string) (*CA, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read ca cert: %w", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read ca key: %w", err)
	}
	cert, err := ParseCertPEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("parse ca cert: %w", err)
	}
	key, err := ParseECKeyPEM(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse ca key: %w", err)
	}
	return &CA{Cert: cert, Key: key, CertPEM: certPEM, KeyPEM: keyPEM}, nil
}

// GenerateCA creates a new self-signed CA in memory.
func GenerateCA(commonName string, ttl time.Duration) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"agent-sandbox-platform"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(ttl),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return &CA{Cert: cert, Key: key, CertPEM: certPEM, KeyPEM: keyPEM}, nil
}

// IssueResult is the PEM material returned to an enrolling node.
type IssueResult struct {
	CertPEM     []byte
	KeyPEM      []byte
	Fingerprint string
	Serial      string // lowercase hex of certificate serial number
	NotBefore   time.Time
	NotAfter    time.Time
}

// ErrInvalidNodeID is returned when a node id cannot be used as a certificate name.
var ErrInvalidNodeID = errors.New("invalid node id")

// nodeIDPattern: DNS-style labels of letters, digits, '-' and '_' separated by dots.
// The control plane uses the node id as the TLS ServerName when it calls the agent.
var nodeIDPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9_-]*[A-Za-z0-9])?)*$`)

// ValidNodeID reports whether id can name a node certificate.
func ValidNodeID(id string) error {
	if len(id) == 0 || len(id) > 253 || !nodeIDPattern.MatchString(id) {
		return fmt.Errorf("%w %q: use letters, digits, '.', '-' and '_' (at most 253 characters)", ErrInvalidNodeID, id)
	}
	if strings.EqualFold(id, ControlPlaneCN) {
		return fmt.Errorf("%w %q: reserved for the control plane", ErrInvalidNodeID, id)
	}
	return nil
}

// EndpointHosts returns the hosts of agent endpoint URLs, for certificate SANs.
// Unspecified addresses (0.0.0.0, ::) and unparsable values are skipped.
func EndpointHosts(endpoints ...string) []string {
	var hosts []string
	for _, raw := range endpoints {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Hostname() == "" {
			continue
		}
		h := u.Hostname()
		if ip := net.ParseIP(h); ip != nil && ip.IsUnspecified() {
			continue
		}
		if !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// IssueNodeCert issues a node certificate (CN = node id, OU nodes) that works as an
// mTLS client towards the control plane and as the TLS server certificate of the
// agent's control-plane listener. SANs carry the node id, which the control plane
// sets as ServerName, plus the given endpoint hosts so operators can reach the agent
// by address.
func (c *CA) IssueNodeCert(nodeID string, hosts []string, ttl time.Duration) (*IssueResult, error) {
	nodeID = strings.TrimSpace(nodeID)
	if err := ValidNodeID(nodeID); err != nil {
		return nil, err
	}
	var dnsNames []string
	var ips []net.IP
	for _, h := range append([]string{nodeID}, hosts...) {
		if ip := net.ParseIP(h); ip != nil {
			if !slices.ContainsFunc(ips, ip.Equal) {
				ips = append(ips, ip)
			}
		} else if h != "" && !slices.Contains(dnsNames, h) {
			dnsNames = append(dnsNames, h)
		}
	}
	return c.issue(pkix.Name{
		CommonName:         nodeID,
		Organization:       []string{"agent-sandbox-platform"},
		OrganizationalUnit: []string{OUNodes},
	}, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, dnsNames, ips, ttl)
}

// IssueNodeClient issues a node certificate without extra endpoint hosts.
func (c *CA) IssueNodeClient(nodeID string, ttl time.Duration) (*IssueResult, error) {
	return c.IssueNodeCert(nodeID, nil, ttl)
}

// IssueControlPlaneClient issues the control plane's client certificate for calls to
// node agents (CN asp-control-plane, OU control-plane). Agents accept only this
// identity on their control-plane listener, so a node certificate from the same CA
// cannot call another node's exec API.
func (c *CA) IssueControlPlaneClient(ttl time.Duration) (*IssueResult, error) {
	return c.issue(pkix.Name{
		CommonName:         ControlPlaneCN,
		Organization:       []string{"agent-sandbox-platform"},
		OrganizationalUnit: []string{OUControlPlane},
	}, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil, nil, ttl)
}

func (c *CA) issue(subject pkix.Name, ekus []x509.ExtKeyUsage, dnsNames []string, ips []net.IP, ttl time.Duration) (*IssueResult, error) {
	if c == nil || c.Cert == nil || c.Key == nil {
		return nil, fmt.Errorf("ca not loaded")
	}
	if ttl <= 0 {
		ttl = DefaultNodeTTL
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               subject,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(ttl),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           ekus,
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, &key.PublicKey, c.Key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return &IssueResult{
		CertPEM:     certPEM,
		KeyPEM:      keyPEM,
		Fingerprint: Fingerprint(cert),
		Serial:      strings.ToLower(cert.SerialNumber.Text(16)),
		NotBefore:   cert.NotBefore,
		NotAfter:    cert.NotAfter,
	}, nil
}

// VerifyClientCert verifies a PEM client cert was issued by this CA and is currently valid.
func (c *CA) VerifyClientCert(certPEM []byte) (*x509.Certificate, error) {
	cert, err := ParseCertPEM(certPEM)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(c.Cert)
	opts := x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if _, err := cert.Verify(opts); err != nil {
		return nil, fmt.Errorf("verify client cert: %w", err)
	}
	return cert, nil
}

// Fingerprint returns lowercase hex SHA-256 of the raw certificate DER.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// FingerprintPEM parses PEM and returns the fingerprint.
func FingerprintPEM(certPEM []byte) (string, error) {
	cert, err := ParseCertPEM(certPEM)
	if err != nil {
		return "", err
	}
	return Fingerprint(cert), nil
}

// ParseCertPEM decodes the first CERTIFICATE PEM block.
func ParseCertPEM(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("no CERTIFICATE PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}

// ParseECKeyPEM decodes an EC PRIVATE KEY or PKCS8 private key PEM.
func ParseECKeyPEM(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no private key PEM block")
	}
	switch block.Type {
	case "EC PRIVATE KEY":
		return x509.ParseECPrivateKey(block.Bytes)
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		ec, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("not an EC private key")
		}
		return ec, nil
	default:
		return nil, fmt.Errorf("unsupported key PEM type %q", block.Type)
	}
}

// CertPool returns an x509.CertPool containing this CA.
func (c *CA) CertPool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(c.Cert)
	return pool
}

func randSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
