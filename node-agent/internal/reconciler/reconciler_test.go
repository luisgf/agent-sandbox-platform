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
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/cpclient"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/poddaemon"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/tap"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

func TestReconcilerClaimStartRunning(t *testing.T) {
	var mu sync.Mutex
	type sb struct {
		ID     string  `json:"id"`
		NodeID *string `json:"node_id"`
		State  string  `json:"state"`
		Image  string  `json:"image_ref"`
		CPU    int     `json:"cpu_millis"`
		Mem    int     `json:"memory_mib"`
	}
	nid := ""
	sandbox := &sb{ID: "sb-1", State: "requested", Image: "img", CPU: 500, Mem: 256}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/nodes/n1/work":
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []any{sandbox}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes/sb-1/claim":
			nid = "n1"
			sandbox.NodeID = &nid
			sandbox.State = "starting"
			_ = json.NewEncoder(w).Encode(sandbox)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes/sb-1/status":
			var body struct {
				State string `json:"state"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			sandbox.State = body.State
			_ = json.NewEncoder(w).Encode(sandbox)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	fake := vmm.NewFakeVMM(nil)
	cp := cpclient.New(srv.URL, srv.Client())
	rec := New(cp, "n1", fake, nil, time.Hour)
	rec.tick(context.Background())

	mu.Lock()
	st := sandbox.State
	mu.Unlock()
	if st != "running" {
		t.Fatalf("want running, got %s", st)
	}
	if len(rec.Handles()) != 1 {
		t.Fatalf("handles=%v", rec.Handles())
	}
	foundStart := false
	for _, c := range fake.Calls {
		if c == "start:sb-1" {
			foundStart = true
			break
		}
	}
	if !foundStart {
		t.Fatalf("FakeVMM calls missing start: %v", fake.Calls)
	}
}

func TestReconcilerUniqueCIDs(t *testing.T) {
	var mu sync.Mutex
	type sb struct {
		ID    string  `json:"id"`
		Node  *string `json:"node_id"`
		State string  `json:"state"`
		Image string  `json:"image_ref"`
		CPU   int     `json:"cpu_millis"`
		Mem   int     `json:"memory_mib"`
	}
	sandboxes := []*sb{
		{ID: "aaa-1111-bbbb", State: "requested", Image: "img", CPU: 1000, Mem: 256},
		{ID: "ccc-2222-dddd", State: "requested", Image: "img", CPU: 1000, Mem: 256},
	}
	byID := map[string]*sb{"aaa-1111-bbbb": sandboxes[0], "ccc-2222-dddd": sandboxes[1]}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/nodes/n1/work":
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": sandboxes})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim"):
			id := stringsBetween(r.URL.Path, "/v1/sandboxes/", "/claim")
			s := byID[id]
			nid := "n1"
			s.Node = &nid
			s.State = "starting"
			_ = json.NewEncoder(w).Encode(s)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/status"):
			id := stringsBetween(r.URL.Path, "/v1/sandboxes/", "/status")
			s := byID[id]
			var body struct {
				State string `json:"state"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.State = body.State
			_ = json.NewEncoder(w).Encode(s)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	fake := vmm.NewFakeVMM(nil)
	cp := cpclient.New(srv.URL, srv.Client())
	reg := poddaemon.NewRegistry(nil)
	rec := New(cp, "n1", fake, nil, time.Hour)
	rec.VsockDir = dir
	rec.Registry = reg
	rec.tick(context.Background())

	h1, ok1 := rec.HandleOf("aaa-1111-bbbb")
	h2, ok2 := rec.HandleOf("ccc-2222-dddd")
	if !ok1 || !ok2 {
		t.Fatalf("handles missing: %v %v", ok1, ok2)
	}
	if h1.CID == h2.CID {
		t.Fatalf("CID collision: both %d", h1.CID)
	}
	if h1.CID < 3 || h2.CID < 3 {
		t.Fatalf("CID reserved range: %d %d", h1.CID, h2.CID)
	}
	if !strings.Contains(h1.VsockPath, "vsock-aaa-1111-bbbb.sock") {
		t.Fatalf("vsock path=%s", h1.VsockPath)
	}
	ep, ok := reg.Lookup("aaa-1111-bbbb")
	if !ok || ep.Mode != poddaemon.ModeHybrid || ep.CID != h1.CID {
		t.Fatalf("registry ep=%+v ok=%v", ep, ok)
	}

	// Stop one and ensure CID can be reused.
	sandboxes[0].State = "stopping"
	rec.tick(context.Background())
	if _, ok := rec.HandleOf("aaa-1111-bbbb"); ok {
		t.Fatal("handle should be gone")
	}
	if reg.Len() != 1 {
		t.Fatalf("registry len=%d", reg.Len())
	}
}

func stringsBetween(s, left, right string) string {
	i := strings.Index(s, left)
	if i < 0 {
		return ""
	}
	i += len(left)
	j := strings.Index(s[i:], right)
	if j < 0 {
		return s[i:]
	}
	return s[i : i+j]
}

func TestReconcilerTapAutoAndSSHLink(t *testing.T) {
	var mu sync.Mutex
	type sb struct {
		ID    string  `json:"id"`
		Node  *string `json:"node_id"`
		State string  `json:"state"`
		Image string  `json:"image_ref"`
		CPU   int     `json:"cpu_millis"`
		Mem   int     `json:"memory_mib"`
	}
	sandbox := &sb{ID: "deadbeef-cafe-0001", State: "requested", Image: "img", CPU: 1000, Mem: 256}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/nodes/n1/work":
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []any{sandbox}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim"):
			nid := "n1"
			sandbox.Node = &nid
			sandbox.State = "starting"
			_ = json.NewEncoder(w).Encode(sandbox)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/status"):
			var body struct {
				State string `json:"state"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			sandbox.State = body.State
			_ = json.NewEncoder(w).Encode(sandbox)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	bridge := filepath.Join(dir, "shared-ssh.sock")
	if err := os.WriteFile(bridge, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}

	recTap := &tap.RecordingRunner{}
	fake := vmm.NewFakeVMM(nil)
	cp := cpclient.New(srv.URL, srv.Client())
	rec := New(cp, "n1", fake, nil, time.Hour)
	rec.VsockDir = dir
	rec.TapAuto = true
	rec.Tap = &tap.Manager{Runner: recTap, SoftFail: false}
	rec.SSHAgentShared = bridge
	rec.tick(context.Background())

	h, ok := rec.HandleOf("deadbeef-cafe-0001")
	if !ok {
		t.Fatal("no handle")
	}
	if h.TapName != "asp-deadbeef" {
		t.Fatalf("tap=%s", h.TapName)
	}
	if len(recTap.Calls) < 3 {
		t.Fatalf("expected tap create calls, got %v", recTap.Calls)
	}
	if h.SSHSock == "" {
		t.Fatal("expected ssh symlink path")
	}
	target, err := os.Readlink(h.SSHSock)
	if err != nil || target != bridge {
		t.Fatalf("symlink=%s err=%v", target, err)
	}

	// Stop cleans tap + symlink
	sandbox.State = "stopping"
	rec.tick(context.Background())
	if _, err := os.Lstat(h.SSHSock); !os.IsNotExist(err) {
		t.Fatalf("symlink should be gone: %v", err)
	}
	foundDel := false
	for _, c := range recTap.Calls {
		if strings.Contains(c, "link delete") {
			foundDel = true
		}
	}
	if !foundDel {
		t.Fatalf("expected tap delete in %v", recTap.Calls)
	}
}
