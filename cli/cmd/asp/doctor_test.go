package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const nodeReport = `{"node_id":"n1","at":"2026-10-07T12:00:00Z","results":[
 {"name":"kvm","status":"ok","detail":"/dev/kvm opens read-write"},
 {"name":"virtiofsd","status":"warn","detail":"not found","fix":"install the Rust virtiofsd"},
 {"name":"nft","status":"fail","detail":"table asp_egress is not applied","fix":"restart the agent"},
 {"name":"vsock","status":"skip","detail":"--host-vsock is off"}]}`

func doctorServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/nodes/{id}/doctor", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "ghost" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"node not found"}`))
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestNodeDoctorPrintsTheReportAndFailsWhenACheckFailed(t *testing.T) {
	clearAuthEnv(t)
	srv := doctorServer(t, http.StatusOK, nodeReport)
	var stdout, stderr strings.Builder
	code := run([]string{"node", "doctor", "n1", "--control-plane-url", srv.URL}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d, want 1 (a check failed): %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"node doctor (n1)", "ok    kvm", "warn  virtiofsd", "fix: install the Rust virtiofsd", "fail  nft", "fix: restart the agent", "skip  vsock", "1 ok, 1 warn, 1 fail, 1 skipped"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// A fix is shown only under a problem.
	if strings.Count(out, "fix:") != 2 {
		t.Errorf("fix lines:\n%s", out)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"node", "doctor", "--json", "n1", "--control-plane-url", srv.URL}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var back struct {
		NodeID  string `json:"node_id"`
		Results []struct{ Name, Status string }
	}
	if err := json.Unmarshal([]byte(stdout.String()), &back); err != nil || back.NodeID != "n1" || len(back.Results) != 4 {
		t.Fatalf("json: %v %s", err, stdout.String())
	}
}

func TestNodeDoctorSucceedsWhenNothingFailed(t *testing.T) {
	clearAuthEnv(t)
	srv := doctorServer(t, http.StatusOK, `{"node_id":"n1","results":[{"name":"kvm","status":"ok","detail":"fine"},{"name":"nft","status":"warn","detail":"leftover"}]}`)
	var stdout, stderr strings.Builder
	if code := run([]string{"node", "doctor", "n1", "--control-plane-url", srv.URL}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s%s", code, stdout.String(), stderr.String())
	}
}

func TestNodeDoctorErrors(t *testing.T) {
	clearAuthEnv(t)
	srv := doctorServer(t, http.StatusNotImplemented, `{"error":"this node-agent has no doctor: upgrade it"}`)
	var stdout, stderr strings.Builder
	if code := run([]string{"node", "doctor", "ghost", "--control-plane-url", srv.URL}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "node not found") {
		t.Fatalf("unknown node: %d %q", code, stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"node", "doctor", "n1", "--control-plane-url", srv.URL}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "upgrade it") {
		t.Fatalf("old agent: %d %q", code, stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"node", "doctor", "--control-plane-url", srv.URL}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "usage: asp node doctor") {
		t.Fatalf("no node: %d %q", code, stderr.String())
	}
}

func TestReadEnvFileLikeSystemd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node-agent.env")
	content := "# settings of this node\n\nASP_CONTROL_PLANE_URL=https://cp.example:8443\nASP_ENDPOINT=\"https://n1.example:9443\"\n; also a comment\nASP_NODE_ID = 'n1'\nnot a setting\n=novalue\nASP_EMPTY=\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ASP_CONTROL_PLANE_URL=https://cp.example:8443", "ASP_ENDPOINT=https://n1.example:9443", "ASP_NODE_ID=n1", "ASP_EMPTY="}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
	if _, err := readEnvFile(filepath.Join(t.TempDir(), "absent")); !os.IsNotExist(err) {
		t.Fatalf("a missing file: %v", err)
	}
}

// asp doctor runs the node-agent's own checks with the settings of the node: its env file
// and the flags given after --; its exit code is the report's.
func TestDoctorRunsTheNodeAgentWithTheNodesSettings(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "node-agent")
	script := "#!/bin/sh\necho \"args: $*\"\necho \"url: $ASP_CONTROL_PLANE_URL\"\nexit ${FAKE_EXIT:-0}\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(dir, "node-agent.env")
	if err := os.WriteFile(envFile, []byte("ASP_CONTROL_PLANE_URL=https://cp.example:8443\nFAKE_EXIT=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	code := run([]string{"doctor", "--node-agent", fake, "--env-file", envFile, "--json", "--", "--disk-dir=/srv/disks"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d, want the node-agent's 1: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "args: --doctor --disk-dir=/srv/disks --doctor-json") || !strings.Contains(out, "url: https://cp.example:8443") {
		t.Fatalf("the node-agent was run with %q", out)
	}
	// No node-agent to run: say so, exit 2.
	stderr.Reset()
	t.Setenv("PATH", filepath.Join(dir, "nowhere"))
	old := installedNodeAgent
	installedNodeAgent = filepath.Join(dir, "absent")
	t.Cleanup(func() { installedNodeAgent = old })
	if code := run([]string{"doctor", "--env-file", filepath.Join(dir, "none")}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "node-agent not found") {
		t.Fatalf("no binary: %d %q", code, stderr.String())
	}
}
