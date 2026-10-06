package reconciler

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

// A sandbox the control plane stops assigning to this node (failed over after
// a node loss, destroyed) is torn down on the next tick: two copies must not
// run. A sandbox still assigned is left alone.
func TestUnassignedSandboxIsStoppedWithinOneTick(t *testing.T) {
	cp := newFakeCP(t, "s1", "s2")
	fake := vmm.NewFakeVMM(nil)
	rec := New(cp.client(), "n1", fake, nil, time.Hour)
	rec.VsockDir = t.TempDir()

	rec.tick(context.Background())
	for _, id := range []string{"s1", "s2"} {
		if _, ok := rec.HandleOf(id); !ok {
			t.Fatalf("%s should be running locally", id)
		}
	}
	cp.setState("s1", "failed")

	rec.tick(context.Background())
	if _, ok := rec.HandleOf("s1"); ok {
		t.Fatal("s1 still runs after the control plane stopped assigning it here")
	}
	if !slices.Contains(fake.Calls, "stop:s1") {
		t.Fatalf("VMM never stopped s1: %v", fake.Calls)
	}
	if st, _ := cp.state("s1"); st != "failed" {
		t.Fatalf("self-fencing must not report a state, got %s", st)
	}
	if _, ok := rec.HandleOf("s2"); !ok || slices.Contains(fake.Calls, "stop:s2") {
		t.Fatal("s2 is still assigned and must keep running")
	}
}

// Once everything runs, a tick is one work poll: no per-sandbox calls.
func TestSteadyStateIsOneWorkPoll(t *testing.T) {
	cp := newFakeCP(t, "s1", "s2", "s3")
	rec := New(cp.client(), "n1", vmm.NewFakeVMM(nil), nil, time.Hour)
	rec.VsockDir = t.TempDir()
	rec.tick(context.Background())

	cp.mu.Lock()
	cp.requests = map[string]int{}
	cp.mu.Unlock()
	for i := 0; i < 3; i++ {
		rec.tick(context.Background())
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	for req, n := range cp.requests {
		if req != "GET /v1/nodes/n1/work" {
			t.Errorf("steady state made %d %q requests", n, req)
		}
	}
	if cp.requests["GET /v1/nodes/n1/work"] != 3 {
		t.Fatalf("want 3 work polls, got %v", cp.requests)
	}
}

// A control plane that does not send the assigned set must not make the node
// stop everything.
func TestWorkWithoutTheAssignedSetStopsNothing(t *testing.T) {
	cp := newFakeCP(t, "s1")
	cp.legacy = true
	fake := vmm.NewFakeVMM(nil)
	rec := New(cp.client(), "n1", fake, nil, time.Hour)
	rec.VsockDir = t.TempDir()
	rec.tick(context.Background())
	rec.tick(context.Background())
	if _, ok := rec.HandleOf("s1"); !ok {
		t.Fatal("s1 was stopped although the control plane sent no assigned set")
	}
	for _, c := range fake.Calls {
		if strings.HasPrefix(c, "stop:") {
			t.Fatalf("unexpected stop: %v", fake.Calls)
		}
	}
}

// A boot that outlives its assignment is torn down when the running report is refused.
func TestRunningReportConflictStopsTheLocalVM(t *testing.T) {
	cp := newFakeCP(t, "s2")
	cp.conflict["s2"] = true
	fake := vmm.NewFakeVMM(nil)
	rec := New(cp.client(), "n1", fake, nil, time.Hour)
	rec.VsockDir = t.TempDir()

	rec.tick(context.Background())
	if _, ok := rec.HandleOf("s2"); ok {
		t.Fatal("s2 kept running after its running report was refused")
	}
	if !slices.Contains(fake.Calls, "start:s2") || !slices.Contains(fake.Calls, "stop:s2") {
		t.Fatalf("want start then stop, got %v", fake.Calls)
	}
}
