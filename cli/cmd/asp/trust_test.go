package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tlsListServer(t *testing.T, auth *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		if auth != nil {
			*auth = r.Header.Get("Authorization")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []any{}})
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func writeServerCert(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tls.crt")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A control plane with a certificate of its own (the self-signed one asp-server makes) is reached
// by naming the certificate, not by turning verification off.
func TestTheCAFileLetsTheCLITrustAControlPlane(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("ASP_CA_FILE", "")
	srv := tlsListServer(t, nil)
	args := []string{"sandbox", "list", "--tenant", "default", "--control-plane-url", srv.URL}

	var stdout, stderr strings.Builder
	if code := run(args, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "certificate") {
		t.Fatalf("an unknown certificate was accepted: exit %d, stderr %q", code, stderr.String())
	}
	t.Setenv("ASP_CA_FILE", writeServerCert(t, srv))
	stderr.Reset()
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("with the certificate named: exit %d, stderr %q", code, stderr.String())
	}
	t.Setenv("ASP_CA_FILE", filepath.Join(t.TempDir(), "none.crt"))
	stderr.Reset()
	if code := run(args, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "ASP_CA_FILE") {
		t.Fatalf("a missing CA file: exit %d, stderr %q", code, stderr.String())
	}
	bad := filepath.Join(t.TempDir(), "bad.crt")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASP_CA_FILE", bad)
	stderr.Reset()
	if code := run(args, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "no PEM certificate") {
		t.Fatalf("a CA file with no certificate: exit %d, stderr %q", code, stderr.String())
	}
}

func TestTheAPIKeyFile(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("ASP_CA_FILE", "")
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []any{}})
	}))
	defer srv.Close()
	args := []string{"sandbox", "list", "--tenant", "default", "--control-plane-url", srv.URL}
	keyFile := filepath.Join(t.TempDir(), "admin-key")
	if err := os.WriteFile(keyFile, []byte("key-from-the-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	t.Setenv("ASP_API_KEY_FILE", keyFile)
	if code := run(args, &stdout, &stderr); code != 0 || auth != "Bearer key-from-the-file" {
		t.Fatalf("key file: exit %d, sent %q, stderr %q", code, auth, stderr.String())
	}
	// The key itself, in the environment or as a flag, wins over the file.
	t.Setenv("ASP_API_KEY", "key-from-the-environment")
	if code := run(args, &stdout, &stderr); code != 0 || auth != "Bearer key-from-the-environment" {
		t.Fatalf("environment over file: exit %d, sent %q", code, auth)
	}
	if code := run(append(args, "--api-key", "key-from-the-flag"), &stdout, &stderr); code != 0 || auth != "Bearer key-from-the-flag" {
		t.Fatalf("flag over file: exit %d, sent %q", code, auth)
	}

	t.Setenv("ASP_API_KEY", "")
	t.Setenv("ASP_API_KEY_FILE", filepath.Join(t.TempDir(), "none"))
	stderr.Reset()
	if code := run(args, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "ASP_API_KEY_FILE") {
		t.Fatalf("a missing key file: exit %d, stderr %q", code, stderr.String())
	}
	if os.Geteuid() != 0 {
		locked := filepath.Join(t.TempDir(), "locked-key")
		if err := os.WriteFile(locked, []byte("k"), 0o000); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ASP_API_KEY_FILE", locked)
		stderr.Reset()
		if code := run(args, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "member of the group") {
			t.Fatalf("an unreadable key file: exit %d, stderr %q", code, stderr.String())
		}
	}
}

// asp node enroll-token says how to put the node behind this control plane, with the
// fingerprint of the certificate the CLI trusts so that the new host can check it.
func TestEnrollTokenSaysHowToJoinTheNode(t *testing.T) {
	clearAuthEnv(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/nodes/enroll-tokens", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "asp_enroll_xyz", "node_id": req["node_id"], "expires_at": time.Now().Add(time.Hour)})
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	t.Setenv("ASP_CA_FILE", writeServerCert(t, srv))
	sum := sha256.Sum256(srv.Certificate().Raw)

	var stdout, stderr strings.Builder
	if code := run([]string{"node", "enroll-token", "--control-plane-url", srv.URL, "--node-id", "node-b"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	for _, want := range []string{
		"INSTALL_ASP_ROLE=agent", "INSTALL_ASP_SERVER=" + srv.URL, "INSTALL_ASP_TOKEN=asp_enroll_xyz",
		"INSTALL_ASP_NODE_ID=node-b", "INSTALL_ASP_CA_SHA256=" + hex.EncodeToString(sum[:]),
		"install.sh | sudo", "--enroll-token=<token> --node-id=node-b",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("the hint lacks %q:\n%s", want, stderr.String())
		}
	}
	if strings.TrimSpace(stdout.String()) != "asp_enroll_xyz" {
		t.Errorf("stdout must stay the token alone: %q", stdout.String())
	}

	// A loopback URL is no use to another host, and the hint says so.
	t.Setenv("ASP_CA_FILE", "")
	plain := httptest.NewServer(mux)
	defer plain.Close()
	stderr.Reset()
	if code := run([]string{"node", "enroll-token", "--control-plane-url", plain.URL}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "loopback") || strings.Contains(stderr.String(), "INSTALL_ASP_CA_SHA256") {
		t.Errorf("loopback hint:\n%s", stderr.String())
	}
}
