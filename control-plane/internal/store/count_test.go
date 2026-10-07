package store

import (
	"context"
	"reflect"
	"testing"
)

// The metrics read how many sandboxes each tenant has in each state with one
// grouped query; both stores must agree on it.
func testCountSandboxes(t *testing.T, s Store) {
	t.Helper()
	lifecycleStore(t, s, 0, "node-a")
	ctx := context.Background()
	if got, err := s.CountSandboxes(ctx); err != nil || len(got) != 0 {
		t.Fatalf("empty store: %v %v", got, err)
	}

	a := runningOn(t, s, "node-a")
	b := runningOn(t, s, "node-a")
	if _, err := s.StopSandbox(ctx, b.ID, ""); err != nil { // stopping, then the node says stopped
		t.Fatal(err)
	}
	report(t, s, b.ID, SandboxStopped, "vmm stopped, disk kept")
	runningOn(t, s, "node-a")
	other, err := s.CreateSandbox(ctx, CreateSandboxInput{TenantID: "other", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 512, NodeID: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	_ = a

	got, err := s.CountSandboxes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []SandboxCount{
		{TenantID: "other", State: SandboxRequested, Count: 1},
		{TenantID: "t", State: SandboxRunning, Count: 2},
		{TenantID: "t", State: SandboxStopped, Count: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("counts:\n got %+v\nwant %+v (other=%s)", got, want, other.ID)
	}
}

func TestMemoryCountSandboxes(t *testing.T)   { testCountSandboxes(t, NewMemoryStore()) }
func TestPostgresCountSandboxes(t *testing.T) { testCountSandboxes(t, newPostgresTestStore(t)) }
