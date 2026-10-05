package reconciler

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

// When the control plane refuses the lease (the sandbox was failed over or
// destroyed), the node stops its local VM: two copies must not run.
func TestRenewConflictStopsTheLocalVM(t *testing.T) {
	cp := newFakeCP(t, "s1")
	fake := vmm.NewFakeVMM(nil)
	rec := New(cp.client(), "n1", fake, nil, time.Hour)
	rec.VsockDir = t.TempDir()

	rec.tick(context.Background())
	if _, ok := rec.HandleOf("s1"); !ok {
		t.Fatal("s1 should be running locally")
	}
	cp.mu.Lock()
	cp.conflict["s1"] = true
	cp.mu.Unlock()
	cp.setState("s1", "failed")

	rec.tick(context.Background())
	if _, ok := rec.HandleOf("s1"); ok {
		t.Fatal("s1 still runs after the control plane refused its lease")
	}
	if !slices.Contains(fake.Calls, "stop:s1") {
		t.Fatalf("VMM never stopped s1: %v", fake.Calls)
	}
	if st, _ := cp.state("s1"); st != "failed" {
		t.Fatalf("self-fencing must not report a state, got %s", st)
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
