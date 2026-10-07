package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestNodeFenceSetAndClear(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	t.Setenv("ASP_API_KEY", "")
	var mu sync.Mutex
	var calls []string
	var body map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/nodes/{id}/fence", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, "PUT "+r.PathValue("id"))
		body = map[string]string{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /v1/nodes/{id}/fence", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, "DELETE "+r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	run1 := func(args ...string) (int, string, string) {
		var stdout, stderr strings.Builder
		code := run(append([]string{"node", "fence"}, append(args, "--control-plane-url", srv.URL)...), &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}

	// A reference to the control plane's environment: the secret is never sent.
	code, out, errOut := run1("set", "n1", "--endpoint", "https://bmc.example/redfish", "--token-env", "BMC_PW")
	if code != 0 || !strings.Contains(out, "fence target set") {
		t.Fatalf("set: exit=%d out=%q err=%q", code, out, errOut)
	}
	if body["endpoint"] != "https://bmc.example/redfish" || body["token"] != "env:BMC_PW" {
		t.Fatalf("body = %v", body)
	}
	code, _, _ = run1("set", "n2", "--endpoint", "10.0.0.9", "--token-file", "/etc/asp/bmc.pw")
	if code != 0 || body["token"] != "file:/etc/asp/bmc.pw" {
		t.Fatalf("token-file: exit=%d body=%v", code, body)
	}
	code, out, _ = run1("clear", "n1")
	if code != 0 || !strings.Contains(out, "cleared") {
		t.Fatalf("clear: exit=%d out=%q", code, out)
	}
	if strings.Join(calls, ",") != "PUT n1,PUT n2,DELETE n1" {
		t.Fatalf("calls = %v", calls)
	}

	// Usage errors do not reach the server.
	calls = nil
	for _, args := range [][]string{
		{"set", "n1"}, // no endpoint
		{"set", "n1", "--endpoint", "x", "--token-env", "A", "--token-file", "/b"}, // two sources
		{"clear"},
		{"bogus"},
		{},
	} {
		if code, _, _ := run1(args...); code != 2 {
			t.Errorf("%v: exit=%d, want 2", args, code)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("a usage error reached the server: %v", calls)
	}
}

func TestNodeFenceShowsRefusals(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	t.Setenv("ASP_API_KEY", "")
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/nodes/{id}/fence", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"a platform-scoped api key is required to configure node fencing"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	var stdout, stderr strings.Builder
	code := run([]string{"node", "fence", "set", "n1", "--endpoint", "https://bmc.example", "--control-plane-url", srv.URL}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "platform-scoped") {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
}
