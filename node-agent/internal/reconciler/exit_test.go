package reconciler

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/poddaemon"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

func statusReports(cp *fakeCP, id string) int {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.requests["POST /v1/sandboxes/"+id+"/status"]
}

// A VM whose process ends while the sandbox runs (killed, out of memory, the
// guest powering off) is reported stopped with the cause, its disk is kept, and
// everything it held is released: the control plane no longer believes in a
// sandbox that nothing backs.
func TestVMThatEndedByItselfIsReportedStopped(t *testing.T) {
	cp := newFakeCP(t, idA)
	fake := vmm.NewFakeVMM(nil)
	rec, disks := retainRec(t, cp, fake)
	rec.tick(context.Background())
	if got, _ := cp.state(idA); got != "running" {
		t.Fatalf("start: %s", got)
	}
	if err := os.WriteFile(diskOf(disks, idA), []byte("guest data"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, _ := rec.HandleOf(idA)

	if !fake.Crash(idA, errors.New("signal: killed"), 3*time.Minute+4*time.Second) {
		t.Fatal("the VM was not running")
	}
	rec.tick(context.Background())

	state, detail := cp.state(idA)
	if state != "stopped" || detail != "vmm_exited: signal: killed after 3m4s" {
		t.Fatalf("state=%s detail=%q", state, detail)
	}
	if _, ok := rec.HandleOf(idA); ok || len(fake.Running) != 0 {
		t.Fatalf("the dead VM is still held: handles=%v running=%v", rec.Handles(), fake.Running)
	}
	if b, err := os.ReadFile(diskOf(disks, idA)); err != nil || string(b) != "guest data" {
		t.Fatalf("the disk was not kept: %v %q", err, b)
	}
	if cid := rec.allocCID(); cid != h.CID {
		t.Fatalf("CID %d not released (next is %d)", h.CID, cid)
	}
	if _, still := rec.exitOf(idA); still {
		t.Fatal("the record outlived the report")
	}
	// It is reported once, not on every poll.
	before := statusReports(cp, idA)
	rec.tick(context.Background())
	if statusReports(cp, idA) != before {
		t.Fatal("a stopped sandbox was reported again")
	}
}

// A control plane that keeps no disks (older) means stopped is final: the disk is
// removed, as a stop does.
func TestVMThatEndedByItselfLosesItsDiskWithAnOlderControlPlane(t *testing.T) {
	cp := newFakeCP(t, idA)
	fake := vmm.NewFakeVMM(nil)
	rec, disks := retainRec(t, cp, fake)
	cp.retains = false
	rec.tick(context.Background())
	fake.Crash(idA, nil, 90*time.Second)
	rec.tick(context.Background())

	state, detail := cp.state(idA)
	if state != "stopped" || detail != "vmm_exited: the guest powered off after 1m30s" {
		t.Fatalf("state=%s detail=%q", state, detail)
	}
	if exists(diskOf(disks, idA)) {
		t.Fatal("an older control plane's stopped sandbox must not leave a disk")
	}
}

// The report comes before the release: while the control plane cannot be told,
// the VM stays accounted for and the next poll tries again.
func TestExitReportIsRetriedUntilTheControlPlaneHearsIt(t *testing.T) {
	cp := newFakeCP(t, idA)
	fake := vmm.NewFakeVMM(nil)
	rec, disks := retainRec(t, cp, fake)
	rec.tick(context.Background())

	fake.Crash(idA, errors.New("exit status 1"), time.Minute)
	cp.mu.Lock()
	cp.failStatus = 2
	cp.mu.Unlock()
	for i := 0; i < 2; i++ {
		rec.tick(context.Background())
		if got, _ := cp.state(idA); got != "running" {
			t.Fatalf("poll %d: state=%s, the report was refused", i, got)
		}
		if _, ok := rec.HandleOf(idA); !ok {
			t.Fatalf("poll %d: released before the control plane was told", i)
		}
	}
	rec.tick(context.Background())
	if state, detail := cp.state(idA); state != "stopped" || !strings.HasPrefix(detail, "vmm_exited: exit status 1") {
		t.Fatalf("state=%s detail=%q", state, detail)
	}
	if _, ok := rec.HandleOf(idA); ok || !exists(diskOf(disks, idA)) {
		t.Fatalf("handle=%v disk=%v", rec.Handles(), exists(diskOf(disks, idA)))
	}
}

// A sandbox the control plane is already stopping or deleting is torn down by
// that, and its VM having ended earlier is no crash to report.
func TestExitOfAStoppingSandboxIsNotACrash(t *testing.T) {
	cp := newFakeCP(t, idA, idB)
	fake := vmm.NewFakeVMM(nil)
	rec, _ := retainRec(t, cp, fake)
	rec.tick(context.Background())
	fake.Crash(idA, errors.New("signal: killed"), time.Minute)
	fake.Crash(idB, errors.New("signal: killed"), time.Minute)
	cp.setState(idA, "stopping")
	cp.setState(idB, "deleting")
	rec.tick(context.Background())

	if state, detail := cp.state(idA); state != "stopped" || strings.Contains(detail, "vmm_exited") {
		t.Fatalf("stop: state=%s detail=%q", state, detail)
	}
	if state, detail := cp.state(idB); state != "deleted" || strings.Contains(detail, "vmm_exited") {
		t.Fatalf("delete: state=%s detail=%q", state, detail)
	}
	for _, id := range []string{idA, idB} {
		if _, still := rec.exitOf(id); still {
			t.Fatalf("%s: the record outlived the teardown", id)
		}
	}
}

// A sandbox that is resumed after its VM ended starts clean: the old exit must
// not fail the new start.
func TestResumeAfterAnExitStartsClean(t *testing.T) {
	cp := newFakeCP(t, idA)
	fake := vmm.NewFakeVMM(nil)
	rec, _ := retainRec(t, cp, fake)
	rec.tick(context.Background())
	fake.Crash(idA, errors.New("signal: killed"), time.Minute)
	rec.tick(context.Background())
	if got, _ := cp.state(idA); got != "stopped" {
		t.Fatalf("exit: %s", got)
	}
	cp.mu.Lock()
	cp.boxes[idA].State, cp.boxes[idA].BootCount = "requested", 2
	cp.mu.Unlock()
	rec.tick(context.Background())
	if got, _ := cp.state(idA); got != "running" {
		t.Fatalf("resume: %s", got)
	}
	if _, ok := rec.HandleOf(idA); !ok {
		t.Fatal("the resumed VM has no handle")
	}
}

// A VM that ends while its start waits for the guest ends the wait: the start
// fails at once with the cause, not after the guest timeout with "guest_not_ready".
func TestVMThatEndsDuringTheStartFailsItAtOnce(t *testing.T) {
	cp, srv := newRunningCP(t, "guest-gone-01")
	eng := &bootingEngine{FakeVMM: vmm.NewFakeVMM(nil), t: t, refuse: 1 << 30,
		console: "[    0.5] Out of memory: Killed process 1 (init)\r\n"}
	rec := newGuestReadyReconciler(t, srv, eng, 30*time.Second)
	go func() {
		for i := 0; i < 400; i++ {
			if _, ok := eng.FakeVMM.RunningConfig("guest-gone-01"); ok {
				time.Sleep(150 * time.Millisecond)
				eng.FakeVMM.Crash("guest-gone-01", errors.New("signal: killed"), 200*time.Millisecond)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	start := time.Now()
	rec.tick(context.Background())

	state, _ := cp.running()
	cp.mu.Lock()
	detail := cp.detail
	cp.mu.Unlock()
	if state != "failed" || !strings.HasPrefix(detail, "vmm_exited: signal: killed after 200ms") || strings.Contains(detail, "guest_not_ready") ||
		!strings.Contains(detail, "console: [    0.5] Out of memory: Killed process 1 (init))") {
		t.Fatalf("state=%s detail=%q", state, detail)
	}
	if waited := time.Since(start); waited > 10*time.Second {
		t.Fatalf("the start waited %v for a VM that was gone", waited)
	}
	if len(rec.Handles()) != 0 {
		t.Fatalf("handles=%v", rec.Handles())
	}
}

// A resume whose VM ends during the start goes back to stopped with its disk.
func TestResumedVMThatEndsDuringTheStartKeepsItsDisk(t *testing.T) {
	cp := newFakeCP(t, idA)
	eng := &bootingEngine{FakeVMM: vmm.NewFakeVMM(nil), t: t}
	rec, disks := retainRec(t, cp, eng)
	rec.Registry = poddaemon.NewRegistry(nil)
	rec.GuestReadyTimeout = 5 * time.Second
	rec.tick(context.Background())
	cp.setState(idA, "stopping")
	rec.tick(context.Background())
	if got, _ := cp.state(idA); got != "stopped" {
		t.Fatalf("stop: %s", got)
	}

	eng.refuse = 1 << 30
	cp.mu.Lock()
	cp.boxes[idA].State, cp.boxes[idA].BootCount = "requested", 2
	cp.mu.Unlock()
	go func() {
		for i := 0; i < 400; i++ {
			if _, ok := eng.FakeVMM.RunningConfig(idA); ok {
				time.Sleep(100 * time.Millisecond)
				eng.FakeVMM.Crash(idA, errors.New("exit status 1"), 100*time.Millisecond)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	rec.tick(context.Background())

	state, detail := cp.state(idA)
	if state != "stopped" || !strings.HasPrefix(detail, "resume failed: vmm_exited: exit status 1") {
		t.Fatalf("state=%s detail=%q", state, detail)
	}
	if !exists(diskOf(disks, idA)) {
		t.Fatal("a resume that failed lost its disk")
	}
}

// A VM the control plane stopped assigning here (failed over) is fenced as ever;
// its exit is not reported over the control plane's decision.
func TestExitOfAnUnassignedVMIsLeftToTheFence(t *testing.T) {
	cp := newFakeCP(t, idA)
	fake := vmm.NewFakeVMM(nil)
	rec, _ := retainRec(t, cp, fake)
	rec.tick(context.Background())
	fake.Crash(idA, errors.New("signal: killed"), time.Minute)
	cp.setState(idA, "failed")
	before := statusReports(cp, idA)
	rec.tick(context.Background())

	if got, detail := cp.state(idA); got != "failed" || strings.Contains(detail, "vmm_exited") {
		t.Fatalf("state=%s detail=%q", got, detail)
	}
	if statusReports(cp, idA) != before {
		t.Fatal("reported a sandbox the control plane no longer assigns here")
	}
	if _, ok := rec.HandleOf(idA); ok {
		t.Fatal("the fence did not release it")
	}
	if _, still := rec.exitOf(idA); still {
		t.Fatal("the record outlived the fence")
	}
}

func TestExitDetail(t *testing.T) {
	for _, c := range []struct {
		info vmm.ExitInfo
		want string
	}{
		{vmm.ExitInfo{Lived: 5*time.Minute + 3*time.Second}, "vmm_exited: the guest powered off after 5m3s"},
		{vmm.ExitInfo{Err: errors.New("exit status 1: Finished with result: oom-kill; Main processes terminated with: code=killed/status=KILL"), Lived: 2*time.Hour + 30*time.Second},
			"vmm_exited: exit status 1: Finished with result: oom-kill; Main processes terminated with: code=killed/status=KILL after 2h0m30s"},
		{vmm.ExitInfo{Err: errors.New("signal: killed"), Lived: 1530 * time.Millisecond}, "vmm_exited: signal: killed after 1.5s"},
		{vmm.ExitInfo{Err: errors.New("exit status 2\n  with\ta\r\nbreak"), Lived: time.Minute}, "vmm_exited: exit status 2 with a break after 1m0s"},
	} {
		if got := exitDetail(c.info); got != c.want {
			t.Errorf("exitDetail(%+v)\n got %q\nwant %q", c.info, got, c.want)
		}
	}
	long := exitDetail(vmm.ExitInfo{Err: errors.New(strings.Repeat("x", 5000)), Lived: time.Minute})
	if len(long) > exitDetailMax+60 || !strings.Contains(long, "...") {
		t.Errorf("a long cause is not cut: %d bytes", len(long))
	}
}

// The guest powering itself off is how a graceful stop works: the VM's process ends
// during the stop's own wait, and that must not be reported afterwards as an exit
// that fails the next start. (Found on a real host: a resume right after a stop
// failed with "vmm_exited: the guest powered off".)
func TestExitDuringAGracefulStopDoesNotFailTheNextResume(t *testing.T) {
	cp := newFakeCP(t, idA)
	eng := &powerEngine{FakeVMM: vmm.NewFakeVMM(nil), t: t, down: true, exitDuringWait: true}
	rec, disks := retainRec(t, cp, eng)
	rec.Registry = poddaemon.NewRegistry(nil)
	rec.GuestReadyTimeout = 5 * time.Second
	rec.StopGrace = 5 * time.Second
	rec.tick(context.Background())
	if got, _ := cp.state(idA); got != "running" {
		t.Fatalf("start: %s", got)
	}

	cp.setState(idA, "stopping")
	rec.tick(context.Background())
	if got, detail := cp.state(idA); got != "stopped" || strings.Contains(detail, "vmm_exited") {
		t.Fatalf("stop: state=%s detail=%q", got, detail)
	}
	if _, stale := rec.exitOf(idA); stale {
		t.Fatal("the exit of a VM that was being stopped outlived the stop")
	}

	cp.mu.Lock()
	cp.boxes[idA].State, cp.boxes[idA].BootCount = "requested", 2
	cp.mu.Unlock()
	rec.tick(context.Background())
	if got, detail := cp.state(idA); got != "running" {
		t.Fatalf("resume: state=%s detail=%q", got, detail)
	}
	if !exists(diskOf(disks, idA)) {
		t.Fatal("the disk is gone")
	}
}
