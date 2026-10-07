package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/client"
	"github.com/luisgf/agent-sandbox-platform/cli/internal/session"
)

// A sandbox stopped because its node's agent restarted (or its node was lost)
// keeps its disk: exec and status tell the user to resume it, and never to start
// a new one with --force, which would delete that disk.
func TestStoppedByANodeEventIsResumableAndSaysSo(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	for reason, cause := range map[string]string{
		client.StopReasonAgentRestarted: "when the node agent restarted",
		client.StopReasonNodeLost:       "when its node stopped responding",
	} {
		node := "node-b"
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(client.Sandbox{ID: "sb", State: "stopped", NodeID: &node, StopReason: reason})
		})
		mux.HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"sandbox was stopped"}`))
		})
		srv := httptest.NewServer(mux)
		sessFile := filepath.Join(t.TempDir(), "session.json")
		if err := session.Save(sessFile, session.State{SandboxID: "sb", CPURL: srv.URL}); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{
			{"session", "exec", "--session-file", sessFile, "--buffered", "--cmd", "true"},
			{"session", "status", "--session-file", sessFile},
		} {
			var stdout, stderr strings.Builder
			code := run(args, &stdout, &stderr)
			out := stderr.String()
			if !strings.Contains(out, cause) || !strings.Contains(out, "disk is kept on node node-b") || !strings.Contains(out, "asp session resume") || strings.Contains(out, "--force") {
				t.Errorf("%s %v: exit=%d stderr=%q", reason, args[1], code, out)
			}
			if args[1] == "status" && (code != 0 || strings.Contains(stdout.String(), "lost_with_node")) {
				t.Errorf("%s status: a resumable sandbox is not lost: exit=%d stdout=%q", reason, code, stdout.String())
			}
		}
		srv.Close()
	}
	// Failed with the same reasons is lost for good: the sandbox state decides.
	if (client.Sandbox{State: "failed", StopReason: client.StopReasonAgentRestarted}).LostWithNode() != true ||
		(client.Sandbox{State: "stopped", StopReason: client.StopReasonAgentRestarted}).LostWithNode() {
		t.Fatal("LostWithNode must follow the state, not only the reason")
	}
}

// --force deletes whatever the session file names. For a stopped sandbox whose
// disk is kept that is the one thing that cannot be undone, so it takes --yes.
func TestSessionStartForceRefusesAStoppedSandboxUnlessYes(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	state := "stopped"
	var deletes, creates atomic.Int32
	node := "n1"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: r.PathValue("id"), State: state, NodeID: &node})
	})
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		deletes.Add(1)
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: r.PathValue("id"), State: "deleting"})
	})
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		creates.Add(1)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: "new-1", State: "running", TenantID: "t"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("ASP_CONTROL_PLANE_URL", srv.URL) // where the new sandbox goes
	sessFile := filepath.Join(t.TempDir(), "session.json")
	save := func() {
		if err := session.Save(sessFile, session.State{SandboxID: "old-1", CPURL: srv.URL}); err != nil {
			t.Fatal(err)
		}
	}
	save()

	var stdout, stderr strings.Builder
	code := run([]string{"session", "start", "--force", "--session-file", sessFile, "--timeout", "2s"}, &stdout, &stderr)
	if code != 1 || deletes.Load() != 0 || creates.Load() != 0 ||
		!strings.Contains(stderr.String(), "stopped with its disk kept on node n1") || !strings.Contains(stderr.String(), "asp session resume") || !strings.Contains(stderr.String(), "--yes") {
		t.Fatalf("exit=%d deletes=%d creates=%d stderr=%q", code, deletes.Load(), creates.Load(), stderr.String())
	}
	if st, err := session.Load(sessFile); err != nil || st.SandboxID != "old-1" {
		t.Fatalf("the refused --force touched the session file: %+v %v", st, err)
	}

	// --yes is the user saying so.
	stderr.Reset()
	code = run([]string{"session", "start", "--force", "--yes", "--session-file", sessFile, "--timeout", "2s"}, &stdout, &stderr)
	if code != 0 || deletes.Load() != 1 || creates.Load() != 1 {
		t.Fatalf("--yes: exit=%d deletes=%d creates=%d stderr=%q", code, deletes.Load(), creates.Load(), stderr.String())
	}

	// A sandbox that is not stopped needs no confirmation: --force is its
	// documented job (running, failed).
	save()
	state = "failed"
	code = run([]string{"session", "start", "--force", "--session-file", sessFile, "--timeout", "2s"}, &stdout, &stderr)
	if code != 0 || deletes.Load() != 2 || creates.Load() != 2 {
		t.Fatalf("failed sandbox: exit=%d deletes=%d creates=%d stderr=%q", code, deletes.Load(), creates.Load(), stderr.String())
	}
}

// The sandbox the session file names lives on the control plane the file
// names. --force deletes it there even when ASP_CP_URL points elsewhere now; the
// new sandbox goes where the environment says.
func TestSessionStartForceDeletesOnTheSessionsControlPlane(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	var oldDeletes, oldCreates, newDeletes, newCreates atomic.Int32
	oldCP := http.NewServeMux()
	oldCP.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: r.PathValue("id"), State: "failed"})
	})
	oldCP.HandleFunc("DELETE /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		oldDeletes.Add(1)
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: r.PathValue("id"), State: "deleting"})
	})
	oldCP.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) { oldCreates.Add(1) })
	newCP := http.NewServeMux()
	newCP.HandleFunc("DELETE /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		newDeletes.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"sandbox not found"}`))
	})
	newCP.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		newCreates.Add(1)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: "new-1", State: "running", TenantID: "t"})
	})
	oldSrv, newSrv := httptest.NewServer(oldCP), httptest.NewServer(newCP)
	defer oldSrv.Close()
	defer newSrv.Close()
	t.Setenv("ASP_CONTROL_PLANE_URL", newSrv.URL)
	sessFile := filepath.Join(t.TempDir(), "session.json")
	if err := session.Save(sessFile, session.State{SandboxID: "old-1", CPURL: oldSrv.URL}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	code := run([]string{"session", "start", "--force", "--session-file", sessFile, "--timeout", "2s"}, &stdout, &stderr)
	if code != 0 || oldDeletes.Load() != 1 || newDeletes.Load() != 0 || oldCreates.Load() != 0 || newCreates.Load() != 1 {
		t.Fatalf("exit=%d old: del=%d new=%d; new: del=%d new=%d; stderr=%q",
			code, oldDeletes.Load(), oldCreates.Load(), newDeletes.Load(), newCreates.Load(), stderr.String())
	}
}

// A sandbox whose VM's process ended while it ran (a crash, an OOM kill, the
// guest powering itself off) is stopped, not lost: the text says how it ended and
// that the disk is kept, and does not call it a failed resume.
func TestStoppedTextSaysTheVMEndedOnItsOwn(t *testing.T) {
	node := "node-b"
	sb := client.Sandbox{ID: "sb-1", State: "stopped", NodeID: &node, StopReason: client.StopReasonVMMExited,
		StatusDetail: "vmm_exited: signal: killed after 3m12s"}
	got := stoppedText(sb, "/f")
	for _, want := range []string{
		"was stopped because its VM ended on its own (signal: killed after 3m12s)",
		"its disk is kept on node node-b", "asp session resume",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("text %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "last resume failed") || strings.Contains(got, "vmm_exited") {
		t.Errorf("text %q reads like a failed resume or leaks the raw prefix", got)
	}
	if sb.LostWithNode() || sb.StoppedByNodeEvent() {
		t.Error("an exited VM is not a node event")
	}
}

// A deleting sandbox holds its node capacity until the node reports it deleted, so
// --force waits for that before it creates the new sandbox: otherwise the create
// could be refused for the room the old one is about to free.
func TestSessionStartForceWaitsForTheOldSandboxToBeGone(t *testing.T) {
	t.Setenv("ASP_REQUIRE_TOKEN", "")
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	var mu sync.Mutex
	var calls []string
	gets := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gets++
		state := "running" // the guard before the delete
		switch {
		case gets > 1 && gets <= 3:
			state = "deleting" // the node has not removed the VM yet
		case gets > 3:
			state = "deleted"
		}
		calls = append(calls, "GET:"+state)
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: r.PathValue("id"), State: state})
	})
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, "DELETE")
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: r.PathValue("id"), State: "deleting"})
	})
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, "CREATE")
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(client.Sandbox{ID: "new-1", State: "running", TenantID: "t"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("ASP_CONTROL_PLANE_URL", srv.URL)
	sessFile := filepath.Join(t.TempDir(), "session.json")
	if err := session.Save(sessFile, session.State{SandboxID: "old-1", CPURL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	if code := run([]string{"session", "start", "--force", "--session-file", sessFile, "--timeout", "5s"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	mu.Lock()
	defer mu.Unlock()
	got := strings.Join(calls, ",")
	if !strings.HasSuffix(got, "GET:deleted,CREATE") || !strings.Contains(got, "DELETE,GET:deleting") {
		t.Fatalf("calls: %s", got)
	}
}
