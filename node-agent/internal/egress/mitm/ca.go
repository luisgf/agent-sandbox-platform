// Package mitm provides optional TLS CONNECT bump for the egress forward proxy.
// Default is OFF (corp caution). Enable only with ASP_EGRESS_MITM=1.
package mitm

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// CA issues per-host leaf certificates for CONNECT bump.
type CA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	tlsCert tls.Certificate
	mu      sync.Mutex
	cache   map[string]*tls.Certificate
}

// LoadOrGenerate loads PEM CA from path (cert+key concatenated or dir with ca.crt/ca.key),
// or generates a new ephemeral CA and writes it when path is a file path.
func LoadOrGenerate(path string) (*CA, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = filepath.Join(os.TempDir(), "asp-egress-mitm-ca.pem")
	}
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		return loadDir(path)
	}
	if data, err := os.ReadFile(path); err == nil {
		return parseBundle(data)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	ca, err := generate()
	if err != nil {
		return nil, err
	}
	bundle := append(ca.CertPEM(), ca.KeyPEM()...)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, bundle, 0o600); err != nil {
		return nil, err
	}
	return ca, nil
}

func loadDir(dir string) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		return nil, err
	}
	return parseBundle(append(certPEM, keyPEM...))
}

func parseBundle(data []byte) (*CA, error) {
	var certPEM, keyPEM []byte
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		switch block.Type {
		case "CERTIFICATE":
			certPEM = pem.EncodeToMemory(block)
		case "EC PRIVATE KEY", "PRIVATE KEY":
			keyPEM = pem.EncodeToMemory(block)
		}
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return nil, fmt.Errorf("mitm CA bundle missing cert or key")
	}
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	if tlsCert.Leaf == nil {
		tlsCert.Leaf, err = x509.ParseCertificate(tlsCert.Certificate[0])
		if err != nil {
			return nil, err
		}
	}
	key, ok := tlsCert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("mitm CA key must be ECDSA")
	}
	return &CA{cert: tlsCert.Leaf, key: key, tlsCert: tlsCert, cache: make(map[string]*tls.Certificate)}, nil
}

func generate() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ASP Egress MITM CA", Organization: []string{"ASP Lab"}},
		NotBefore:             time.Now().UTC().Add(-time.Hour),
		NotAfter:              time.Now().UTC().Add(10 * 365 * 24 * time.Hour),
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
	tlsCert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}
	return &CA{cert: cert, key: key, tlsCert: tlsCert, cache: make(map[string]*tls.Certificate)}, nil
}

func (c *CA) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw})
}

func (c *CA) KeyPEM() []byte {
	b, _ := x509.MarshalECPrivateKey(c.key)
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: b})
}

// CertificateForHost returns a leaf cert for hostname (cached).
func (c *CA) CertificateForHost(host string) (*tls.Certificate, error) {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return nil, fmt.Errorf("empty host")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cert, ok := c.cache[host]; ok {
		return cert, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().UTC().Add(-time.Hour),
		NotAfter:     time.Now().UTC().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	cert := &tls.Certificate{Certificate: [][]byte{der, c.cert.Raw}, PrivateKey: key, Leaf: leaf}
	c.cache[host] = cert
	return cert, nil
}
