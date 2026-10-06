package execproxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/poddaemon"
)

// A sandbox this node does not run gets 404 (the control plane answers 409);
// a node without any pod-daemon wiring keeps 503.
func TestExecForASandboxNotOnThisNode(t *testing.T) {
	s := &Server{Registry: poddaemon.NewRegistry(nil)}
	h := s.Handler()
	for path, body := range map[string]string{
		"/v1/internal/exec":       `{"sandbox_id":"sb-x","cmd":["true"]}`,
		"/v1/internal/exec/stdin": `{"sandbox_id":"sb-x","exec_id":"e1","data":"x"}`,
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "sb-x") {
			t.Errorf("%s: want 404 naming the sandbox, got %d %s", path, rr.Code, rr.Body.String())
		}
	}

	rr := httptest.NewRecorder()
	(&Server{}).Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/internal/exec", strings.NewReader(`{"sandbox_id":"sb-x","cmd":["true"]}`)))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("no pod-daemon wiring: want 503, got %d %s", rr.Code, rr.Body.String())
	}
}
