package reconciler

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

// A 404 on the work poll means the control plane lost this node: register again,
// at most every unknownNodeEvery.
func TestWorkPoll404RegistersAgain(t *testing.T) {
	f := newFakeCP(t)
	rec := New(f.client(), "ghost", vmm.NewFakeVMM(slog.Default()), slog.Default(), time.Hour)
	calls := 0
	rec.OnUnknownNode = func(context.Context) { calls++ }

	rec.tick(context.Background())
	rec.tick(context.Background())
	if calls != 1 {
		t.Fatalf("OnUnknownNode calls = %d, want 1 (rate limited)", calls)
	}
	rec.lastUnknownNode = time.Now().Add(-unknownNodeEvery)
	rec.tick(context.Background())
	if calls != 2 {
		t.Fatalf("OnUnknownNode calls = %d, want 2 after the window", calls)
	}

	// A known node never triggers it.
	known := New(f.client(), "n1", vmm.NewFakeVMM(slog.Default()), slog.Default(), time.Hour)
	known.OnUnknownNode = func(context.Context) { t.Fatal("called for a known node") }
	known.tick(context.Background())
}
