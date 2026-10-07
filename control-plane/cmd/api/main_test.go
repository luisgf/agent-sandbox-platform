package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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
	return startRunIn(t, t.TempDir(), nil)
}

// startRunIn is startRun with its keys in dir (a restart keeps them) and extra settings.
func startRunIn(t *testing.T, dir string, extra map[string]string) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
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
		// These tests exercise shutdown, not authentication.
		"ASP_INSECURE_OPEN_API": "1",
	} {
		t.Setenv(k, v)
	}
	for k, v := range extra {
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

// With a sqlite: database URL the state is a file: what a first run wrote is there after the
// control plane stopped and started again, and the file is private.
func TestSQLiteStateSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state", "asp.db")
	extra := map[string]string{"ASP_DATABASE_URL": "sqlite://" + db, "ASP_AUTO_PROVISION": "0", "ASP_ALLOW_TMP_KEYS": "1"}

	base, cancel, done := startRunIn(t, dir, extra)
	postJSON(t, base+"/v1/nodes/register", `{"id":"lite-node","agent_endpoint":"http://127.0.0.1:9100"}`)
	sb := postJSON(t, base+"/v1/sandboxes", `{"tenant_id":"t","image_ref":"img","cpu_millis":100,"memory_mib":64}`)
	id, _ := sb["id"].(string)
	if id == "" {
		t.Fatalf("no sandbox id in %v", sb)
	}
	cancel()
	waitRun(t, done)

	fi, err := os.Stat(db)
	if err != nil {
		t.Fatalf("the database file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("the database file is %v: it holds key hashes and fence tokens", fi.Mode().Perm())
	}
	if di, err := os.Stat(filepath.Dir(db)); err != nil || di.Mode().Perm() != 0o700 {
		t.Errorf("its directory: %v %v", di, err)
	}

	base, cancel, done = startRunIn(t, dir, extra)
	resp, err := http.Get(base + "/v1/sandboxes/" + id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), id) {
		t.Fatalf("the sandbox after a restart: %d %s", resp.StatusCode, raw)
	}
	resp, err = http.Get(base + "/v1/nodes")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), "lite-node") {
		t.Fatalf("the node after a restart: %s", raw)
	}
	cancel()
	waitRun(t, done)
}

func TestSQLitePathOfTheDatabaseURL(t *testing.T) {
	for in, want := range map[string]string{
		"sqlite:///var/lib/asp/asp.db": "/var/lib/asp/asp.db",
		"sqlite:/var/lib/asp/asp.db":   "/var/lib/asp/asp.db",
		"sqlite://relative.db":         "relative.db",
		"sqlite:relative.db":           "relative.db",
		" sqlite:///x.db ":             "/x.db",
	} {
		got, ok, err := sqlitePath(in)
		if err != nil || !ok || got != want {
			t.Errorf("sqlitePath(%q) = %q %v %v, want %q", in, got, ok, err, want)
		}
	}
	for _, in := range []string{"postgres://asp@db/asp", "", "host=db user=asp"} {
		if _, ok, err := sqlitePath(in); ok || err != nil {
			t.Errorf("sqlitePath(%q) = %v %v: not a SQLite URL", in, ok, err)
		}
	}
	for _, in := range []string{"sqlite:", "sqlite://", "sqlite:///"} {
		if _, ok, err := sqlitePath(in); !ok || err == nil {
			t.Errorf("sqlitePath(%q) = %v %v: want a SQLite URL with no path to be an error", in, ok, err)
		}
	}
}
