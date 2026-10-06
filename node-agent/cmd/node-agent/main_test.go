package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckAgentListen(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:9100": true,
		"[::1]:9100":     true,
		"localhost:9100": true,
		"0.0.0.0:9100":   false,
		":9100":          false,
		"10.0.0.5:9100":  false,
		"no-port":        false,
	} {
		if err := checkAgentListen(addr, false); (err == nil) != ok {
			t.Errorf("checkAgentListen(%q) = %v, want ok=%v", addr, err, ok)
		}
	}
	if err := checkAgentListen("0.0.0.0:9100", true); err != nil {
		t.Errorf("--insecure-agent-listen must allow any address: %v", err)
	}
}

func TestDefaultTLSEndpoint(t *testing.T) {
	if got := defaultTLSEndpoint("10.0.0.5:9443"); got != "https://10.0.0.5:9443" {
		t.Errorf("explicit host: %s", got)
	}
	host, _ := os.Hostname()
	for _, addr := range []string{"0.0.0.0:9443", ":9443", "[::]:9443"} {
		if got := defaultTLSEndpoint(addr); !strings.HasPrefix(got, "https://") || !strings.Contains(got, host) || !strings.HasSuffix(got, ":9443") {
			t.Errorf("defaultTLSEndpoint(%q) = %s, want the hostname %s", addr, got, host)
		}
	}
}

func TestCertNodeIDReadsTheEnrolledCertificate(t *testing.T) {
	dir := t.TempDir()
	if got := certNodeID(dir); got != "" {
		t.Fatalf("no certificate: got %q", got)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "node-7", OrganizationalUnit: []string{"nodes"}},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "client.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := certNodeID(dir); got != "node-7" {
		t.Fatalf("certNodeID = %q, want node-7", got)
	}
}

func TestRegisterRequestReportsCapacityAndWork(t *testing.T) {
	cfg := config{NodeID: "n1", Endpoint: "https://n1:9443", CapacityCPU: 16, CapacityMemMiB: 60000, MaxSandboxes: 10, LocalNetDial: "203.0.113.10", Reconcile: true}
	req := registerRequest(cfg)
	if req.CapacityCPU != 16 || req.CapacityMemMiB != 60000 || req.MaxSandboxes != 10 || req.LocalNetDial != "203.0.113.10" ||
		req.AgentEndpoint != "https://n1:9443" || req.AcceptsWork == nil || !*req.AcceptsWork {
		t.Fatalf("register request: %+v", req)
	}
	cfg.Reconcile = false
	if req := registerRequest(cfg); *req.AcceptsWork {
		t.Fatal("an agent without --reconcile must not accept work")
	}
}
