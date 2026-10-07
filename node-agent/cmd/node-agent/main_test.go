package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/cpclient"
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

// writeNodeCert writes a self-signed node certificate for cn to dir/client.crt.
func writeNodeCert(t *testing.T, dir, cn string, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn, OrganizationalUnit: []string{"nodes"}},
		NotBefore:    notAfter.Add(-2 * time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "client.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCertNodeIDReadsTheEnrolledCertificate(t *testing.T) {
	dir := t.TempDir()
	if got := certNodeID(dir); got != "" {
		t.Fatalf("no certificate: got %q", got)
	}
	writeNodeCert(t, dir, "node-7", time.Now().Add(time.Hour))
	if got := certNodeID(dir); got != "node-7" {
		t.Fatalf("certNodeID = %q, want node-7", got)
	}
}

// An agent restarted with --enroll keeps its certificate when the control
// plane says the node is enrolled already.
func TestEnrollKeepsTheCertificateOfAnEnrolledNode(t *testing.T) {
	status := http.StatusConflict
	var gotAuth string
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if status != http.StatusCreated {
			http.Error(w, `{"error":"node is enrolled and not revoked"}`, status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"node_id": "node-7", "client_cert_pem": "C", "client_key_pem": "K", "ca_cert_pem": "A"})
	}))
	defer cp.Close()
	c := cpclient.New(cp.URL, cp.Client())
	now := time.Now()
	dir := t.TempDir()
	cfg := config{NodeID: "node-7", CertDir: dir, BootstrapToken: "boot", AgentListen: "127.0.0.1:9100"}

	if _, err := enrollNode(context.Background(), cfg, c, now); err == nil {
		t.Fatal("409 without a certificate in cert-dir must fail")
	}
	writeNodeCert(t, dir, "node-7", now.Add(time.Hour))
	if id, err := enrollNode(context.Background(), cfg, c, now); err != nil || id != "node-7" {
		t.Fatalf("409 with this node's certificate: id=%q err=%v", id, err)
	}
	if _, err := enrollNode(context.Background(), cfg, c, now.Add(2*time.Hour)); err == nil {
		t.Fatal("an expired certificate must not be kept")
	}
	other := cfg
	other.NodeID = "node-8"
	if _, err := enrollNode(context.Background(), other, c, now); err == nil {
		t.Fatal("another node's certificate must not be kept")
	}
	status = http.StatusUnauthorized
	if _, err := enrollNode(context.Background(), cfg, c, now); err == nil {
		t.Fatal("only a 409 falls back to the existing certificate")
	}

	// The enroll token wins over the bootstrap token, and a 201 writes the certificates.
	status = http.StatusCreated
	cfg.EnrollToken = "asp_enroll_x"
	if id, err := enrollNode(context.Background(), cfg, c, now); err != nil || id != "node-7" {
		t.Fatalf("enroll: id=%q err=%v", id, err)
	}
	if gotAuth != "Bearer asp_enroll_x" {
		t.Fatalf("credential sent: %q", gotAuth)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "client.crt")); string(b) != "C" {
		t.Fatalf("client.crt not replaced: %q", b)
	}
}

func TestRegisterRequestReportsCapacityAndWork(t *testing.T) {
	cfg := config{NodeID: "n1", Endpoint: "https://n1:9443", CapacityCPU: 16, CapacityMemMiB: 60000, MaxSandboxes: 10, LocalNetDial: "203.0.113.10", Reconcile: true, InstanceID: "abc"}
	req := registerRequest(cfg)
	if req.AgentInstanceID != "abc" {
		t.Fatalf("instance id: %q", req.AgentInstanceID)
	}
	if a, b := newInstanceID(), newInstanceID(); a == b || len(a) != 32 {
		t.Fatalf("instance ids must be random: %q %q", a, b)
	}
	if req.CapacityCPU != 16 || req.CapacityMemMiB != 60000 || req.MaxSandboxes != 10 || req.LocalNetDial != "203.0.113.10" ||
		req.AgentEndpoint != "https://n1:9443" || req.AcceptsWork == nil || !*req.AcceptsWork {
		t.Fatalf("register request: %+v", req)
	}
	cfg.Reconcile = false
	if req := registerRequest(cfg); *req.AcceptsWork {
		t.Fatal("an agent without --reconcile must not accept work")
	}
}

func TestTapManagerSoftFailsOnlyInDryRun(t *testing.T) {
	if !tapManager(config{DryRun: true}).SoftFail {
		t.Fatal("a dry-run agent may run without TAPs")
	}
	if tapManager(config{}).SoftFail {
		t.Fatal("a real agent must not boot a VM whose TAP failed")
	}
}

// --disk-min-free-mib: a value is used as given; -1 is twice the base image.
func TestDiskMinFreeMiB(t *testing.T) {
	base := filepath.Join(t.TempDir(), "rootfs.img")
	if err := os.WriteFile(base, make([]byte, 3<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		flag  int
		image string
		want  int64
	}{
		{500, base, 500},
		{0, base, 0},
		{-1, base, 6},
		{-1, filepath.Join(t.TempDir(), "missing.img"), 0},
	} {
		if got := diskMinFreeMiB(tc.flag, tc.image); got != tc.want {
			t.Errorf("diskMinFreeMiB(%d, %s)=%d, want %d", tc.flag, filepath.Base(tc.image), got, tc.want)
		}
	}
}
