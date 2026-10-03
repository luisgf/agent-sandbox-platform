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

func TestExecPTYAndStdinReachNodeAgent(t *testing.T) {
	var sawPTY bool
	var sawRows float64
	var stdinBody string
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/v1/internal/exec":
			if r.URL.Query().Get("stream") != "1" {
				t.Errorf("query=%s", r.URL.RawQuery)
			}
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			sawPTY, _ = body["pty"].(bool)
			sawRows, _ = body["rows"].(float64)
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = io.WriteString(w, "{\"type\":\"ready\",\"exec_id\":\"e1\"}\n{\"type\":\"stdout\",\"data\":\"ok\"}\n{\"type\":\"exit\",\"exit_code\":0}\n")
		case "/v1/internal/exec/stdin":
			stdinBody = string(raw)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer agent.Close()

	mem := store.NewMemoryStore()
	mem.SetProvisionNodeID("n")
	if _, err := mem.RegisterNode(store.RegisterNodeInput{ID: "n", Name: "n", Endpoint: agent.URL, AgentEndpoint: agent.URL}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(mem)
	srv.Client = agent.Client()
	cp := httptest.NewServer(testMux(srv))
	defer cp.Close()

	creq, err := http.NewRequest(http.MethodPost, cp.URL+"/v1/sandboxes", strings.NewReader(`{"tenant_id":"t1","image_ref":"img","cpu_millis":1,"memory_mib":1,"node_id":"n"}`))
	if err != nil {
		t.Fatal(err)
	}
	cresp, err := http.DefaultClient.Do(creq)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(cresp.Body)
	cresp.Body.Close()
	var sb store.Sandbox
	if err := json.Unmarshal(raw, &sb); err != nil {
		t.Fatal(err)
	}
	ereq, err := http.NewRequest(http.MethodPost, cp.URL+"/v1/sandboxes/"+sb.ID+"/exec?stream=1", strings.NewReader(`{"cmd":["sh"],"pty":true,"rows":24,"cols":80}`))
	if err != nil {
		t.Fatal(err)
	}
	eresp, err := http.DefaultClient.Do(ereq)
	if err != nil {
		t.Fatal(err)
	}
	ebody, _ := io.ReadAll(eresp.Body)
	eresp.Body.Close()
	if eresp.StatusCode != http.StatusOK || !strings.Contains(string(ebody), "exec_id") {
		t.Fatalf("exec=%d %s", eresp.StatusCode, ebody)
	}
	if !sawPTY || sawRows != 24 {
		t.Fatalf("pty=%v rows=%v", sawPTY, sawRows)
	}
	sreq, err := http.NewRequest(http.MethodPost, cp.URL+"/v1/sandboxes/"+sb.ID+"/exec/stdin", strings.NewReader(`{"exec_id":"e1","data":"pwd\n"}`))
	if err != nil {
		t.Fatal(err)
	}
	sresp, err := http.DefaultClient.Do(sreq)
	if err != nil {
		t.Fatal(err)
	}
	defer sresp.Body.Close()
	if sresp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(sresp.Body)
		t.Fatalf("stdin=%d %s", sresp.StatusCode, b)
	}
	if !strings.Contains(stdinBody, "e1") || !strings.Contains(stdinBody, "pwd") || !strings.Contains(stdinBody, sb.ID) {
		t.Fatalf("forwarded=%s", stdinBody)
	}
	got, err := mem.GetSandbox(sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastActivityAt.Before(sb.LastActivityAt) {
		t.Fatal("stdin did not refresh activity")
	}
}
