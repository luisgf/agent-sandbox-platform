package cpclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// serverCAFile writes the httptest server's certificate, which signs itself.
func serverCAFile(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cp-ca.pem")
	b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// selfSignedPEM returns a throwaway certificate and key.
func selfSignedPEM(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func TestEnrollTrustsTheGivenControlPlaneCA(t *testing.T) {
	var got EnrollRequest
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/nodes/enroll" || r.Header.Get("Authorization") != "Bearer boot" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(EnrollResponse{NodeID: "n1", ClientCertPEM: "c", ClientKeyPEM: "k", CACertPEM: "ca"})
	}))
	defer srv.Close()

	untrusted, err := NewEnrollHTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(srv.URL, untrusted).Enroll(context.Background(), "boot", EnrollRequest{ID: "n1"}); err == nil {
		t.Fatal("enroll must fail when the control plane's certificate is not trusted")
	}

	trusted, err := NewEnrollHTTPClient(serverCAFile(t, srv))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := New(srv.URL, trusted).Enroll(context.Background(), "boot", EnrollRequest{ID: "n1", AgentEndpoint: "https://n1:9443"})
	if err != nil {
		t.Fatalf("enroll with --control-plane-ca: %v", err)
	}
	if resp.NodeID != "n1" || got.AgentEndpoint != "https://n1:9443" {
		t.Fatalf("resp=%+v request=%+v", resp, got)
	}
}

func TestLoadMTLSClientPrefersTheControlPlaneCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	certDir := t.TempDir()
	clientCert, clientKey := selfSignedPEM(t, "n1")
	enrollCA, _ := selfSignedPEM(t, "enrollment-ca") // does not sign the server certificate
	for name, b := range map[string][]byte{"client.crt": clientCert, "client.key": clientKey, "ca.crt": enrollCA} {
		if err := os.WriteFile(filepath.Join(certDir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	c, mtls, err := LoadMTLSClient(certDir, false, "")
	if err != nil || !mtls {
		t.Fatalf("mtls=%v err=%v", mtls, err)
	}
	if _, err := c.Get(srv.URL); err == nil {
		t.Fatal("without --control-plane-ca the enrollment CA must be the only root")
	}

	c, mtls, err = LoadMTLSClient(certDir, false, serverCAFile(t, srv))
	if err != nil || !mtls {
		t.Fatalf("mtls=%v err=%v", mtls, err)
	}
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("with --control-plane-ca: %v", err)
	}
	resp.Body.Close()
}

func TestLoadMTLSClientWithoutCertificates(t *testing.T) {
	c, mtls, err := LoadMTLSClient(t.TempDir(), false, "")
	if err != nil || mtls || c == nil {
		t.Fatalf("no certs, not forced: client=%v mtls=%v err=%v", c, mtls, err)
	}
	if _, _, err := LoadMTLSClient(t.TempDir(), true, ""); err == nil {
		t.Fatal("ASP_MTLS=1 without certificates must fail")
	}
	if _, err := NewEnrollHTTPClient(filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Fatal("a missing --control-plane-ca file must fail")
	}
}

func TestStatusErrorsAreTyped(t *testing.T) {
	var gotRegister map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/nodes/n1/heartbeat":
			http.Error(w, `{"error":"node not found"}`, http.StatusNotFound)
		case "/v1/sandboxes/s1/status":
			http.Error(w, `{"error":"conflict: cannot move sandbox from failed to running"}`, http.StatusConflict)
		case "/v1/nodes/register":
			_ = json.NewDecoder(r.Body).Decode(&gotRegister)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, srv.Client())

	err := c.Heartbeat(context.Background(), "n1", HeartbeatInfo{})
	if !IsNotFound(err) || IsConflict(err) || err.Error() != `heartbeat status 404: {"error":"node not found"}` {
		t.Fatalf("heartbeat 404: %v", err)
	}
	_, err = c.ReportStatus(context.Background(), "s1", "running", "booted")
	if !IsConflict(err) || IsNotFound(err) {
		t.Fatalf("status 409: %v", err)
	}

	no := false
	if err := c.Register(context.Background(), RegisterRequest{ID: "n1", CapacityCPU: 8, CapacityMemMiB: 16384, MaxSandboxes: 3, AcceptsWork: &no, LocalNetDial: "203.0.113.10"}); err != nil {
		t.Fatal(err)
	}
	if gotRegister["max_sandboxes"] != float64(3) || gotRegister["accepts_work"] != false || gotRegister["local_net_dial"] != "203.0.113.10" || gotRegister["capacity_cpu"] != float64(8) {
		t.Fatalf("register body: %v", gotRegister)
	}
}

// The assigned set distinguishes "nothing assigned" from a control plane
// that does not send it.
func TestListWorkAssignedSet(t *testing.T) {
	body := `{"sandboxes":[{"id":"s1","state":"requested"}],"assigned":["s1","s2"]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := New(srv.URL, srv.Client())
	for _, tc := range []struct {
		body     string
		assigned []string
		isNil    bool
	}{
		{`{"sandboxes":[{"id":"s1","state":"requested"}],"assigned":["s1","s2"]}`, []string{"s1", "s2"}, false},
		{`{"sandboxes":[],"assigned":[]}`, []string{}, false},
		{`{"sandboxes":[]}`, nil, true},
	} {
		body = tc.body
		work, err := c.ListWork(context.Background(), "n1")
		if err != nil {
			t.Fatal(err)
		}
		if (work.Assigned == nil) != tc.isNil || len(work.Assigned) != len(tc.assigned) {
			t.Fatalf("%s: assigned=%#v", tc.body, work.Assigned)
		}
		if work.Sandboxes == nil {
			t.Fatalf("%s: sandboxes must not be nil", tc.body)
		}
	}
}

// The retained list tells the node whether stopping keeps a sandbox's disk. An
// absent list is a control plane that predates it; an empty one is not.
func TestListWorkRetainedAndBootCount(t *testing.T) {
	body := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := New(srv.URL, srv.Client())
	for _, tc := range []struct {
		body     string
		retained []string
		retains  bool
	}{
		{`{"sandboxes":[],"assigned":[],"retained":["s3"]}`, []string{"s3"}, true},
		{`{"sandboxes":[],"assigned":[],"retained":[]}`, []string{}, true},
		{`{"sandboxes":[],"assigned":[]}`, nil, false},
	} {
		body = tc.body
		work, err := c.ListWork(context.Background(), "n1")
		if err != nil {
			t.Fatal(err)
		}
		if work.Retains() != tc.retains || len(work.Retained) != len(tc.retained) {
			t.Fatalf("%s: retained=%#v retains=%v", tc.body, work.Retained, work.Retains())
		}
	}
	body = `{"sandboxes":[{"id":"s1","state":"requested","boot_count":3},{"id":"s2","state":"requested"}]}`
	work, err := c.ListWork(context.Background(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	if work.Sandboxes[0].BootCount != 3 || work.Sandboxes[1].BootCount != 0 {
		t.Fatalf("boot counts: %+v", work.Sandboxes)
	}
}

// The heartbeat carries the free disk space when it is known, and no body when
// it is not, so a control plane that predates the field sees what it always did.
func TestHeartbeatCarriesFreeDisk(t *testing.T) {
	var body string
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body, contentType = string(raw), r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := New(srv.URL, srv.Client())

	if err := c.Heartbeat(context.Background(), "n1", HeartbeatInfo{}); err != nil {
		t.Fatal(err)
	}
	if body != "" || contentType != "" {
		t.Fatalf("a heartbeat without info must have no body: %q (%s)", body, contentType)
	}
	free := int64(123456)
	if err := c.Heartbeat(context.Background(), "n1", HeartbeatInfo{DiskFreeMiB: &free}); err != nil {
		t.Fatal(err)
	}
	if body != `{"disk_free_mib":123456}` || contentType != "application/json" {
		t.Fatalf("body=%q content-type=%q", body, contentType)
	}
}
