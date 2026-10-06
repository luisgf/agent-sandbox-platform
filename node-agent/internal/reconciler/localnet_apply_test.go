package reconciler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/cpclient"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/localnet"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

// recordingApplier counts Apply calls and plays the host's key and device check.
type recordingApplier struct {
	mu      sync.Mutex
	applies int
	healthy bool
	plans   map[string]localnet.Plan
}

func (a *recordingApplier) Apply(p localnet.Plan) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.applies++
	p.KeyPath = "/var/lib/asp/local-net/" + p.SandboxID + ".key" // as the host does
	a.plans[p.SandboxID] = p
	return nil
}

func (a *recordingApplier) Clear(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.plans, id)
	return nil
}

func (a *recordingApplier) Current(id string) (localnet.Plan, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.plans[id]
	return p, ok
}

func (a *recordingApplier) EnsureNodeKey(string) (string, int, string, error) {
	return "bm9kZS1wdWJsaWMta2V5LWZvci10ZXN0cy0wMDAwMDA=", 51820, "/k", nil
}

func (a *recordingApplier) Healthy(string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.healthy
}

func (a *recordingApplier) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.applies
}

// The local-net recipe (about 15 processes) runs only when the plan changes;
// the node key is published once; the device is checked every 30 s and the
// plan applied again if it is gone.
func TestLocalNetPlanAppliedOnlyWhenItChanges(t *testing.T) {
	var mu sync.Mutex
	state, lnState, client := "requested", "pending", ""
	publishes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		sb := map[string]any{
			"id": "ln", "state": state, "image_ref": "img", "cpu_millis": 500, "memory_mib": 128,
			"local_net": true, "local_net_state": lnState, "local_net_client_public": client, "owner_sub": "owner-a",
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/work"):
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []any{sb}, "assigned": []string{"ln"}})
		case strings.HasSuffix(r.URL.Path, "/claim"):
			state = "starting"
			sb["state"] = state
			_ = json.NewEncoder(w).Encode(sb)
		case strings.HasSuffix(r.URL.Path, "/status"):
			var body struct {
				State string `json:"state"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			state = body.State
			sb["state"] = state
			_ = json.NewEncoder(w).Encode(sb)
		case strings.HasSuffix(r.URL.Path, "/local-net/node-public"):
			publishes++
			_ = json.NewEncoder(w).Encode(sb)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	app := &recordingApplier{healthy: true, plans: map[string]localnet.Plan{}}
	now := time.Now()
	rec := New(cpclient.New(srv.URL, srv.Client()), "n1", vmm.NewFakeVMM(nil), nil, time.Hour)
	rec.VsockDir = t.TempDir()
	rec.LocalNet = app
	rec.now = func() time.Time { return now }
	pubs := func() int { mu.Lock(); defer mu.Unlock(); return publishes }

	rec.tick(context.Background()) // claim, boot, running
	first := app.count()
	if first == 0 || pubs() != 1 {
		t.Fatalf("first tick: applies=%d publishes=%d", first, pubs())
	}
	rec.tick(context.Background())
	rec.tick(context.Background())
	if app.count() != first || pubs() != 1 {
		t.Fatalf("unchanged ticks: applies %d (was %d), publishes %d", app.count(), first, pubs())
	}

	mu.Lock()
	lnState, client = "up", "Y2xpZW50LXB1YmxpYy1rZXktZm9yLXRlc3RzLTAwMDA="
	mu.Unlock()
	rec.tick(context.Background())
	if app.count() != first+1 || pubs() != 1 {
		t.Fatalf("pending → up: applies %d, publishes %d", app.count(), pubs())
	}
	if p, _ := app.Current("ln"); p.Kind != localnet.KindTunnel {
		t.Fatalf("plan after up: %+v", p)
	}

	now = now.Add(31 * time.Second) // device check: still there
	rec.tick(context.Background())
	if app.count() != first+1 {
		t.Fatalf("healthy device re-applied: %d", app.count())
	}
	app.mu.Lock()
	app.healthy = false
	app.mu.Unlock()
	rec.tick(context.Background()) // not due yet
	if app.count() != first+1 {
		t.Fatalf("checked before it was due: %d", app.count())
	}
	now = now.Add(31 * time.Second)
	rec.tick(context.Background())
	if app.count() != first+2 || pubs() != 1 {
		t.Fatalf("missing device: applies %d, publishes %d", app.count(), pubs())
	}
}
