package reconciler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/cpclient"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/egress"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/tap"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

// Two sandboxes on one node must get different /30s: a shared prefix routes
// both guests' replies to one TAP and makes their egress indistinguishable.
func TestReconcilerGivesEachTapItsOwnSubnet(t *testing.T) {
	type sb struct {
		ID    string  `json:"id"`
		Node  *string `json:"node_id"`
		State string  `json:"state"`
	}
	var mu sync.Mutex
	boxes := map[string]*sb{
		"aaaaaaaa-0001": {ID: "aaaaaaaa-0001", State: "requested"},
		"bbbbbbbb-0002": {ID: "bbbbbbbb-0002", State: "requested"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(r.URL.Path, "/")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/nodes/n1/work":
			list := []any{}
			for _, b := range boxes {
				if b.State != "running" && b.State != "stopped" {
					list = append(list, b)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": list})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim"):
			b := boxes[parts[3]]
			nid := "n1"
			b.Node, b.State = &nid, "starting"
			_ = json.NewEncoder(w).Encode(b)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/status"):
			var body struct {
				State string `json:"state"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			b := boxes[parts[3]]
			b.State = body.State
			_ = json.NewEncoder(w).Encode(b)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	recTap := &tap.RecordingRunner{}
	fake := vmm.NewFakeVMM(nil)
	cache := &egress.PolicyCache{}
	rec := New(cpclient.New(srv.URL, srv.Client()), "n1", fake, nil, time.Hour)
	rec.VsockDir = t.TempDir()
	rec.TapAuto = true
	rec.Tap = &tap.Manager{Runner: recTap}
	rec.Egress = cache
	rec.tick(context.Background())

	var addrs []string
	for _, c := range recTap.Calls {
		if strings.Contains(c, "addr add") {
			addrs = append(addrs, c)
		}
	}
	if len(addrs) != 2 {
		t.Fatalf("want 2 addr add calls, got %v", recTap.Calls)
	}
	joined := strings.Join(addrs, "\n")
	for _, want := range []string{"10.200.0.1/30", "10.200.0.5/30"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in %v", want, addrs)
		}
	}

	ips := map[string]bool{}
	for _, cfg := range fake.Configs {
		i := strings.Index(cfg.Cmdline, "ip=")
		if i < 0 {
			t.Fatalf("cmdline without ip=: %q", cfg.Cmdline)
		}
		ips[cfg.Cmdline[i:]] = true
	}
	if len(ips) != 2 {
		t.Fatalf("guests must get distinct ip= args: %v", ips)
	}

	a := cache.SandboxFor(netip.MustParseAddr("10.200.0.2"))
	b := cache.SandboxFor(netip.MustParseAddr("10.200.0.6"))
	if a == "" || b == "" || a == b {
		t.Fatalf("proxy bindings: 10.200.0.2→%q 10.200.0.6→%q", a, b)
	}

	// Stopping one sandbox frees its /30 and its proxy binding.
	mu.Lock()
	boxes[a].State = "stopping"
	mu.Unlock()
	rec.tick(context.Background())
	if got := cache.SandboxFor(netip.MustParseAddr("10.200.0.2")); got != "" {
		t.Fatalf("stopped sandbox still bound to %q", got)
	}
	if _, gnet, err := rec.allocSlot(); err != nil || gnet.HostCIDR() != "10.200.0.1/30" {
		t.Fatalf("freed slot not reused: %v %v", gnet.HostCIDR(), err)
	}
}
