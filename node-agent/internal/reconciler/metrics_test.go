package reconciler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/metrics"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/poddaemon"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

func scrape(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	var b strings.Builder
	if _, err := reg.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func wantMetrics(t *testing.T, reg *metrics.Registry, wants ...string) {
	t.Helper()
	out := scrape(t, reg)
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
}

// A VM's life shows in the counters: its start and how long it took, the VMs held,
// its crash, and its release with the reason.
func TestReconcilerCountsStartsStopsAndExits(t *testing.T) {
	reg := metrics.NewRegistry()
	cp := newFakeCP(t, idA, idB)
	fake := vmm.NewFakeVMM(nil)
	rec, _ := retainRec(t, cp, fake)
	rec.Metrics = NewMetrics(reg)
	reg.AddCollector(rec.Collector())

	rec.tick(context.Background())
	wantMetrics(t, reg,
		`asp_agent_vm_starts_total{kind="new",result="ok"} 2`,
		`asp_agent_vm_start_seconds_count{kind="new"} 2`,
		"asp_agent_vms 2\n",
	)

	cp.setState(idA, "stopping")
	fake.Crash(idB, errors.New("signal: killed"), time.Minute)
	rec.tick(context.Background())
	wantMetrics(t, reg,
		`asp_agent_vm_stops_total{how="stop"} 1`,
		`asp_agent_vm_exits_total{cause="crash"} 1`,
		`asp_agent_vm_stops_total{how="exited"} 1`,
		"asp_agent_vms 0\n",
	)

	// A resume is its own kind of start.
	cp.mu.Lock()
	cp.boxes[idA].State, cp.boxes[idA].BootCount = "requested", 2
	cp.mu.Unlock()
	rec.tick(context.Background())
	wantMetrics(t, reg, `asp_agent_vm_starts_total{kind="resume",result="ok"} 1`)

	// The guest powering itself off is not a crash.
	fake.Crash(idA, nil, time.Minute)
	rec.tick(context.Background())
	wantMetrics(t, reg, `asp_agent_vm_exits_total{cause="poweroff"} 1`)

	cp.setState(idA, "deleting")
	rec.tick(context.Background())
}

func TestReconcilerCountsAStartThatFailed(t *testing.T) {
	reg := metrics.NewRegistry()
	cp, srv := newRunningCP(t, "guest-silent-metrics")
	eng := &bootingEngine{FakeVMM: vmm.NewFakeVMM(nil), t: t, refuse: 1 << 30}
	rec := newGuestReadyReconciler(t, srv, eng, 300*time.Millisecond)
	rec.Metrics = NewMetrics(reg)
	rec.tick(context.Background())
	if state, _ := cp.running(); state != "failed" {
		t.Fatalf("state %s", state)
	}
	wantMetrics(t, reg,
		`asp_agent_vm_starts_total{kind="new",result="guest_not_ready"} 1`,
		`asp_agent_guest_ready_seconds_count{result="timeout"} 1`,
		`asp_agent_vm_stops_total{how="start_failed"} 1`,
	)
	if strings.Contains(scrape(t, reg), `asp_agent_vm_start_seconds_count`) {
		t.Error("a failed start was timed as a start")
	}
}

func TestReconcilerCountsAGuestThatAnswered(t *testing.T) {
	reg := metrics.NewRegistry()
	_, srv := newRunningCP(t, "guest-ok-metrics")
	eng := &bootingEngine{FakeVMM: vmm.NewFakeVMM(nil), t: t, refuse: 2}
	rec := newGuestReadyReconciler(t, srv, eng, 10*time.Second)
	rec.Metrics = NewMetrics(reg)
	rec.Registry = poddaemon.NewRegistry(nil)
	rec.tick(context.Background())
	wantMetrics(t, reg, `asp_agent_guest_ready_seconds_count{result="ready"} 1`, `asp_agent_vm_starts_total{kind="new",result="ok"} 1`)
}

func TestStartResult(t *testing.T) {
	for err, want := range map[error]string{
		nil: "ok",
		errors.New("start: guest_not_ready: the guest did not answer within 2m0s"): "guest_not_ready",
		errors.New("start: vmm_exited: signal: killed after 3s"):                   "vmm_exited",
		errors.New("vmm start: wait for CH API"):                                   "vmm_start",
		errors.New("report running: 500"):                                          "report",
		errors.New("tap create: ip: operation not permitted"):                      "setup",
		errors.New("claim: 409"):                                                   "setup",
	} {
		if got := startResult(err); got != want {
			t.Errorf("startResult(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestNilMetricsCountNothing(t *testing.T) {
	var m *Metrics
	m.recordStart("new", time.Now(), nil)
	m.recordGuestWait(time.Now(), true)
	m.stop("stop")
	m.exit("crash")
	m.diskRemoved()
	m.pollFailed()
}
