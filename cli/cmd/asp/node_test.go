package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/client"
	"github.com/luisgf/agent-sandbox-platform/cli/internal/session"
)

func TestNodeListCordonUncordon(t *testing.T) {
	t.Setenv("ASP_API_KEY", "")
	t.Setenv("ASP_ID_TOKEN", "")
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	var cordonCalls []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/nodes", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"nodes":[
			{"id":"node-a","state":"ready","schedulable":true,"egress_enforced":true,"agent_version":"0.1.0","last_seen_at":"` + time.Now().UTC().Add(-3*time.Second).Format(time.RFC3339) + `",
			 "cert_not_after":"` + time.Now().UTC().Add(200*24*time.Hour+time.Hour).Format(time.RFC3339) + `",
			 "stopped_sandboxes":3,"disk_free_mib":716800,
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
	if code := run([]string{"node", "list", "--control-plane-url", srv.URL}, &stdout, &stderr); code != 0 {
		t.Fatalf("list exit=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"NODE", "node-a", "yes", "1.5/16.0", "1024/7168", "1/-", "node-b", "no: cordoned", "0/4", "never", "CERT EXPIRES", "in 200d", "STOPPED (DISKS)", "DISK FREE", "700 GiB", "EGRESS", "enforced", "VERSION", "0.1.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("node list missing %q:\n%s", want, out)
		}
	}

	// Each node's row says how many stopped sandboxes keep a disk on it, and its free disk.
	var rowA, rowB string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "node-a"):
			rowA = line
		case strings.HasPrefix(line, "node-b"):
			rowB = line
		}
	}
	if f := strings.Fields(rowA); len(f) < 10 || f[7] != "3" || f[3] != "enforced" {
		t.Errorf("node-a row should show egress enforced and 3 stopped sandboxes: %q", rowA)
	}
	if f := strings.Fields(rowB); len(f) < 5 || f[len(f)-1] == "enforced" || !strings.Contains(rowB, " off ") {
		t.Errorf("node-b does not enforce egress: %q", rowB)
	}
	if !strings.Contains(rowB, "  0  ") || !strings.Contains(rowB, " -  ") {
		t.Errorf("node-b reported no disk and has no stopped sandboxes: %q", rowB)
	}

	stdout.Reset()
	if code := run([]string{"node", "cordon", "--control-plane-url", srv.URL, "node-a"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "node node-a cordoned") || !strings.Contains(stdout.String(), "2 running stay") {
		t.Fatalf("cordon exit=%d out=%s err=%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if code := run([]string{"node", "uncordon", "--control-plane-url", srv.URL, "node-a"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "schedulable=true") {
		t.Fatalf("uncordon exit=%d out=%s", code, stdout.String())
	}
	stderr.Reset()
	if code := run([]string{"node", "cordon", "--control-plane-url", srv.URL, "ghost"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "node not found") {
		t.Fatalf("cordon unknown exit=%d err=%s", code, stderr.String())
	}
	if code := run([]string{"node", "cordon", "--control-plane-url", srv.URL}, &stdout, &stderr); code != 2 {
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

func TestSessionStatusExplainsASandboxLostWithItsNode(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	t.Setenv("ASP_API_KEY", "")
	for reason, want := range map[string]string{
		client.StopReasonNodeLost:       "its node stopped responding",
		client.StopReasonAgentRestarted: "the node agent restarted",
	} {
		node := "node-a"
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(client.Sandbox{ID: "lost-1", State: "failed", TenantID: "acme", NodeID: &node, StopReason: reason})
		})
		srv := httptest.NewServer(mux)
		sessFile := filepath.Join(t.TempDir(), "session.json")
		if err := session.Save(sessFile, session.State{SandboxID: "lost-1", CPURL: srv.URL, TenantID: "acme"}); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr strings.Builder
		code := run([]string{"session", "status", "--session-file", sessFile}, &stdout, &stderr)
		srv.Close()
		if code != 1 || !strings.Contains(stderr.String(), want) || !strings.Contains(stderr.String(), "asp session start --force") ||
			!strings.Contains(stdout.String(), "lost_with_node=true") || !strings.Contains(stdout.String(), "node=node-a") {
			t.Fatalf("%s: exit=%d stdout=%q stderr=%q", reason, code, stdout.String(), stderr.String())
		}
	}
}

func TestNodeEnrollToken(t *testing.T) {
	t.Setenv("ASP_API_KEY", "")
	t.Setenv("ASP_ID_TOKEN", "")
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	var got map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/nodes/enroll-tokens", func(w http.ResponseWriter, r *http.Request) {
		got = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&got)
		if got["node_id"] == "tenant" {
			http.Error(w, `{"error":"a platform-scoped api key is required to issue enroll tokens"}`, http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "asp_enroll_abc", "node_id": got["node_id"], "expires_at": time.Now().Add(time.Hour)})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var stdout, stderr strings.Builder
	if code := run([]string{"node", "enroll-token", "--control-plane-url", srv.URL, "--node-id", "node-a", "--ttl", "2h"}, &stdout, &stderr); code != 0 {
		t.Fatalf("enroll-token exit=%d stderr=%s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "asp_enroll_abc" {
		t.Fatalf("stdout must be the token alone, got %q", stdout.String())
	}
	if got["node_id"] != "node-a" || got["ttl_seconds"] != float64(7200) {
		t.Fatalf("request body: %v", got)
	}
	if !strings.Contains(stderr.String(), "node node-a only") || !strings.Contains(stderr.String(), "--enroll-token=<token> --node-id=node-a") {
		t.Fatalf("usage hint: %s", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"node", "enroll-token", "--control-plane-url", srv.URL}, &stdout, &stderr); code != 0 || got["node_id"] != nil || got["ttl_seconds"] != float64(3600) {
		t.Fatalf("unpinned token: exit=%d body=%v err=%s", code, got, stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"node", "enroll-token", "--control-plane-url", srv.URL, "--node-id", "tenant"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "platform-scoped") {
		t.Fatalf("refused: exit=%d err=%s", code, stderr.String())
	}
	if code := run([]string{"node", "enroll-token", "--control-plane-url", srv.URL, "extra"}, &stdout, &stderr); code != 2 {
		t.Fatalf("extra argument must be a usage error, got %d", code)
	}
}

func TestExpiryText(t *testing.T) {
	now := time.Now()
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	for _, tc := range []struct {
		t    *time.Time
		want string
	}{
		{nil, "-"},
		{at(-time.Hour), "EXPIRED"},
		{at(10*24*time.Hour + time.Hour), "in 10d (renewal failing?)"},
		{at(100*24*time.Hour + time.Hour), "in 100d"},
	} {
		if got := expiryText(tc.t, now); got != tc.want {
			t.Errorf("expiryText(%v) = %q, want %q", tc.t, got, tc.want)
		}
	}
}
