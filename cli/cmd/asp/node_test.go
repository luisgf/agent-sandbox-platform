package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/client"
)

func TestNodeListCordonUncordon(t *testing.T) {
	t.Setenv("ASP_API_KEY", "")
	t.Setenv("ASP_ID_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	var cordonCalls []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/nodes", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"nodes":[
			{"id":"node-a","state":"ready","schedulable":true,"last_seen_at":"` + time.Now().UTC().Add(-3*time.Second).Format(time.RFC3339) + `",
			 "allocated":{"cpu_millis":1500,"memory_mib":1024,"sandboxes":1},"allocatable":{"cpu_millis":16000,"memory_mib":7168,"sandboxes":0}},
			{"id":"node-b","state":"ready","cordoned":true,"schedulable":false,"unschedulable_reason":"cordoned","last_seen_at":null,
			 "allocated":{"cpu_millis":0,"memory_mib":0,"sandboxes":0},"allocatable":{"cpu_millis":0,"memory_mib":0,"sandboxes":4}}]}`))
	})
	for _, action := range []string{"cordon", "uncordon"} {
		action := action
		mux.HandleFunc("POST /v1/nodes/{id}/"+action, func(w http.ResponseWriter, r *http.Request) {
			cordonCalls = append(cordonCalls, action+":"+r.PathValue("id"))
			if r.PathValue("id") == "ghost" {
				http.Error(w, `{"error":"node not found"}`, http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(`{"id":"` + r.PathValue("id") + `","cordoned":` + map[string]string{"cordon": "true", "uncordon": "false"}[action] +
				`,"schedulable":` + map[string]string{"cordon": "false", "uncordon": "true"}[action] + `,"allocated":{"sandboxes":2}}`))
		})
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var stdout, stderr strings.Builder
	if code := run([]string{"node", "list", "--cp-url", srv.URL}, &stdout, &stderr); code != 0 {
		t.Fatalf("list exit=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"NODE", "node-a", "yes", "1.5/16.0", "1024/7168", "1/-", "node-b", "no: cordoned", "0/4", "never"} {
		if !strings.Contains(out, want) {
			t.Errorf("node list missing %q:\n%s", want, out)
		}
	}

	stdout.Reset()
	if code := run([]string{"node", "cordon", "--cp-url", srv.URL, "node-a"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "node node-a cordoned") || !strings.Contains(stdout.String(), "2 running stay") {
		t.Fatalf("cordon exit=%d out=%s err=%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if code := run([]string{"node", "uncordon", "--cp-url", srv.URL, "node-a"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "schedulable=true") {
		t.Fatalf("uncordon exit=%d out=%s", code, stdout.String())
	}
	stderr.Reset()
	if code := run([]string{"node", "cordon", "--cp-url", srv.URL, "ghost"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "node not found") {
		t.Fatalf("cordon unknown exit=%d err=%s", code, stderr.String())
	}
	if code := run([]string{"node", "cordon", "--cp-url", srv.URL}, &stdout, &stderr); code != 2 {
		t.Fatalf("cordon without id must be a usage error, got %d", code)
	}
	if len(cordonCalls) != 3 {
		t.Fatalf("calls=%v", cordonCalls)
	}
}

func TestCreateExplainsNoCapacityAndRejectedPins(t *testing.T) {
	cases := map[int]string{
		http.StatusServiceUnavailable: "no capacity: no node can fit",
		http.StatusConflict:           "node pin rejected: node ghost is not registered",
	}
	for status, want := range cases {
		msg := map[int]string{
			http.StatusServiceUnavailable: "no node can fit cpu_millis=1000 memory_mib=512 (2 nodes: 2 max_sandboxes)",
			http.StatusConflict:           "node ghost is not registered",
		}[status]
		got := explainCreateError(&client.HTTPError{StatusCode: status, Message: msg})
		if !strings.HasPrefix(got, want) || !strings.Contains(got, "asp node list") {
			t.Errorf("status %d: %s", status, got)
		}
	}
	if got := explainCreateError(&client.HTTPError{StatusCode: 400, Message: "bad"}); got != "control-plane HTTP 400: bad" {
		t.Errorf("other errors unchanged: %s", got)
	}
}
