package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

func TestRevokedNodeCannotHeartbeatOrRegister(t *testing.T) {
	mem := store.NewMemoryStore()
	if _, err := mem.RegisterNode(store.RegisterNodeInput{ID: "n1", AgentEndpoint: "http://127.0.0.1:9100"}); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.RevokeNode("n1"); err != nil {
		t.Fatal(err)
	}
	mux := testMux(NewServer(mem))

	for _, tc := range []struct{ path, body string }{
		{"/v1/nodes/n1/heartbeat", ""},
		{"/v1/nodes/register", `{"id":"n1","agent_endpoint":"http://127.0.0.1:9100"}`},
	} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, tc.path, bytes.NewBufferString(tc.body)))
		if rr.Code != http.StatusConflict {
			t.Errorf("%s: want 409 for a revoked node, got %d %s", tc.path, rr.Code, rr.Body.String())
		}
	}
}
