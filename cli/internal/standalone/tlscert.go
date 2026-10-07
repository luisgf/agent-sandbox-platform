package standalone

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"sort"
	"strings"
	"time"
)

// Names are the names a certificate has to be valid for.
type Names struct {
	DNS []string
	IPs []net.IP
}

// NamesFor lists what the control plane's certificate must cover: the loopback, this host's
// name, the address it listens on (every address of the host when it listens on all of them),
// and whatever the operator adds (--tls-san), as host names or addresses.
func NamesFor(listenHost, hostname string, extra []string, addrs func() ([]net.Addr, error)) (Names, error) {
	dns := map[string]bool{"localhost": true}
	ips := map[string]net.IP{}
	addIP := func(ip net.IP) {
		if ip != nil {
			ips[ip.String()] = ip
		}
	}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if ip := net.ParseIP(strings.Trim(name, "[]")); ip != nil {
			addIP(ip)
		} else {
			dns[strings.ToLower(name)] = true
		}
	}
	addIP(net.ParseIP("127.0.0.1"))
	addIP(net.ParseIP("::1"))
	add(hostname)
	for _, e := range extra {
		add(e)
	}
	switch h := strings.Trim(listenHost, "[]"); h {
	case "", "0.0.0.0", "::":
		// All of them: a node on another address reaches it by that one.
		if addrs == nil {
			addrs = net.InterfaceAddrs
		}
		list, err := addrs()
		if err != nil {
			return Names{}, fmt.Errorf("list this host's addresses: %w", err)
		}
		for _, a := range list {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLinkLocalUnicast() {
				addIP(ipn.IP)
			}
		}
	default:
		add(h)
	}
	var out Names
	for d := range dns {
		out.DNS = append(out.DNS, d)
	}
	sort.Strings(out.DNS)
	for _, ip := range ips {
		out.IPs = append(out.IPs, ip)
	}
	sort.Slice(out.IPs, func(i, j int) bool { return out.IPs[i].String() < out.IPs[j].String() })
	return out, nil
}

// Covers reports whether cert is valid for every name, and for another month.
func (n Names) Covers(cert *x509.Certificate, now time.Time) bool {
	if !cert.NotAfter.After(now.Add(30 * 24 * time.Hour)) {
		return false
	}
	have := map[string]bool{}
	for _, d := range cert.DNSNames {
		have[strings.ToLower(d)] = true
	}
	for _, d := range n.DNS {
		if !have[d] {
			return false
		}
	}
	haveIP := map[string]bool{}
	for _, ip := range cert.IPAddresses {
		haveIP[ip.String()] = true
	}
	for _, ip := range n.IPs {
		if !haveIP[ip.String()] {
			return false
		}
	}
	return true
}

// EnsureCertificate makes sure certPath and keyPath hold a certificate valid for names:
// the one that is there is kept if it covers them, and replaced if it does not (a name was
// added), which the caller reports, because a node that trusts the old certificate has to be
// given the new one.
func EnsureCertificate(certPath, keyPath string, names Names, now time.Time) (replaced bool, err error) {
	if cert, err := LoadCertificate(certPath); err == nil {
		if _, kerr := os.Stat(keyPath); kerr == nil && names.Covers(cert, now) {
			return false, nil
		}
		replaced = true
	}
	certPEM, keyPEM, err := newSelfSigned(names, now)
	if err != nil {
		return false, err
	}
	// The key first, private from its first byte; the certificate after it, so that a reader that
	// finds the certificate finds the key.
	if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return false, err
	}
	if err := writeFileAtomic(certPath, certPEM, 0o644); err != nil {
		return false, err
	}
	return replaced, nil
}

// LoadCertificate reads the first certificate of a PEM file.
func LoadCertificate(path string) (*x509.Certificate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			return nil, fmt.Errorf("%s has no certificate", path)
		}
		if block.Type == "CERTIFICATE" {
			return x509.ParseCertificate(block.Bytes)
		}
	}
}

// Fingerprint is the SHA-256 of a certificate, as lower-case hex.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

func newSelfSigned(names Names, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	if err != nil {
		return nil, nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "asp-server", Organization: []string{"Agent Sandbox Platform"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              names.DNS,
		IPAddresses:           names.IPs,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

// writeFileAtomic writes beside path and renames over it, so a reader sees the old file or
// the new one and never half of it.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(dirOf(path), ".tmp-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func dirOf(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		if i == 0 {
			return "/"
		}
		return path[:i]
	}
	return "."
}
