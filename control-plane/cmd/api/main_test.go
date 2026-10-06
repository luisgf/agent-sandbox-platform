package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startRun runs the control plane on a free loopback port with every key in a
// temp dir, and returns its base URL, the cancel func standing in for SIGTERM,
// and the channel run's result arrives on.
func startRun(t *testing.T) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	dir := t.TempDir()
	for k, v := range map[string]string{
		"LISTEN_ADDR":              addr,
		"DATABASE_URL":             "",
		"ASP_CA_CERT":              filepath.Join(dir, "ca.crt"),
		"ASP_CA_KEY":               filepath.Join(dir, "ca.key"),
		"ASP_OIDC_KEY":             filepath.Join(dir, "oidc.pem"),
		"ASP_ATTEST_KEY":           filepath.Join(dir, "attest.pem"),
		"ASP_SHUTDOWN_TIMEOUT":     "5s",
		"ASP_IDP_ISSUER":           "",
		"ASP_TLS_CERT":             "",
		"ASP_TLS_KEY":              "",
		"ASP_CLIENT_CA":            "",
		"ASP_AUTO_PROVISION":       "",
		"ASP_SANDBOX_IDLE_TIMEOUT": "",
		"ASP_EGRESS_DEFAULT_ALLOW": "",
		"ASP_BOOTSTRAP_API_KEY":    "",
	} {
		t.Setenv(k, v)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- run(ctx, nil) }()
	base := "http://" + addr
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base, cancel, done
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("control plane did not become healthy: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitRun(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after shutdown")
	}
}

func TestRunStopsWhenCancelled(t *testing.T) {
	base, cancel, done := startRun(t)
	cancel()
	waitRun(t, done)
	if _, err := http.Get(base + "/healthz"); err == nil {
		t.Fatal("the listener is still serving after shutdown")
	}
}

func postJSON(t *testing.T, url, body string) map[string]any {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("POST %s: %d %s", url, resp.StatusCode, raw)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

// SIGTERM during a streamed exec lets the stream finish instead of cutting it.
func TestShutdownWaitsForAnInFlightStream(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fl := w.(http.Flusher)
		for i := 0; i < 6; i++ {
			_, _ = io.WriteString(w, "{\"type\":\"stdout\",\"data\":\"tick\\n\"}\n")
			fl.Flush()
			time.Sleep(100 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "{\"type\":\"exit\",\"exit_code\":0}\n")
	}))
	defer agent.Close()

	base, cancel, done := startRun(t)
	postJSON(t, base+"/v1/nodes/register", `{"id":"n1","agent_endpoint":"`+agent.URL+`"}`)
	sb := postJSON(t, base+"/v1/sandboxes", `{"tenant_id":"t","image_ref":"img","cpu_millis":100,"memory_mib":64}`)
	id, _ := sb["id"].(string)
	postJSON(t, base+"/v1/sandboxes/"+id+"/claim", `{"node_id":"n1"}`)
	postJSON(t, base+"/v1/sandboxes/"+id+"/status", `{"state":"running"}`)

	resp, err := http.Post(base+"/v1/sandboxes/"+id+"/exec?stream=1", "application/json", strings.NewReader(`{"cmd":["x"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	if line, err := r.ReadString('\n'); err != nil || !strings.Contains(line, "tick") {
		t.Fatalf("first event %q, err=%v", line, err)
	}
	cancel()
	rest, err := io.ReadAll(r)
	if err != nil || !strings.Contains(string(rest), `"exit_code":0`) {
		t.Fatalf("stream cut by the shutdown: err=%v rest=%q", err, rest)
	}
	waitRun(t, done)
}

func TestShutdownTimeoutFromEnv(t *testing.T) {
	t.Setenv(EnvShutdownTimeout, "")
	if d, err := shutdownTimeoutFromEnv(); err != nil || d != defaultShutdownTimeout {
		t.Fatalf("default: %v %v", d, err)
	}
	t.Setenv(EnvShutdownTimeout, "2m")
	if d, err := shutdownTimeoutFromEnv(); err != nil || d != 2*time.Minute {
		t.Fatalf("2m: %v %v", d, err)
	}
	for _, bad := range []string{"0", "-1s", "soon"} {
		t.Setenv(EnvShutdownTimeout, bad)
		if _, err := shutdownTimeoutFromEnv(); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}
