package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clearProdEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"ASP_CLIENT_CA", "ASP_TLS_CERT", "DATABASE_URL", "ASP_IDP_REQUIRED", EnvAllowTmpKeys,
		"ASP_CA_CERT", "ASP_CA_KEY", "ASP_OIDC_KEY", "ASP_ATTEST_KEY"} {
		t.Setenv(k, "")
	}
}

func TestProdMode(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		prod bool
	}{
		{map[string]string{}, false},
		{map[string]string{"ASP_CLIENT_CA": "/etc/asp/ca.pem"}, true},
		{map[string]string{"ASP_TLS_CERT": "/etc/asp/tls.crt"}, true},
		{map[string]string{"DATABASE_URL": "postgres://x"}, true},
		{map[string]string{"ASP_IDP_REQUIRED": "1"}, true},
		{map[string]string{"ASP_IDP_REQUIRED": "0"}, false},
		{map[string]string{"ASP_REQUIRE_API_KEY": "1"}, false},
	} {
		clearProdEnv(t)
		for k, v := range tc.env {
			t.Setenv(k, v)
		}
		if prod, why := prodMode(); prod != tc.prod {
			t.Errorf("env %v: prod=%v (%s), want %v", tc.env, prod, why, tc.prod)
		}
	}
}

func TestInTempDir(t *testing.T) {
	for path, want := range map[string]bool{
		"/tmp/asp-dev-ca/ca.key":                    true,
		"/var/tmp/asp.pem":                          true,
		"/dev/shm/asp.pem":                          true,
		filepath.Join(os.TempDir(), "asp-oidc.pem"): true,
		"/var/lib/asp/ca.key":                       false,
		"/etc/asp/oidc.pem":                         false,
		"/tmpfoo/key.pem":                           false,
	} {
		if got := inTempDir(path); got != want {
			t.Errorf("inTempDir(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestCheckKeyLocations(t *testing.T) {
	safe := map[string]string{
		"ASP_CA_CERT": "/var/lib/asp/ca.crt", "ASP_CA_KEY": "/var/lib/asp/ca.key",
		"ASP_OIDC_KEY": "/var/lib/asp/oidc.pem", "ASP_ATTEST_KEY": "/var/lib/asp/attest.pem",
	}
	set := func(env map[string]string) {
		for k, v := range env {
			t.Setenv(k, v)
		}
	}

	// Lab: defaults in /tmp only warn.
	clearProdEnv(t)
	if err := checkKeyLocations(); err != nil {
		t.Fatalf("lab with default keys: %v", err)
	}

	// Production with the defaults: refused, naming every variable.
	clearProdEnv(t)
	t.Setenv("DATABASE_URL", "postgres://x")
	err := checkKeyLocations()
	var cfgErr configError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("production with keys in /tmp: want a configError, got %v", err)
	}
	for _, k := range []string{"ASP_CA_CERT", "ASP_CA_KEY", "ASP_OIDC_KEY", "ASP_ATTEST_KEY", "DATABASE_URL", EnvAllowTmpKeys} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("the refusal should name %s: %v", k, err)
		}
	}

	// One key left in /tmp is enough to refuse.
	set(safe)
	t.Setenv("ASP_OIDC_KEY", "/tmp/oidc.pem")
	if err := checkKeyLocations(); err == nil || !strings.Contains(err.Error(), "ASP_OIDC_KEY=/tmp/oidc.pem") || strings.Contains(err.Error(), "ASP_CA_KEY") {
		t.Fatalf("one key in /tmp: %v", err)
	}

	// Override.
	t.Setenv(EnvAllowTmpKeys, "1")
	if err := checkKeyLocations(); err != nil {
		t.Fatalf("%s=1: %v", EnvAllowTmpKeys, err)
	}

	// Persistent paths pass.
	clearProdEnv(t)
	t.Setenv("ASP_CLIENT_CA", "/etc/asp/ca.pem")
	set(safe)
	if err := checkKeyLocations(); err != nil {
		t.Fatalf("persistent keys: %v", err)
	}
}
