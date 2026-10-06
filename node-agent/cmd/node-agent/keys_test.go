package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNodeProdMode(t *testing.T) {
	withCert := t.TempDir()
	if err := os.WriteFile(filepath.Join(withCert, "client.crt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	for _, tc := range []struct {
		name string
		cfg  config
		prod bool
	}{
		{"lab without certificates", config{CertDir: empty}, false},
		{"agent TLS listener", config{CertDir: empty, AgentTLSListen: "0.0.0.0:9443"}, true},
		{"--mtls", config{CertDir: empty, MTLS: true}, true},
		{"enrolled certificate", config{CertDir: withCert}, true},
		{"dry-run with everything", config{CertDir: withCert, AgentTLSListen: "0.0.0.0:9443", MTLS: true, DryRun: true}, false},
	} {
		if prod, why := nodeProdMode(tc.cfg); prod != tc.prod {
			t.Errorf("%s: prod=%v (%s), want %v", tc.name, prod, why, tc.prod)
		}
	}
}

func TestCheckNodeKeyLocations(t *testing.T) {
	t.Setenv(envAllowTmpKeys, "")
	t.Setenv("ASP_ATTEST_KEY", "")
	tmpCerts := filepath.Join(os.TempDir(), "asp-node-certs")
	const persistent = "/var/lib/asp-test-not-created/node-certs"

	if err := checkNodeKeyLocations(config{CertDir: tmpCerts, DryRun: true}); err != nil {
		t.Fatalf("dry-run lab with certificates in tmp: %v", err)
	}
	prod := config{CertDir: tmpCerts, AgentTLSListen: "0.0.0.0:9443"}
	err := checkNodeKeyLocations(prod)
	if err == nil || !strings.Contains(err.Error(), "--cert-dir") || !strings.Contains(err.Error(), envAllowTmpKeys) {
		t.Fatalf("production node with cert-dir in tmp: %v", err)
	}
	t.Setenv(envAllowTmpKeys, "1")
	if err := checkNodeKeyLocations(prod); err != nil {
		t.Fatalf("%s=1: %v", envAllowTmpKeys, err)
	}
	t.Setenv(envAllowTmpKeys, "")

	// A production node with persistent certificates and no ASP_ATTEST_KEY is
	// fine: it signs attestations with its certificate key.
	ok := config{CertDir: persistent, AgentTLSListen: "0.0.0.0:9443"}
	if err := checkNodeKeyLocations(ok); err != nil {
		t.Fatalf("persistent cert-dir: %v", err)
	}
	t.Setenv("ASP_ATTEST_KEY", "/tmp/attest.pem")
	if err := checkNodeKeyLocations(ok); err == nil || !strings.Contains(err.Error(), "ASP_ATTEST_KEY=/tmp/attest.pem") {
		t.Fatalf("ASP_ATTEST_KEY in tmp: %v", err)
	}
	t.Setenv("ASP_ATTEST_KEY", "")

	mitm := ok
	mitm.EgressMITM = true
	if err := checkNodeKeyLocations(mitm); err == nil || !strings.Contains(err.Error(), "--egress-mitm-ca") {
		t.Fatalf("MITM CA defaulting to tmp: %v", err)
	}
	mitm.EgressMITMCA = "/etc/asp/mitm-ca.pem"
	if err := checkNodeKeyLocations(mitm); err != nil {
		t.Fatalf("persistent MITM CA: %v", err)
	}
}

func TestInTempDir(t *testing.T) {
	for path, want := range map[string]bool{
		"/tmp/asp-node-certs":                     true,
		"/var/tmp/asp.pem":                        true,
		filepath.Join(os.TempDir(), "a", "b.pem"): true,
		"/var/lib/asp/node-certs":                 false,
		"/tmpx/asp.pem":                           false,
	} {
		if got := inTempDir(path); got != want {
			t.Errorf("inTempDir(%q) = %v, want %v", path, got, want)
		}
	}
}
