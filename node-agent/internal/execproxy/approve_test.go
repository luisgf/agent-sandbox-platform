package execproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/egress"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/sshagent"
)

func approve(t *testing.T, s *Server, body string) (int, map[string]any) {
	t.Helper()
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/internal/ssh-agent/approve", strings.NewReader(body)))
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

// Approvals are bound to the sandbox that will sign.
func TestApproveRequiresSandboxID(t *testing.T) {
	ap := sshagent.NewApprover(time.Minute)
	s := &Server{SSHApprover: ap}
	for _, body := range []string{``, `{}`, `{"ttl_seconds":30,"actor_sub":"user:alice"}`} {
		if code, out := approve(t, s, body); code != http.StatusBadRequest {
			t.Fatalf("approve %q without sandbox_id: want 400, got %d %v", body, code, out)
		}
	}
	if code, _ := approve(t, s, `{"sandbox_id":`); code != http.StatusBadRequest {
		t.Fatalf("invalid JSON: want 400, got %d", code)
	}

	code, out := approve(t, s, `{"sandbox_id":"sb-1","ttl_seconds":30,"actor_sub":"user:alice"}`)
	if code != http.StatusOK {
		t.Fatalf("approve sb-1: %d %v", code, out)
	}
	if out["sandbox_id"] != "sb-1" || out["approval_id"] == "" || out["token"] != nil {
		t.Fatalf("response %v: want sandbox_id, an approval_id and no token", out)
	}
	if ap.Consume("sb-2") || !ap.Consume("sb-1") {
		t.Fatal("the approval must unlock a sign from sb-1 only")
	}
}

func TestApproveGlobalNeedsTheLabFlag(t *testing.T) {
	ap := sshagent.NewApprover(time.Minute)
	ap.GlobalApprovals = true
	code, out := approve(t, &Server{SSHApprover: ap}, `{}`)
	if code != http.StatusOK || out["scope"] != "global" {
		t.Fatalf("global approval with GlobalApprovals: %d %v", code, out)
	}
	if !ap.Consume("") {
		t.Fatal("the global approval should unlock a sign on a listener without a sandbox")
	}

	if code, _ := approve(t, &Server{}, `{"sandbox_id":"sb-1"}`); code != http.StatusServiceUnavailable {
		t.Fatalf("no confirmation gate: want 503, got %d", code)
	}
}

// egress-check with sandbox_id evaluates the policy the proxy applies to that
// sandbox now (from the work poll), deny-default until it has one.
func TestEgressCheckForASandbox(t *testing.T) {
	cache := &egress.PolicyCache{}
	cache.Set("sb-1", egress.NewAllowlist("api.github.com"))
	h := (&Server{PolicyCache: cache}).Handler()
	check := func(sandbox, host string) bool {
		t.Helper()
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/internal/egress-check",
			strings.NewReader(`{"host":"`+host+`","sandbox_id":"`+sandbox+`"}`)))
		var out struct {
			Allowed bool `json:"allowed"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("%d %s", rr.Code, rr.Body.String())
		}
		return out.Allowed
	}
	if !check("sb-1", "api.github.com") || check("sb-1", "evil.example") {
		t.Fatal("sb-1 must follow its policy")
	}
	if check("sb-2", "api.github.com") {
		t.Fatal("a sandbox without a policy is deny-default")
	}
}
