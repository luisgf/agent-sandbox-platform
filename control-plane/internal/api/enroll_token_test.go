package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/pki"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

type enrollEnv struct {
	t   *testing.T
	h   http.Handler
	mem *store.MemoryStore
}

func newEnrollEnv(t *testing.T) *enrollEnv {
	t.Helper()
	t.Setenv("ASP_NODE_BOOTSTRAP_TOKEN", "boot-secret")
	ca, err := pki.GenerateCA("test", pki.DefaultNodeTTL)
	if err != nil {
		t.Fatal(err)
	}
	mem := store.NewMemoryStore()
	if _, err := mem.EnsureAPIKey("default", "ops", store.APIKeyScopePlatform, "asp_ops", store.HashAPIKeySecret("platform-key")); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.EnsureAPIKey("t1", "tenant", store.APIKeyScopeTenant, "asp_tenant", store.HashAPIKeySecret("tenant-key")); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(mem)
	srv.CA = ca
	return &enrollEnv{t: t, h: AuthMiddleware(mem, AuthConfig{})(testMux(srv)), mem: mem}
}

func (e *enrollEnv) do(method, path, bearer, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr
}

// issue returns a new enroll token, pinned to nodeID when it is not empty.
func (e *enrollEnv) issue(nodeID string) string {
	e.t.Helper()
	rr := e.do(http.MethodPost, "/v1/nodes/enroll-tokens", "platform-key", fmt.Sprintf(`{"node_id":%q}`, nodeID))
	if rr.Code != http.StatusCreated {
		e.t.Fatalf("issue enroll token: %d %s", rr.Code, rr.Body.String())
	}
	var out enrollTokenResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		e.t.Fatal(err)
	}
	if !strings.HasPrefix(out.Token, enrollTokenPrefix) || out.NodeID != nodeID || !out.ExpiresAt.After(time.Now()) {
		e.t.Fatalf("enroll token response: %+v", out)
	}
	return out.Token
}

func (e *enrollEnv) enroll(nodeID, bearer string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(http.MethodPost, "/v1/nodes/enroll", bearer, fmt.Sprintf(`{"id":%q,"agent_endpoint":"http://127.0.0.1:9100"}`, nodeID))
}

func fingerprintOf(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var er enrollResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &er); err != nil {
		t.Fatal(err)
	}
	return er.CertFingerprint
}

func TestEnrollTokensAreIssuedByAdminsOnly(t *testing.T) {
	e := newEnrollEnv(t)
	for _, tc := range []struct {
		bearer string
		want   int
	}{
		{"tenant-key", http.StatusForbidden},
		{"boot-secret", http.StatusUnauthorized},
		{"", http.StatusUnauthorized},
	} {
		if rr := e.do(http.MethodPost, "/v1/nodes/enroll-tokens", tc.bearer, `{}`); rr.Code != tc.want {
			t.Errorf("issue with %q: want %d, got %d %s", tc.bearer, tc.want, rr.Code, rr.Body.String())
		}
	}
	for _, body := range []string{`{"node_id":"bad id/"}`, `{"ttl_seconds":999999999}`, `{"node_id":`} {
		if rr := e.do(http.MethodPost, "/v1/nodes/enroll-tokens", "platform-key", body); rr.Code != http.StatusBadRequest {
			t.Errorf("issue %s: want 400, got %d", body, rr.Code)
		}
	}
	e.issue("")
}

// The open lab has no admin: enroll tokens need a credential there too, and
// nodes keep enrolling with the bootstrap token.
func TestEnrollTokensInTheOpenLab(t *testing.T) {
	h, _ := nodeAdminServer(t, AuthConfig{})
	req := httptest.NewRequest(http.MethodPost, "/v1/nodes/enroll-tokens", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("open lab enroll token: want 401, got %d", rr.Code)
	}
}

func TestEnrollTokenIsSingleUse(t *testing.T) {
	e := newEnrollEnv(t)
	tok := e.issue("")
	if rr := e.enroll("new-1", tok); rr.Code != http.StatusCreated {
		t.Fatalf("enroll with a fresh token: %d %s", rr.Code, rr.Body.String())
	}
	if rr := e.enroll("new-2", tok); rr.Code != http.StatusUnauthorized {
		t.Fatalf("second use: want 401, got %d %s", rr.Code, rr.Body.String())
	}
	if rr := e.enroll("new-3", "asp_enroll_made-up"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unknown token: want 401, got %d", rr.Code)
	}
	if rr := e.enroll("new-4", "wrong-bootstrap"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bootstrap token: want 401, got %d", rr.Code)
	}
}

func TestEnrollTokenExpires(t *testing.T) {
	e := newEnrollEnv(t)
	raw := enrollTokenPrefix + "expired"
	if err := e.mem.CreateEnrollToken(store.EnrollToken{Hash: store.HashEnrollToken(raw), ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if rr := e.enroll("late", raw); rr.Code != http.StatusUnauthorized {
		t.Fatalf("expired token: want 401, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestPinnedEnrollToken(t *testing.T) {
	e := newEnrollEnv(t)
	tok := e.issue("node-a")
	if rr := e.enroll("node-b", tok); rr.Code != http.StatusForbidden {
		t.Fatalf("token pinned to node-a used by node-b: want 403, got %d %s", rr.Code, rr.Body.String())
	}
	if rr := e.enroll("node-a", tok); rr.Code != http.StatusCreated {
		t.Fatalf("pinned token for its node: %d %s", rr.Code, rr.Body.String())
	}
}

// Whoever holds the shared bootstrap token must not take over a live node.
func TestBootstrapTokenCannotTakeOverALiveNode(t *testing.T) {
	e := newEnrollEnv(t)
	first := e.enroll("live", "boot-secret")
	if first.Code != http.StatusCreated {
		t.Fatalf("first enroll: %d %s", first.Code, first.Body.String())
	}
	oldFP := fingerprintOf(t, first)

	rr := e.enroll("live", "boot-secret")
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "asp node enroll-token --node-id live") {
		t.Fatalf("bootstrap re-enroll of a live node: want 409 naming the fix, got %d %s", rr.Code, rr.Body.String())
	}
	unpinned := e.issue("")
	if rr := e.enroll("live", unpinned); rr.Code != http.StatusConflict {
		t.Fatalf("unpinned token on a live node: want 409, got %d", rr.Code)
	}
	if revoked, _ := e.mem.IsCertRevoked(oldFP); revoked {
		t.Fatal("a refused enrollment revoked the node's certificate")
	}
	// The refused attempt did not use the token up.
	if rr := e.enroll("other", unpinned); rr.Code != http.StatusCreated {
		t.Fatalf("unpinned token after a refused attempt: %d %s", rr.Code, rr.Body.String())
	}

	// An admin re-keys the node with a token pinned to it.
	rekey := e.enroll("live", e.issue("live"))
	if rekey.Code != http.StatusCreated {
		t.Fatalf("pinned re-enroll: %d %s", rekey.Code, rekey.Body.String())
	}
	if revoked, _ := e.mem.IsCertRevoked(oldFP); !revoked {
		t.Fatal("re-keying must revoke the previous certificate")
	}
}

func TestBootstrapTokenReEnrollsARevokedNode(t *testing.T) {
	e := newEnrollEnv(t)
	if rr := e.enroll("gone", "boot-secret"); rr.Code != http.StatusCreated {
		t.Fatalf("enroll: %d", rr.Code)
	}
	if _, err := e.mem.RevokeNode("gone"); err != nil {
		t.Fatal(err)
	}
	if rr := e.enroll("gone", "boot-secret"); rr.Code != http.StatusCreated {
		t.Fatalf("bootstrap re-enroll of a revoked node: want 201, got %d %s", rr.Code, rr.Body.String())
	}
	// A node that registered without a certificate can get its first one.
	if _, err := e.mem.RegisterNode(store.RegisterNodeInput{ID: "plain", AgentEndpoint: "http://127.0.0.1:9100"}); err != nil {
		t.Fatal(err)
	}
	if rr := e.enroll("plain", "boot-secret"); rr.Code != http.StatusCreated {
		t.Fatalf("first certificate for a registered node: %d %s", rr.Code, rr.Body.String())
	}
}

// Two enrollments racing for one token: exactly one wins.
func TestEnrollTokenConcurrentUse(t *testing.T) {
	e := newEnrollEnv(t)
	tok := e.issue("")
	codes := make(chan int, 8)
	for i := 0; i < 8; i++ {
		go func(i int) { codes <- e.enroll(fmt.Sprintf("race-%d", i), tok).Code }(i)
	}
	won := 0
	for i := 0; i < 8; i++ {
		switch c := <-codes; c {
		case http.StatusCreated:
			won++
		case http.StatusUnauthorized:
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	if won != 1 {
		t.Fatalf("%d enrollments used one token", won)
	}
}
