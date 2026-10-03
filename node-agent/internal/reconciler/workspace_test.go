package reconciler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/cpclient"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

func TestWorkspaceSetsFSSocketAndTagWithoutKVM(t *testing.T) {
	host := t.TempDir()
	dir := t.TempDir()
	var stopped atomic.Bool
	var launched atomic.Int32
	fake := vmm.NewFakeVMM(nil)
	rec := newWorkspaceRec(t, fake, dir, cpclient.Sandbox{
		ID: "sb-ws", State: "requested", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 128,
		WorkspaceHostPath: host,
	})
	rec.FSLauncher = func(_ context.Context, id, hostPath, sock string) (func(), error) {
		launched.Add(1)
		if id != "sb-ws" || hostPath != host {
			t.Errorf("launch id=%s host=%s", id, hostPath)
		}
		if !strings.HasSuffix(sock, "virtiofs-sb-ws.sock") {
			t.Errorf("socket=%s", sock)
		}
		if err := os.WriteFile(sock, []byte{}, 0o600); err != nil {
			return nil, err
		}
		return func() { stopped.Store(true); _ = os.Remove(sock) }, nil
	}
	rec.tick(context.Background())

	cfg, ok := fake.RunningConfig("sb-ws")
	if !ok {
		t.Fatal("FakeVMM did not record the sandbox")
	}
	if cfg.WorkspaceHostPath != host {
		t.Fatalf("host=%q", cfg.WorkspaceHostPath)
	}
	tag, sock, has := vmm.WorkspaceFS(cfg)
	if !has || tag != vmm.WorkspaceVirtiofsTag || !strings.Contains(sock, "virtiofs-sb-ws.sock") {
		t.Fatalf("tag=%q sock=%q has=%v", tag, sock, has)
	}
	if launched.Load() != 1 {
		t.Fatalf("launches=%d", launched.Load())
	}
	if err := rec.ensureStopped(context.Background(), cpclient.Sandbox{ID: "sb-ws", State: "stopping"}); err != nil {
		t.Fatal(err)
	}
	if !stopped.Load() {
		t.Fatal("virtiofsd stop was not called")
	}
}

func TestNoWorkspaceOmitsFSOnFakeVMM(t *testing.T) {
	dir := t.TempDir()
	var launched atomic.Int32
	fake := vmm.NewFakeVMM(nil)
	rec := newWorkspaceRec(t, fake, dir, cpclient.Sandbox{
		ID: "sb-plain", State: "requested", ImageRef: "img", CPUMillis: 500, MemoryMiB: 64,
	})
	rec.FSLauncher = func(context.Context, string, string, string) (func(), error) {
		launched.Add(1)
		return func() {}, nil
	}
	rec.tick(context.Background())
	cfg, ok := fake.RunningConfig("sb-plain")
	if !ok {
		t.Fatal("sandbox was not started")
	}
	if _, _, has := vmm.WorkspaceFS(cfg); has {
		t.Fatalf("fs leaked without workspace: %+v", cfg)
	}
	if cfg.WorkspaceFSSocket != "" || cfg.WorkspaceHostPath != "" {
		t.Fatalf("cfg=%+v", cfg)
	}
	if launched.Load() != 0 {
		t.Fatalf("virtiofsd launched without workspace: %d", launched.Load())
	}
}

func TestWorkspaceMissingVirtiofsdFailsStart(t *testing.T) {
	host := t.TempDir()
	dir := t.TempDir()
	var failed atomic.Bool
	fake := vmm.NewFakeVMM(nil)
	rec := newWorkspaceRec(t, fake, dir, cpclient.Sandbox{
		ID: "sb-miss", State: "requested", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 128,
		WorkspaceHostPath: host,
	})
	rec.onStatus = func(state, reason string) {
		if state == "failed" && strings.Contains(reason, "virtiofsd") {
			failed.Store(true)
		}
	}
	rec.VirtiofsdBin = "asp-virtiofsd-missing"
	rec.tick(context.Background())
	if !failed.Load() {
		t.Fatal("expected failed status when virtiofsd is missing")
	}
	if _, started := fake.RunningConfig("sb-miss"); started {
		t.Fatal("VMM started without virtiofsd")
	}
	if len(rec.Handles()) != 0 {
		t.Fatalf("handles=%v", rec.Handles())
	}
}

type wsRec struct {
	*Reconciler
	onStatus func(state, reason string)
}

func newWorkspaceRec(t *testing.T, fake *vmm.FakeVMM, vsockDir string, sb cpclient.Sandbox) *wsRec {
	t.Helper()
	var mu sync.Mutex
	cur := sb
	nid := "n1"
	wr := &wsRec{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/nodes/n1/work":
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []cpclient.Sandbox{cur}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim"):
			cur.NodeID = &nid
			cur.State = "starting"
			_ = json.NewEncoder(w).Encode(cur)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/status"):
			var body struct {
				State  string `json:"state"`
				Detail string `json:"detail"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			cur.State = body.State
			if wr.onStatus != nil {
				wr.onStatus(body.State, body.Detail)
			}
			_ = json.NewEncoder(w).Encode(cur)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/attest"):
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	cp := cpclient.New(srv.URL, srv.Client())
	rec := New(cp, "n1", fake, nil, time.Hour)
	rec.VsockDir = vsockDir
	rec.KernelPath = "/k"
	rec.RootFSPath = "/r"
	wr.Reconciler = rec
	return wr
}

func TestVMConfigRecordsHostPathOnly(t *testing.T) {
	r := &Reconciler{VsockDir: t.TempDir(), KernelPath: "/k", RootFSPath: "/r"}
	cfg := r.vmConfig(cpclient.Sandbox{
		ID: "sb-ws", CPUMillis: 1000, MemoryMiB: 128, WorkspaceHostPath: "/data/proj",
	})
	if cfg.WorkspaceHostPath != "/data/proj" {
		t.Fatalf("host=%q", cfg.WorkspaceHostPath)
	}
	if cfg.WorkspaceFSSocket != "" {
		t.Fatalf("vmConfig must not invent a socket, got %q", cfg.WorkspaceFSSocket)
	}
	if _, err := os.Stat(filepath.Join(r.VsockDir, "virtiofs-sb-ws.sock")); !os.IsNotExist(err) {
		t.Fatalf("vmConfig spawned something: %v", err)
	}
	_ = vmm.WorkspaceGuestMount
}
