package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// A command runs as root in the guest only when its request says as_root; the
// control plane passes the flag to the node agent and journals it.
func TestExecAsRootIsForwardedAndJournaled(t *testing.T) {
	var bodies []map[string]any
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stdout":"","stderr":"","exit_code":0}`))
	}))
	defer agent.Close()

	mem := store.NewMemoryStore()
	mem.SetProvisionNodeID("n")
	if _, err := mem.RegisterNode(store.RegisterNodeInput{ID: "n", Name: "n", Endpoint: agent.URL, AgentEndpoint: agent.URL}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(mem)
	srv.Client = agent.Client()
	mux := testMux(srv)

	post := func(path, body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return rr
	}
	rr := post("/v1/sandboxes", `{"tenant_id":"t1","image_ref":"img","cpu_millis":1,"memory_mib":64,"node_id":"n"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var sb store.Sandbox
	_ = json.Unmarshal(rr.Body.Bytes(), &sb)
	runSandbox(t, mem, sb.ID)

	for _, body := range []string{`{"cmd":["id"],"as_root":true}`, `{"cmd":["id"]}`} {
		if rr := post("/v1/sandboxes/"+sb.ID+"/exec", body); rr.Code != http.StatusOK {
			t.Fatalf("exec %s: %d %s", body, rr.Code, rr.Body.String())
		}
	}
	if len(bodies) != 2 || bodies[0]["as_root"] != true || bodies[1]["as_root"] != false {
		t.Fatalf("node agent saw %v", bodies)
	}

	evs, _ := mem.ListEvents(sb.ID)
	var flags []any
	for _, ev := range evs {
		if ev.EventType != "sandbox.exec" {
			continue
		}
		var p map[string]any
		_ = json.Unmarshal(ev.Payload, &p)
		flags = append(flags, p["as_root"])
	}
	if len(flags) != 2 || flags[0] != true || flags[1] != false {
		t.Fatalf("journal as_root flags: %v", flags)
	}
}
