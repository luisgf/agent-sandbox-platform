package reconciler

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

// slowEngine is a FakeVMM whose Start takes a while and counts starts per id.
type slowEngine struct {
	*vmm.FakeVMM
	delay  time.Duration
	mu     sync.Mutex
	starts map[string]int
	max    int // most starts running at once
	active int
}

func newSlowEngine(delay time.Duration) *slowEngine {
	return &slowEngine{FakeVMM: vmm.NewFakeVMM(nil), delay: delay, starts: map[string]int{}}
}

func (e *slowEngine) Start(ctx context.Context, cfg vmm.MicroVMConfig) error {
	e.mu.Lock()
	e.starts[cfg.ID]++
	e.active++
	if e.active > e.max {
		e.max = e.active
	}
	e.mu.Unlock()
	time.Sleep(e.delay)
	e.mu.Lock()
	e.active--
	e.mu.Unlock()
	return e.FakeVMM.Start(ctx, cfg)
}

func (e *slowEngine) startsOf(id string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.starts[id]
}

// Five slow boots run four at a time instead of one after another.
func TestWorkersStartSandboxesConcurrently(t *testing.T) {
	ids := make([]string, 5)
	for i := range ids {
		ids[i] = fmt.Sprintf("s%d", i)
	}
	cp := newFakeCP(t, ids...)
	eng := newSlowEngine(500 * time.Millisecond)
	rec := New(cp.client(), "n1", eng, nil, time.Hour)
	rec.VsockDir = t.TempDir()
	rec.Workers = 4

	began := time.Now()
	rec.tick(context.Background())
	took := time.Since(began)
	for _, id := range ids {
		if st, _ := cp.state(id); st != "running" {
			t.Fatalf("%s: %s", id, st)
		}
	}
	if took > 1800*time.Millisecond {
		t.Fatalf("five 500 ms boots with 4 workers took %s; sequential is 2.5 s", took)
	}
	if eng.max != 4 {
		t.Fatalf("at most %d boots ran at once, want 4", eng.max)
	}
}

// Polls while boots are in flight never start an id twice.
func TestPollsNeverDoubleStartAnID(t *testing.T) {
	cp := newFakeCP(t, "s1", "s2")
	eng := newSlowEngine(300 * time.Millisecond)
	rec := New(cp.client(), "n1", eng, nil, time.Hour)
	rec.VsockDir = t.TempDir()
	for i := 0; i < 5; i++ {
		rec.poll(context.Background())
		time.Sleep(20 * time.Millisecond)
	}
	rec.work.Wait()
	rec.tick(context.Background())
	for _, id := range []string{"s1", "s2"} {
		if n := eng.startsOf(id); n != 1 {
			t.Fatalf("%s started %d times", id, n)
		}
	}
}

// A stopping that arrives while the start is in flight waits for it, then
// tears the sandbox down: no leaked handle, the control plane sees stopped.
func TestStoppingDuringAStartTearsDownAfterIt(t *testing.T) {
	cp := newFakeCP(t, "s1")
	eng := newSlowEngine(300 * time.Millisecond)
	rec := New(cp.client(), "n1", eng, nil, time.Hour)
	rec.VsockDir = t.TempDir()

	rec.poll(context.Background()) // start in flight
	time.Sleep(50 * time.Millisecond)
	cp.setState("s1", "stopping")  // the user destroyed it meanwhile
	rec.poll(context.Background()) // s1 is in flight: skipped
	rec.work.Wait()
	rec.tick(context.Background()) // now the stop
	if _, ok := rec.HandleOf("s1"); ok {
		t.Fatal("handle leaked after stopping")
	}
	if st, _ := cp.state("s1"); st != "stopped" {
		t.Fatalf("control plane state %s, want stopped", st)
	}
	if n := eng.startsOf("s1"); n != 1 {
		t.Fatalf("started %d times", n)
	}
}
