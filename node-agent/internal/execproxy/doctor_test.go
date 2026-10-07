package execproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The self-check report is served on the loopback API (behind the agent token, added
// by the caller) and on the control plane's mTLS one, from the same hook.
func TestDoctorRoute(t *testing.T) {
	calls := 0
	s := &Server{Doctor: func(ctx context.Context) any {
		calls++
		return map[string]any{"node_id": "n1", "results": []map[string]string{{"name": "kvm", "status": "ok"}}}
	}}
	for name, h := range map[string]http.Handler{"local": s.Handler(), "remote": s.RemoteHandler()} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/internal/doctor", nil))
		if rr.Code != http.StatusOK || !strings.Contains(rr.Header().Get("Content-Type"), "json") {
			t.Fatalf("%s: %d %q", name, rr.Code, rr.Header().Get("Content-Type"))
		}
		var back struct {
			NodeID  string `json:"node_id"`
			Results []struct{ Name, Status string }
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &back); err != nil || back.NodeID != "n1" || len(back.Results) != 1 {
			t.Fatalf("%s: %v %s", name, err, rr.Body.String())
		}
		// It is a read: no other method reaches it.
		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/internal/doctor", nil))
		if rr.Code == http.StatusOK {
			t.Fatalf("%s: POST was answered", name)
		}
	}
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
}

// An agent built without the hook answers 501, which is what tells the control plane
// to say the node should be upgraded.
func TestDoctorRouteWithoutAHook(t *testing.T) {
	rr := httptest.NewRecorder()
	(&Server{}).RemoteHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/internal/doctor", nil))
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
}
