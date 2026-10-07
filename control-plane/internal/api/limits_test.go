package api

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/attest"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/oidc"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/pki"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// newLimitsEnv is a server with every feature the body-reading routes need
// configured (CA, OIDC, attestation) behind the API-key middleware, with a
// platform key, so each route gets as far as reading its body.
func newLimitsEnv(t *testing.T) (h http.Handler, mem *backend, sandboxID string) {
	t.Helper()
	t.Setenv("ASP_AUTO_PROVISION", "0")
	t.Setenv("ASP_NODE_BOOTSTRAP_TOKEN", "boot-secret")
	ca, err := pki.GenerateCA("test", pki.DefaultNodeTTL)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	mem = newTestStore(t, "n1")
	if _, err := mem.EnsureAPIKey(context.Background(), "default", "ops", store.APIKeyScopePlatform, "asp_ops", store.HashAPIKeySecret("platform-key")); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(mem)
	srv.CA = ca
	srv.OIDC = oidc.NewSignerFromKey(rsaKey, "http://issuer.test")
	srv.Attestor = attest.NewSoftwareAttestorFromKey(ecKey)
	sandboxID = newPlacedSandbox(t, mem)
	runSandbox(t, mem, sandboxID)
	return AuthMiddleware(mem, AuthConfig{})(testMux(srv)), mem, sandboxID
}

// Every route that reads a body answers 413 above its limit, before it acts.
func TestOversizedBodiesAreRejectedWith413(t *testing.T) {
	h, _, id := newLimitsEnv(t)

	cases := []struct {
		name, method, path string
		limit              int
	}{
		{"create", "POST", "/v1/sandboxes", maxBodyBytes},
		{"exec", "POST", "/v1/sandboxes/" + id + "/exec", maxBodyBytes},
		{"exec stdin", "POST", "/v1/sandboxes/" + id + "/exec/stdin", maxBodyBytes},
		{"claim", "POST", "/v1/sandboxes/" + id + "/claim", maxSmallBodyBytes},
		{"status", "POST", "/v1/sandboxes/" + id + "/status", maxBodyBytes},
		{"attest", "POST", "/v1/sandboxes/" + id + "/attest", maxSmallBodyBytes},
		{"attestation verify", "POST", "/v1/attestation/verify", maxSmallBodyBytes},
		{"tenant egress", "PUT", "/v1/tenants/t/egress", maxBodyBytes},
		{"tenant egress check", "POST", "/v1/tenants/t/egress/check", maxBodyBytes},
		{"enroll", "POST", "/v1/nodes/enroll", maxSmallBodyBytes},
		{"enroll token", "POST", "/v1/nodes/enroll-tokens", maxSmallBodyBytes},
		{"register", "POST", "/v1/nodes/register", maxSmallBodyBytes},
		{"heartbeat", "POST", "/v1/nodes/n1/heartbeat", maxHeartbeatBytes},
		{"oidc mint", "POST", "/v1/internal/oidc/token", maxSmallBodyBytes},
		{"local-net heartbeat", "POST", "/v1/sandboxes/" + id + "/local-net/heartbeat", maxBodyBytes},
		{"local-net node-public", "POST", "/v1/sandboxes/" + id + "/local-net/node-public", maxBodyBytes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// One byte over the limit is enough; it is not valid JSON, so a
			// handler that read it anyway would answer 400 instead.
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(strings.Repeat("a", tc.limit+1)))
			req.Header.Set("Authorization", "Bearer platform-key")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413: %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "too large") {
				t.Fatalf("body = %q", rr.Body.String())
			}
		})
	}
}

// A body at the limit is still read: the optional heartbeat body is the one
// with a limit small enough to build by hand.
func TestBodyAtTheLimitIsAccepted(t *testing.T) {
	h, _, _ := newLimitsEnv(t)
	pad := maxHeartbeatBytes - len(`{"disk_free_mib":7,"pad":""}`)
	body := `{"disk_free_mib":7,"pad":"` + strings.Repeat("x", pad) + `"}`
	if len(body) != maxHeartbeatBytes {
		t.Fatalf("test body is %d bytes", len(body))
	}
	req := httptest.NewRequest("POST", "/v1/nodes/n1/heartbeat", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer platform-key")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK && rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
}

// countingReader yields n bytes of 'a' and counts what was read.
type countingReader struct{ n, max int64 }

func (c *countingReader) Read(p []byte) (int, error) {
	if c.n >= c.max {
		return 0, io.EOF
	}
	k := int64(len(p))
	if k > c.max-c.n {
		k = c.max - c.n
	}
	for i := int64(0); i < k; i++ {
		p[i] = 'a'
	}
	c.n += k
	return int(k), nil
}

// The server stops reading at the limit instead of draining a huge body.
func TestOversizedBodyIsNotReadToTheEnd(t *testing.T) {
	h, _, _ := newLimitsEnv(t)
	body := &countingReader{max: 200 << 20}
	req := httptest.NewRequest("POST", "/v1/nodes/enroll", body)
	req.Header.Set("Authorization", "Bearer boot-secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	if body.n > maxSmallBodyBytes+1 {
		t.Fatalf("read %d bytes of a body limited to %d", body.n, maxSmallBodyBytes)
	}
}

// A client that sends headers and then stalls does not hold the handler.
func TestSlowBodyTimesOut(t *testing.T) {
	old := bodyReadTimeout
	bodyReadTimeout = 200 * time.Millisecond
	defer func() { bodyReadTimeout = old }()

	srv := httptest.NewServer(testMux(NewServer(newBackend(t))))
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Announces 100 bytes, sends one, then waits.
	_, _ = conn.Write([]byte("POST /v1/nodes/register HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{"))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer to a stalled body: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("answered after %s", d)
	}
}
