package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

func TestExecStreamProxiesNDJSONBeforeUpstreamFinishes(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/internal/exec" || r.URL.Query().Get("stream") != "1" {
			t.Errorf("upstream %s %s", r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "{\"type\":\"stdout\",\"data\":\"chunk\\n\"}\n")
		if fl != nil {
			fl.Flush()
		}
		close(started)
		<-release
		_, _ = io.WriteString(w, "{\"type\":\"exit\",\"exit_code\":0}\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	defer agent.Close()

	mem := store.NewMemoryStore()
	mem.SetProvisionNodeID("exec-node")
	if _, err := mem.RegisterNode(store.RegisterNodeInput{
		ID: "exec-node", Name: "exec-node",
		Endpoint: agent.URL, AgentEndpoint: agent.URL,
	}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(mem)
	srv.Client = agent.Client()
	cp := httptest.NewServer(testMux(srv))
	defer cp.Close()

	body := `{"tenant_id":"t1","image_ref":"img","cpu_millis":1,"memory_mib":1,"node_id":"exec-node","workspace_host_path":"/opt/work"}`
	creq, err := http.NewRequest(http.MethodPost, cp.URL+"/v1/sandboxes", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	cresp, err := http.DefaultClient.Do(creq)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(cresp.Body)
	cresp.Body.Close()
	if cresp.StatusCode != http.StatusCreated {
		t.Fatalf("create=%d %s", cresp.StatusCode, raw)
	}
	var sb store.Sandbox
	if err := json.Unmarshal(raw, &sb); err != nil {
		t.Fatal(err)
	}
	if sb.WorkspaceHostPath != "/opt/work" {
		t.Fatalf("workspace=%q", sb.WorkspaceHostPath)
	}
	runSandbox(t, mem, sb.ID)

	ereq, err := http.NewRequest(http.MethodPost, cp.URL+"/v1/sandboxes/"+sb.ID+"/exec?stream=1", bytes.NewBufferString(`{"cmd":["echo","chunk"]}`))
	if err != nil {
		t.Fatal(err)
	}
	eresp, err := http.DefaultClient.Do(ereq)
	if err != nil {
		t.Fatal(err)
	}
	defer eresp.Body.Close()
	if eresp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(eresp.Body)
		t.Fatalf("exec=%d %s", eresp.StatusCode, b)
	}
	if ct := eresp.Header.Get("Content-Type"); !strings.Contains(ct, "ndjson") {
		t.Fatalf("content-type=%q", ct)
	}
	rd := bufio.NewReader(eresp.Body)
	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "chunk") {
		t.Fatalf("first line=%q", line)
	}
	select {
	case <-started:
	default:
		t.Fatal("first line arrived without upstream having flushed")
	}
	close(release)
	rest, err := io.ReadAll(rd)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rest), `"exit_code":0`) && !strings.Contains(string(rest), `"exit_code": 0`) {
		t.Fatalf("rest=%s", rest)
	}
	got, err := mem.GetSandbox(sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastActivityAt.Before(sb.LastActivityAt) {
		t.Fatalf("activity not refreshed: before=%s after=%s", sb.LastActivityAt, got.LastActivityAt)
	}
}

func TestExecBufferedJSONUnchanged(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stream") != "" {
			t.Errorf("buffered call set stream=%q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stdout":"full\n","stderr":"","exit_code":0}`))
	}))
	defer agent.Close()
	mem := store.NewMemoryStore()
	mem.SetProvisionNodeID("n")
	_, _ = mem.RegisterNode(store.RegisterNodeInput{ID: "n", Name: "n", Endpoint: agent.URL, AgentEndpoint: agent.URL})
	srv := NewServer(mem)
	srv.Client = agent.Client()
	mux := testMux(srv)
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", strings.NewReader(`{"tenant_id":"t1","image_ref":"img","cpu_millis":1,"memory_mib":1,"node_id":"n"}`))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	var sb store.Sandbox
	_ = json.Unmarshal(rr.Body.Bytes(), &sb)
	runSandbox(t, mem, sb.ID)
	req = httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/exec", strings.NewReader(`{"cmd":["echo","full"]}`))
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"stdout":"full\n"`) {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Header().Get("Content-Type"), "ndjson") {
		t.Fatalf("buffered response should stay JSON, ct=%s", rr.Header().Get("Content-Type"))
	}
}
