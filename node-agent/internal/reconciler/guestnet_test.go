package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/attest"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/cpclient"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/egress"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/tap"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

// Two sandboxes on one node must get different /30s: a shared prefix routes
// both guests' replies to one TAP and makes their egress indistinguishable.
func TestReconcilerGivesEachTapItsOwnSubnet(t *testing.T) {
	cp := newFakeCP(t, "aaaaaaaa-0001", "bbbbbbbb-0002")

	recTap := &tap.RecordingRunner{}
	fake := vmm.NewFakeVMM(nil)
	cache := &egress.PolicyCache{}
	rec := New(cp.client(), "n1", fake, nil, time.Hour)
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
	cp.setState(a, "stopping")
	rec.tick(context.Background())
	if got := cache.SandboxFor(netip.MustParseAddr("10.200.0.2")); got != "" {
		t.Fatalf("stopped sandbox still bound to %q", got)
	}
	if _, gnet, err := rec.allocSlot(); err != nil || gnet.HostCIDR() != "10.200.0.1/30" {
		t.Fatalf("freed slot not reused: %v %v", gnet.HostCIDR(), err)
	}
}

// fakeCP is a minimal control plane for reconciler tests: work, claim, status.
type fakeCP struct {
	t     *testing.T
	srv   *httptest.Server
	mu    sync.Mutex
	boxes map[string]*fakeSandbox
	// conflict makes status=running answer 409 for a sandbox, as the control
	// plane does once it no longer assigns it to this node.
	conflict map[string]bool
	// legacy omits the assigned set from /work, like a control plane that
	// predates it.
	legacy bool
	// retains sends the retained list (the ids of stopped sandboxes) with the
	// work poll, as a control plane that keeps stopped sandboxes' disks does.
	retains bool
	// requests counts calls by "METHOD path".
	requests map[string]int
	// egress, when set, is sent as each tenant's policy with the work poll.
	egress map[string]cpclient.EgressPolicy
	// attests holds the boot attestations posted for each sandbox, oldest first.
	attests map[string][]attest.Evidence
}

type fakeSandbox struct {
	ID     string  `json:"id"`
	Tenant string  `json:"tenant_id"`
	Node   *string `json:"node_id"`
	State  string  `json:"state"`
	Detail string  `json:"-"`
	// BootCount is sent when set: 2 and up is a resume.
	BootCount int `json:"boot_count,omitempty"`
}

func newFakeCP(t *testing.T, ids ...string) *fakeCP {
	f := &fakeCP{t: t, boxes: map[string]*fakeSandbox{}, conflict: map[string]bool{}, requests: map[string]int{}, attests: map[string][]attest.Evidence{}}
	for _, id := range ids {
		f.boxes[id] = &fakeSandbox{ID: id, Tenant: "t1", State: "requested"}
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCP) client() *cpclient.Client { return cpclient.New(f.srv.URL, f.srv.Client()) }

// lastAttest is the newest attestation posted for id.
func (f *fakeCP) lastAttest(id string) (attest.BootStatement, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.attests[id]
	if len(list) == 0 {
		return attest.BootStatement{}, false
	}
	return list[len(list)-1].Statement, true
}

func (f *fakeCP) setState(id, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.boxes[id].State = state
}

func (f *fakeCP) state(id string) (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.boxes[id].State, f.boxes[id].Detail
}

func (f *fakeCP) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	f.requests[r.Method+" "+r.URL.Path]++
	parts := strings.Split(r.URL.Path, "/")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/nodes/n1/work":
		list := []any{}
		assigned := []string{}
		retained := []string{}
		for _, b := range f.boxes {
			switch b.State {
			case "requested", "starting", "stopping":
				list = append(list, b)
				assigned = append(assigned, b.ID)
			case "running", "paused":
				assigned = append(assigned, b.ID)
			case "deleting":
				list = append(list, b) // needs the node, holds no capacity
			case "stopped":
				retained = append(retained, b.ID)
			}
		}
		resp := map[string]any{"sandboxes": list}
		if !f.legacy {
			resp["assigned"] = assigned
		}
		if f.retains {
			resp["retained"] = retained
		}
		if f.egress != nil {
			tenants := map[string]string{}
			for _, id := range assigned {
				tenants[id] = f.boxes[id].Tenant
			}
			resp["egress"] = map[string]any{"tenants": tenants, "policies": f.egress}
		}
		_ = json.NewEncoder(w).Encode(resp)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim"):
		b := f.boxes[parts[3]]
		nid := "n1"
		b.Node, b.State = &nid, "starting"
		_ = json.NewEncoder(w).Encode(b)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/attest"):
		var ev attest.Evidence
		_ = json.NewDecoder(r.Body).Decode(&ev)
		f.attests[parts[3]] = append(f.attests[parts[3]], ev)
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/status"):
		var body struct {
			State  string `json:"state"`
			Detail string `json:"detail"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		cur := f.boxes[parts[3]].State
		if body.State == "running" && (f.conflict[parts[3]] || cur == "stopping" || cur == "stopped" || cur == "failed") {
			// Like the control plane's ValidAgentTransition.
			http.Error(w, `{"error":"conflict: cannot move sandbox from `+cur+` to running"}`, http.StatusConflict)
			return
		}
		b := f.boxes[parts[3]]
		b.State, b.Detail = body.State, body.Detail
		_ = json.NewEncoder(w).Encode(b)
	default:
		http.NotFound(w, r)
	}
}

// Outside dry-run a TAP that cannot be created fails the sandbox: the VM does
// not boot without its network, and the /30 and the CID are released.
func TestReconcilerFailsTheSandboxWhenTheTapCannotBeCreated(t *testing.T) {
	cp := newFakeCP(t, "cccccccc-0003")
	fake := vmm.NewFakeVMM(nil)
	rec := New(cp.client(), "n1", fake, nil, time.Hour)
	rec.VsockDir = t.TempDir()
	rec.TapAuto = true
	rec.Tap = &tap.Manager{Runner: &tap.RecordingRunner{InjectedErr: errTapDenied}, SysClassNet: t.TempDir()}
	rec.Egress = &egress.PolicyCache{}
	rec.tick(context.Background())

	state, detail := cp.state("cccccccc-0003")
	if state != "failed" || !strings.Contains(detail, "tap:") {
		t.Fatalf("want failed with a tap detail, got %s %q", state, detail)
	}
	if len(rec.Handles()) != 0 || len(fake.Running) != 0 {
		t.Fatalf("no VM may run without its TAP: handles=%v running=%v", rec.Handles(), fake.Running)
	}
	if n, gnet, err := rec.allocSlot(); err != nil || n != 0 || gnet.HostCIDR() != "10.200.0.1/30" {
		t.Fatalf("slot not released: %d %v %v", n, gnet.HostCIDR(), err)
	}
	if cid := rec.allocCID(); cid != 3 {
		t.Fatalf("CID not released: got %d", cid)
	}
}

var errTapDenied = errors.New("ip: operation not permitted")
