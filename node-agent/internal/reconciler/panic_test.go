package reconciler

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

// panicEngine is a FakeVMM that panics where a test tells it to.
type panicEngine struct {
	*vmm.FakeVMM
	mu        sync.Mutex
	panicOn   map[string]bool // sandbox ids whose Start panics
	panicStop map[string]bool // sandbox ids whose first Stop panics
	stops     map[string]int
}

func newPanicEngine() *panicEngine {
	return &panicEngine{FakeVMM: vmm.NewFakeVMM(nil), panicOn: map[string]bool{}, panicStop: map[string]bool{}, stops: map[string]int{}}
}

func (e *panicEngine) Start(ctx context.Context, cfg vmm.MicroVMConfig) error {
	e.mu.Lock()
	boom := e.panicOn[cfg.ID]
	e.mu.Unlock()
	if boom {
		nilMapWrite(nil)
	}
	return e.FakeVMM.Start(ctx, cfg)
}

func (e *panicEngine) Stop(ctx context.Context, id string) error {
	e.mu.Lock()
	e.stops[id]++
	first := e.stops[id] == 1
	boom := e.panicStop[id] && first
	e.mu.Unlock()
	if boom {
		panic("stop blew up")
	}
	return e.FakeVMM.Stop(ctx, id)
}

func panicRec(t *testing.T, cp *fakeCP, eng vmm.MicroVM) (*Reconciler, *bytes.Buffer) {
	t.Helper()
	rec, _ := retainRec(t, cp, eng)
	var logs bytes.Buffer
	rec.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	return rec, &logs
}

// A panic in a VMM call ends that sandbox's start, not the agent: the sandbox is
// reported failed with the panic as the reason, the stack is logged once, and the
// next sandbox and the next poll are handled.
func TestAStartThatPanicsFailsOnlyThatSandbox(t *testing.T) {
	cp := newFakeCP(t, idA, idB)
	eng := newPanicEngine()
	eng.panicOn[idA] = true
	rec, logs := panicRec(t, cp, eng)
	rec.tick(context.Background()) // would crash the test binary without recovery

	got, detail := cp.state(idA)
	if got != "failed" || !strings.HasPrefix(detail, "panic:") || !strings.Contains(detail, "nil map") {
		t.Fatalf("panicking sandbox: state=%s detail=%q", got, detail)
	}
	if got, _ := cp.state(idB); got != "running" {
		t.Fatalf("the other sandbox: state=%s, want running", got)
	}
	out := logs.String()
	if strings.Count(out, "reconciler worker panicked") != 1 || !strings.Contains(out, "sandbox_id="+idA) || !strings.Contains(out, "panic_test.go") {
		t.Fatalf("log: %s", out)
	}
	// The failed first boot keeps no disk, and the agent still works.
	rec.tick(context.Background())
	if got, _ := cp.state(idB); got != "running" {
		t.Fatalf("after another poll: %s", got)
	}
	if h := rec.Handles(); len(h) != 1 {
		t.Fatalf("handles after the panic: %v", h)
	}
}

// A resume that panics goes back to stopped with its disk, like any failed resume.
func TestAResumeThatPanicsReturnsToStoppedWithItsDisk(t *testing.T) {
	cp := newFakeCP(t, idA)
	eng := newPanicEngine()
	rec, _ := panicRec(t, cp, eng)
	rec.tick(context.Background())
	if got, _ := cp.state(idA); got != "running" {
		t.Fatal(got)
	}
	cp.setState(idA, "stopping")
	rec.tick(context.Background())
	disks := rec.DiskDir

	eng.mu.Lock()
	eng.panicOn[idA] = true
	eng.mu.Unlock()
	cp.mu.Lock()
	cp.boxes[idA].State, cp.boxes[idA].BootCount = "requested", 2
	cp.mu.Unlock()
	rec.tick(context.Background())

	got, detail := cp.state(idA)
	if got != "stopped" || !strings.HasPrefix(detail, "panic:") {
		t.Fatalf("state=%s detail=%q, want stopped with a panic detail", got, detail)
	}
	if !exists(diskOf(disks, idA)) {
		t.Fatal("the disk of a resume that panicked was removed")
	}
}

// A stop whose VMM call panics still ends with the VM torn down and the sandbox
// stopped, disk kept.
func TestAStopThatPanicsStillStopsTheSandbox(t *testing.T) {
	cp := newFakeCP(t, idA)
	eng := newPanicEngine()
	rec, _ := panicRec(t, cp, eng)
	rec.tick(context.Background())
	eng.mu.Lock()
	eng.panicStop[idA] = true
	eng.mu.Unlock()
	cp.setState(idA, "stopping")
	rec.tick(context.Background())

	got, detail := cp.state(idA)
	if got != "stopped" || !strings.HasPrefix(detail, "panic:") {
		t.Fatalf("state=%s detail=%q", got, detail)
	}
	if len(rec.Handles()) != 0 || !exists(diskOf(rec.DiskDir, idA)) {
		t.Fatalf("handles=%v disk=%v", rec.Handles(), exists(diskOf(rec.DiskDir, idA)))
	}
}

// A delete that panics is reported deleted, with the disk removed.
func TestADeleteThatPanicsStillDeletes(t *testing.T) {
	cp := newFakeCP(t, idA)
	eng := newPanicEngine()
	rec, _ := panicRec(t, cp, eng)
	rec.tick(context.Background())
	eng.mu.Lock()
	eng.panicStop[idA] = true
	eng.mu.Unlock()
	cp.setState(idA, "deleting")
	rec.tick(context.Background())

	if got, detail := cp.state(idA); got != "deleted" || !strings.HasPrefix(detail, "panic:") {
		t.Fatalf("state=%s detail=%q", got, detail)
	}
	if exists(diskOf(rec.DiskDir, idA)) || len(rec.Handles()) != 0 {
		t.Fatalf("disk=%v handles=%v", exists(diskOf(rec.DiskDir, idA)), rec.Handles())
	}
}

// A panic in the poll itself (here the callback that registers the node again)
// costs that poll: the loop keeps going.
func TestAPanicInThePollDoesNotKillTheLoop(t *testing.T) {
	cp := newFakeCP(t, idA)
	rec, logs := panicRec(t, cp, newPanicEngine())
	calls := 0
	rec.OnUnknownNode = func(context.Context) { calls++; panic("register blew up") }
	cp.mu.Lock()
	cp.unknownNode = true
	cp.mu.Unlock()
	rec.tick(context.Background()) // a 404 on /work calls OnUnknownNode, which panics
	if calls != 1 || !strings.Contains(logs.String(), "reconciler poll panicked") {
		t.Fatalf("calls=%d log=%s", calls, logs.String())
	}
	cp.mu.Lock()
	cp.unknownNode = false
	cp.mu.Unlock()
	rec.lastUnknownNode = time.Time{}
	rec.tick(context.Background())
	if got, _ := cp.state(idA); got != "running" {
		t.Fatalf("the next poll did not run: state=%s", got)
	}
}

// nilMapWrite panics the way a real bug would: an assignment to a nil map.
func nilMapWrite(m map[string]int) { m["a nil map in a VMM call"] = 1 }
