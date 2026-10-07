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
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/workspace"
)

func TestWorkspaceSetsFSSocketAndTagWithoutKVM(t *testing.T) {
	roots, host := workspaceUnder(t, "acme")
	dir := t.TempDir()
	var stopped atomic.Bool
	var launched atomic.Int32
	fake := vmm.NewFakeVMM(nil)
	rec := newWorkspaceRec(t, fake, dir, cpclient.Sandbox{
		ID: "sb-ws", TenantID: "acme", State: "requested", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 128,
		WorkspaceHostPath: host,
	})
	rec.WorkspaceRoots = roots
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
	roots, host := workspaceUnder(t, "acme")
	dir := t.TempDir()
	var failed atomic.Bool
	fake := vmm.NewFakeVMM(nil)
	rec := newWorkspaceRec(t, fake, dir, cpclient.Sandbox{
		ID: "sb-miss", TenantID: "acme", State: "requested", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 128,
		WorkspaceHostPath: host,
	})
	rec.WorkspaceRoots = roots
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

// workspaceUnder makes <root>/<tenant>/proj, as an operator would for a
// tenant, and returns the roots to configure and the workspace path.
func workspaceUnder(t *testing.T, tenant string) (workspace.Roots, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(root, tenant, "proj")
	if err := os.MkdirAll(host, 0o755); err != nil {
		t.Fatal(err)
	}
	return workspace.Roots{root}, host
}

// A workspace outside the tenant's directory under a workspace root is refused
// before virtiofsd or the VMM start, and the sandbox fails with the reason.
func TestWorkspaceOutsideTheRootsFailsStart(t *testing.T) {
	roots, _ := workspaceUnder(t, "acme")
	other := t.TempDir() // a directory that exists, but not under any root
	_, othersProj := workspaceUnder(t, "acme")
	for name, tc := range map[string]struct{ tenant, host string }{
		"the node's root":      {"acme", "/"},
		"an unrelated dir":     {"acme", other},
		"another root's dir":   {"acme", othersProj},
		"another tenant's dir": {"rival", filepath.Join(string(roots[0]), "acme", "proj")},
		"a tenant with a path": {"../acme", filepath.Join(string(roots[0]), "acme", "proj")},
	} {
		t.Run(name, func(t *testing.T) {
			var reason atomic.Value
			var launched atomic.Int32
			fake := vmm.NewFakeVMM(nil)
			rec := newWorkspaceRec(t, fake, t.TempDir(), cpclient.Sandbox{
				ID: "sb-out", TenantID: tc.tenant, State: "requested", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 128,
				WorkspaceHostPath: tc.host,
			})
			rec.WorkspaceRoots = roots
			rec.FSLauncher = func(context.Context, string, string, string) (func(), error) {
				launched.Add(1)
				return func() {}, nil
			}
			rec.onStatus = func(state, why string) {
				if state == "failed" {
					reason.Store(why)
				}
			}
			rec.tick(context.Background())
			if launched.Load() != 0 {
				t.Fatal("virtiofsd was started for a workspace outside the roots")
			}
			if _, started := fake.RunningConfig("sb-out"); started {
				t.Fatal("the VM started")
			}
			if got, _ := reason.Load().(string); !strings.Contains(got, "workspace") {
				t.Fatalf("failure reason %q does not explain the refusal", got)
			}
		})
	}
}

// With no root configured no sandbox has a workspace, whatever the path.
func TestWorkspaceWithoutRootsFailsStart(t *testing.T) {
	_, host := workspaceUnder(t, "acme")
	var reason atomic.Value
	fake := vmm.NewFakeVMM(nil)
	rec := newWorkspaceRec(t, fake, t.TempDir(), cpclient.Sandbox{
		ID: "sb-none", TenantID: "acme", State: "requested", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 128,
		WorkspaceHostPath: host,
	})
	rec.onStatus = func(state, why string) {
		if state == "failed" {
			reason.Store(why)
		}
	}
	rec.tick(context.Background())
	if got, _ := reason.Load().(string); !strings.Contains(got, "--workspace-root") {
		t.Fatalf("failure reason %q does not name the flag", got)
	}
}

// What virtiofsd shares is the resolved path, not the one in the spec.
func TestWorkspaceSharesTheResolvedPath(t *testing.T) {
	roots, host := workspaceUnder(t, "acme")
	link := filepath.Join(string(roots[0]), "acme", "alias")
	if err := os.Symlink(host, link); err != nil {
		t.Fatal(err)
	}
	var shared string
	fake := vmm.NewFakeVMM(nil)
	rec := newWorkspaceRec(t, fake, t.TempDir(), cpclient.Sandbox{
		ID: "sb-link", TenantID: "acme", State: "requested", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 128,
		WorkspaceHostPath: link,
	})
	rec.WorkspaceRoots = roots
	rec.FSLauncher = func(_ context.Context, _, hostPath, sock string) (func(), error) {
		shared = hostPath
		return func() {}, os.WriteFile(sock, nil, 0o600)
	}
	rec.tick(context.Background())
	if shared != host {
		t.Fatalf("virtiofsd shares %q, want the resolved %q", shared, host)
	}
}
