package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Stop, resume and delete (ADR-0012), in either store.

func lifecycleStore(t *testing.T, s Store, maxSandboxes int, nodes ...string) {
	t.Helper()
	t.Setenv("ASP_AUTO_PROVISION", "0")
	registerPlacementNodes(t, s, maxSandboxes, nodes...)
}

// runningOn creates a sandbox on node, claims it and reports it running.
func runningOn(t *testing.T, s Store, node string) Sandbox {
	t.Helper()
	sb, err := s.CreateSandbox(context.Background(), CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 512,
		OwnerSub: "user:a", NodeID: node})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimSandbox(context.Background(), sb.ID, node); err != nil {
		t.Fatal(err)
	}
	out, err := s.UpdateSandboxStatus(context.Background(), sb.ID, SandboxRunning, "up")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func report(t *testing.T, s Store, id string, state SandboxState, detail string) Sandbox {
	t.Helper()
	out, err := s.UpdateSandboxStatus(context.Background(), id, state, detail)
	if err != nil {
		t.Fatalf("report %s: %v", state, err)
	}
	return out
}

func sandboxOf(t *testing.T, s Store, id string) Sandbox {
	t.Helper()
	sb, err := s.GetSandbox(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func inWork(w NodeWork, id string) bool {
	for _, sb := range w.Sandboxes {
		if sb.ID == id {
			return true
		}
	}
	return false
}

func workOf(t *testing.T, s Store, node string) NodeWork {
	t.Helper()
	w, err := s.ListNodeWork(context.Background(), node)
	if err != nil {
		t.Fatal(err)
	}
	if w.Retained == nil {
		t.Fatal("Retained must never be nil: the node reads that as an older control plane")
	}
	return w
}

// A stop keeps the sandbox: its node powers it off, then it is retained, not
// assigned; a resume brings it back as requested on the same node with the same
// owner, one more boot, and the node is asked to start it.
func testStopThenResume(t *testing.T, s Store) {
	lifecycleStore(t, s, 0, "node-a")
	sb := runningOn(t, s, "node-a")
	if sb.BootCount != 1 {
		t.Fatalf("first boot_count=%d", sb.BootCount)
	}

	stopping, err := s.StopSandbox(context.Background(), sb.ID, "user:a")
	if err != nil || stopping.State != SandboxStopping || stopping.StoppedAt != nil {
		t.Fatalf("stop: %+v %v", stopping, err)
	}
	w := workOf(t, s, "node-a")
	if !contains(w.Assigned, sb.ID) || !inWork(w, sb.ID) || contains(w.Retained, sb.ID) {
		t.Fatalf("a stopping sandbox is still the node's to stop: %+v", w)
	}
	if again, err := s.StopSandbox(context.Background(), sb.ID, "user:a"); err != nil || again.State != SandboxStopping {
		t.Fatalf("stopping twice: %+v %v", again, err)
	}
	if _, err := s.ResumeSandbox(context.Background(), sb.ID, "user:a"); !errors.Is(err, ErrConflict) {
		t.Fatalf("resume while stopping: %v", err)
	}

	stopped := report(t, s, sb.ID, SandboxStopped, "vmm stopped, disk kept")
	if stopped.StoppedAt == nil || stopped.StatusDetail != "" {
		t.Fatalf("stopped: stopped_at=%v status_detail=%q", stopped.StoppedAt, stopped.StatusDetail)
	}
	w = workOf(t, s, "node-a")
	if contains(w.Assigned, sb.ID) || inWork(w, sb.ID) || !contains(w.Retained, sb.ID) {
		t.Fatalf("a stopped sandbox is retained and holds nothing: %+v", w)
	}
	if again, err := s.StopSandbox(context.Background(), sb.ID, "user:a"); err != nil || again.State != SandboxStopped {
		t.Fatalf("stopping a stopped sandbox: %+v %v", again, err)
	}

	resumed, err := s.ResumeSandbox(context.Background(), sb.ID, "user:a")
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State != SandboxRequested || resumed.BootCount != 2 || resumed.StoppedAt != nil ||
		resumed.NodeID == nil || *resumed.NodeID != "node-a" || resumed.OwnerSub != "user:a" || resumed.ID != sb.ID {
		t.Fatalf("resume: %+v", resumed)
	}
	w = workOf(t, s, "node-a")
	if !contains(w.Assigned, sb.ID) || !inWork(w, sb.ID) || contains(w.Retained, sb.ID) {
		t.Fatalf("a resumed sandbox is the node's to start: %+v", w)
	}
	var found bool
	events, _ := s.ListEvents(context.Background(), sb.ID)
	for _, e := range events {
		found = found || e.EventType == "sandbox.resumed"
	}
	if !found {
		t.Fatalf("no sandbox.resumed event: %+v", events)
	}

	if _, err := s.ClaimSandbox(context.Background(), sb.ID, "node-a"); err != nil {
		t.Fatal(err)
	}
	up := report(t, s, sb.ID, SandboxRunning, "vmm started")
	if up.BootCount != 2 {
		t.Fatalf("boot_count=%d after running", up.BootCount)
	}
	if again, err := s.ResumeSandbox(context.Background(), sb.ID, "user:a"); err != nil || again.State != SandboxRunning || again.BootCount != 2 {
		t.Fatalf("resuming a running sandbox: %+v %v", again, err)
	}
}

func TestMemoryStopThenResume(t *testing.T)   { testStopThenResume(t, NewMemoryStore()) }
func TestPostgresStopThenResume(t *testing.T) { testStopThenResume(t, newPostgresTestStore(t)) }

// A sandbox never claimed has no VM: stop is immediate. Only a sandbox with
// something to stop can be stopped.
func testStopPlans(t *testing.T, s Store) {
	lifecycleStore(t, s, 0, "node-a")
	unclaimed, err := s.CreateSandbox(context.Background(), CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64, NodeID: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.StopSandbox(context.Background(), unclaimed.ID, "")
	if err != nil || out.State != SandboxStopped || out.StoppedAt == nil {
		t.Fatalf("stop before the claim: %+v %v", out, err)
	}
	// It has a node and never had a VM: resumable, and a resume is a first boot's work.
	if r, err := s.ResumeSandbox(context.Background(), unclaimed.ID, ""); err != nil || r.State != SandboxRequested {
		t.Fatalf("resume of a never-started sandbox: %+v %v", r, err)
	}

	failed := runningOn(t, s, "node-a")
	report(t, s, failed.ID, SandboxFailed, "boom")
	if _, err := s.StopSandbox(context.Background(), failed.ID, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("stopping a failed sandbox: %v", err)
	}
	if _, err := s.ResumeSandbox(context.Background(), failed.ID, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("resuming a failed sandbox: %v", err)
	}
	if _, err := s.StopSandbox(context.Background(), "ghost", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown sandbox: %v", err)
	}
	if _, err := s.ResumeSandbox(context.Background(), "ghost", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown sandbox: %v", err)
	}
	deleted, err := s.DeleteSandbox(context.Background(), failed.ID, "")
	if err != nil || deleted.State != SandboxDeleted {
		t.Fatalf("delete of a failed sandbox: %+v %v", deleted, err)
	}
	for name, fn := range map[string]func() error{
		"stop":   func() error { _, err := s.StopSandbox(context.Background(), deleted.ID, ""); return err },
		"resume": func() error { _, err := s.ResumeSandbox(context.Background(), deleted.ID, ""); return err },
	} {
		if err := fn(); !errors.Is(err, ErrConflict) {
			t.Fatalf("%s of a deleted sandbox: %v", name, err)
		}
	}
}

func TestMemoryStopPlans(t *testing.T)   { testStopPlans(t, NewMemoryStore()) }
func TestPostgresStopPlans(t *testing.T) { testStopPlans(t, newPostgresTestStore(t)) }

// A resume is placed like a pinned create: on its node or nowhere. A node that
// cannot take it refuses, the sandbox stays stopped, and room is never exceeded.
func testResumeNeedsRoomOnItsNode(t *testing.T, s Store) {
	lifecycleStore(t, s, 1, "node-a", "node-b")
	first := runningOn(t, s, "node-a")
	if _, err := s.StopSandbox(context.Background(), first.ID, ""); err != nil {
		t.Fatal(err)
	}
	report(t, s, first.ID, SandboxStopped, "")

	// node-a has one slot, and a new sandbox takes it while the first is stopped.
	second := runningOn(t, s, "node-a")
	if _, err := s.ResumeSandbox(context.Background(), first.ID, ""); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("resume on a full node: %v, want no capacity", err)
	}
	if got := sandboxOf(t, s, first.ID); got.State != SandboxStopped || got.BootCount != 1 {
		t.Fatalf("a refused resume changed the sandbox: %+v", got)
	}
	// node-b has room, but the disk is on node-a: no moving it.
	if got := sandboxOf(t, s, first.ID); got.NodeID == nil || *got.NodeID != "node-a" {
		t.Fatalf("node=%v", got.NodeID)
	}

	if _, err := s.StopSandbox(context.Background(), second.ID, ""); err != nil {
		t.Fatal(err)
	}
	report(t, s, second.ID, SandboxStopped, "")
	if _, err := s.SetNodeCordoned(context.Background(), "node-a", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResumeSandbox(context.Background(), first.ID, ""); !errors.Is(err, ErrNodeUnavailable) {
		t.Fatalf("resume on a cordoned node: %v, want node unavailable", err)
	}
	if _, err := s.SetNodeCordoned(context.Background(), "node-a", false); err != nil {
		t.Fatal(err)
	}
	if r, err := s.ResumeSandbox(context.Background(), first.ID, ""); err != nil || r.State != SandboxRequested {
		t.Fatalf("resume once there is room: %+v %v", r, err)
	}
	if _, err := s.ResumeSandbox(context.Background(), second.ID, ""); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("the second resume must not overfill the node: %v", err)
	}
}

func TestMemoryResumeNeedsRoomOnItsNode(t *testing.T) {
	testResumeNeedsRoomOnItsNode(t, NewMemoryStore())
}
func TestPostgresResumeNeedsRoomOnItsNode(t *testing.T) {
	testResumeNeedsRoomOnItsNode(t, newPostgresTestStore(t))
}

// Concurrent resumes cannot overfill a node either.
func testConcurrentResumesRespectCapacity(t *testing.T, s Store) {
	lifecycleStore(t, s, 2, "node-a")
	var ids []string
	for i := 0; i < 2; i++ {
		sb := runningOn(t, s, "node-a")
		if _, err := s.StopSandbox(context.Background(), sb.ID, ""); err != nil {
			t.Fatal(err)
		}
		report(t, s, sb.ID, SandboxStopped, "")
		ids = append(ids, sb.ID)
	}
	// Two stopped; fill the node with two others, then free one slot: one resume wins.
	a, b := runningOn(t, s, "node-a"), runningOn(t, s, "node-a")
	if _, err := s.StopSandbox(context.Background(), a.ID, ""); err != nil {
		t.Fatal(err)
	}
	report(t, s, a.ID, SandboxStopped, "")
	_ = b
	results := make(chan error, len(ids))
	for _, id := range ids {
		go func(id string) { _, err := s.ResumeSandbox(context.Background(), id, ""); results <- err }(id)
	}
	ok, refused := 0, 0
	for range ids {
		switch err := <-results; {
		case err == nil:
			ok++
		case errors.Is(err, ErrNoCapacity):
			refused++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("resumed %d, refused %d; want 1 and 1 for one free slot", ok, refused)
	}
}

func TestMemoryConcurrentResumesRespectCapacity(t *testing.T) {
	testConcurrentResumesRespectCapacity(t, NewMemoryStore())
}
func TestPostgresConcurrentResumesRespectCapacity(t *testing.T) {
	testConcurrentResumesRespectCapacity(t, newPostgresTestStore(t))
}

// Delete: the node removes the VM and the disk of anything that has them; the
// row stays as deleted.
func testDeleteEndsAtDeleted(t *testing.T, s Store) {
	lifecycleStore(t, s, 0, "node-a")
	running := runningOn(t, s, "node-a")
	out, err := s.DeleteSandbox(context.Background(), running.ID, "user:a")
	if err != nil || out.State != SandboxDeleting {
		t.Fatalf("delete of a running sandbox: %+v %v", out, err)
	}
	// The node still has a VM to remove: the work is its, the sandbox is still
	// assigned to it, and it holds its capacity until the node reports deleted.
	w := workOf(t, s, "node-a")
	if !inWork(w, running.ID) || !contains(w.Assigned, running.ID) || contains(w.Retained, running.ID) {
		t.Fatalf("deleting is the node's to do and still holds its capacity: %+v", w)
	}
	if usage, _ := s.ListNodeUsage(context.Background()); usage["node-a"].Sandboxes != 1 {
		t.Fatalf("a deleting sandbox does not hold node capacity: %+v", usage)
	}
	if again, err := s.DeleteSandbox(context.Background(), running.ID, ""); err != nil || again.State != SandboxDeleting {
		t.Fatalf("deleting twice: %+v %v", again, err)
	}
	// A late report from a start or a stop must not bring it back.
	for _, st := range []SandboxState{SandboxRunning, SandboxStopped, SandboxFailed, SandboxStarting} {
		if _, err := s.UpdateSandboxStatus(context.Background(), running.ID, st, ""); !errors.Is(err, ErrConflict) {
			t.Fatalf("report %s on a deleting sandbox: %v", st, err)
		}
	}
	done := report(t, s, running.ID, SandboxDeleted, "vmm and disk removed")
	if done.State != SandboxDeleted {
		t.Fatalf("deleted: %+v", done)
	}
	if usage, _ := s.ListNodeUsage(context.Background()); usage["node-a"].Sandboxes != 0 {
		t.Fatalf("the capacity of a deleted sandbox is not released: %+v", usage)
	}
	w = workOf(t, s, "node-a")
	if inWork(w, running.ID) || contains(w.Assigned, running.ID) || contains(w.Retained, running.ID) {
		t.Fatalf("a deleted sandbox is nobody's work: %+v", w)
	}
	if again, err := s.UpdateSandboxStatus(context.Background(), running.ID, SandboxDeleted, ""); err != nil || again.State != SandboxDeleted {
		t.Fatalf("deleted twice: %+v %v", again, err)
	}
	if _, err := s.UpdateSandboxStatus(context.Background(), running.ID, SandboxRunning, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("a deleted sandbox must stay deleted: %v", err)
	}
	if got, err := s.GetSandbox(context.Background(), running.ID); err != nil || got.State != SandboxDeleted {
		t.Fatalf("the row is kept: %+v %v", got, err)
	}
	if events, _ := s.ListEvents(context.Background(), running.ID); len(events) < 4 {
		t.Fatalf("the audit trail survives a delete: %d events", len(events))
	}

	// stopped → deleting (the node holds the disk), and the retention list drops it.
	other := runningOn(t, s, "node-a")
	if _, err := s.StopSandbox(context.Background(), other.ID, ""); err != nil {
		t.Fatal(err)
	}
	report(t, s, other.ID, SandboxStopped, "")
	if out, err := s.DeleteSandbox(context.Background(), other.ID, ""); err != nil || out.State != SandboxDeleting {
		t.Fatalf("delete of a stopped sandbox: %+v %v", out, err)
	}
	if w := workOf(t, s, "node-a"); contains(w.Retained, other.ID) || !inWork(w, other.ID) {
		t.Fatalf("a stopped sandbox being deleted is no longer retained: %+v", w)
	}

	// Never claimed: nothing on any node, so deleted at once.
	unclaimed, err := s.CreateSandbox(context.Background(), CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64, NodeID: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := s.DeleteSandbox(context.Background(), unclaimed.ID, ""); err != nil || out.State != SandboxDeleted {
		t.Fatalf("delete before the claim: %+v %v", out, err)
	}
	if _, err := s.DeleteSandbox(context.Background(), "ghost", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown sandbox: %v", err)
	}
}

func TestMemoryDeleteEndsAtDeleted(t *testing.T) { testDeleteEndsAtDeleted(t, NewMemoryStore()) }
func TestPostgresDeleteEndsAtDeleted(t *testing.T) {
	testDeleteEndsAtDeleted(t, newPostgresTestStore(t))
}

// What a node may report from each state, in the table form of the ADR.
func TestValidAgentTransitionWithRetention(t *testing.T) {
	all := []SandboxState{SandboxRequested, SandboxScheduled, SandboxStarting, SandboxRunning, SandboxPaused,
		SandboxStopping, SandboxStopped, SandboxFailed, SandboxDeleting, SandboxDeleted}
	reportable := []SandboxState{SandboxStarting, SandboxRunning, SandboxFailed, SandboxStopped, SandboxDeleted}
	for _, st := range all {
		want := false
		for _, r := range reportable {
			if r == st {
				want = true
			}
		}
		if ValidAgentStatus(st) != want {
			t.Errorf("ValidAgentStatus(%s)=%v", st, !want)
		}
	}
	for _, c := range []struct {
		from, to SandboxState
		ok       bool
	}{
		{SandboxStopping, SandboxStopped, true},
		{SandboxStarting, SandboxStopped, true}, // a resume that could not start
		{SandboxStopped, SandboxStopped, true},
		{SandboxStopped, SandboxRunning, false}, // a late report cannot revive it
		{SandboxStopped, SandboxStarting, false},
		{SandboxStopped, SandboxDeleted, false}, // deleting first
		{SandboxRunning, SandboxDeleted, false},
		{SandboxDeleting, SandboxDeleted, true},
		{SandboxDeleting, SandboxRunning, false},
		{SandboxDeleting, SandboxStopped, false},
		{SandboxDeleting, SandboxFailed, false},
		{SandboxDeleted, SandboxDeleted, true},
		{SandboxDeleted, SandboxStopped, false},
		{SandboxDeleted, SandboxRunning, false},
		{SandboxFailed, SandboxStopped, true},
		{SandboxFailed, SandboxRunning, false},
	} {
		if got := ValidAgentTransition(c.from, c.to); got != c.ok {
			t.Errorf("ValidAgentTransition(%s, %s)=%v, want %v", c.from, c.to, got, c.ok)
		}
	}
}

// A node's detail is kept for a start that could not finish, not for a plain
// stop, and a sandbox that runs or is resumed starts clean.
func testStatusDetail(t *testing.T, s Store) {
	lifecycleStore(t, s, 0, "node-a")
	sb := runningOn(t, s, "node-a")
	if _, err := s.StopSandbox(context.Background(), sb.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got := report(t, s, sb.ID, SandboxStopped, "vmm stopped, disk kept"); got.StatusDetail != "" {
		t.Fatalf("a plain stop recorded %q", got.StatusDetail)
	}
	if _, err := s.ResumeSandbox(context.Background(), sb.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimSandbox(context.Background(), sb.ID, "node-a"); err != nil {
		t.Fatal(err)
	}
	// The resume could not start: back to stopped, with the reason.
	back := report(t, s, sb.ID, SandboxStopped, "resume failed: vm.boot failed")
	if back.State != SandboxStopped || back.StatusDetail != "resume failed: vm.boot failed" || back.StoppedAt == nil {
		t.Fatalf("failed resume: %+v", back)
	}
	if again, err := s.ResumeSandbox(context.Background(), sb.ID, ""); err != nil || again.StatusDetail != "" || again.BootCount != 3 {
		t.Fatalf("a new resume starts clean: %+v %v", again, err)
	}
	if _, err := s.ClaimSandbox(context.Background(), sb.ID, "node-a"); err != nil {
		t.Fatal(err)
	}
	if got := report(t, s, sb.ID, SandboxFailed, "disk_lost: the retained disk is missing"); got.StatusDetail != "disk_lost: the retained disk is missing" {
		t.Fatalf("failed detail: %+v", got)
	}
}

func TestMemoryStatusDetail(t *testing.T)   { testStatusDetail(t, NewMemoryStore()) }
func TestPostgresStatusDetail(t *testing.T) { testStatusDetail(t, newPostgresTestStore(t)) }

// A node that finds the VM's process gone moves a running sandbox to stopped and
// says why: the detail is kept with a reason clients can switch on, a resume
// starts clean, and only that report is kept (a plain stop's detail still is not).
func testVMMExitedReport(t *testing.T, s Store) {
	lifecycleStore(t, s, 0, "node-a")
	sb := runningOn(t, s, "node-a")
	const detail = "vmm_exited: signal: killed after 3m12s"
	got := report(t, s, sb.ID, SandboxStopped, detail)
	if got.State != SandboxStopped || got.StatusDetail != detail || got.StopReason != StopReasonVMMExited || got.StoppedAt == nil {
		t.Fatalf("exit report: %+v", got)
	}
	if again := report(t, s, sb.ID, SandboxStopped, "vmm stopped, disk kept"); again.StatusDetail != detail || again.StopReason != StopReasonVMMExited {
		t.Fatalf("a later plain report changed it: %+v", again)
	}
	resumed, err := s.ResumeSandbox(context.Background(), sb.ID, "")
	if err != nil || resumed.StatusDetail != "" || resumed.StopReason != "" {
		t.Fatalf("a resume starts clean: %+v %v", resumed, err)
	}
	if _, err := s.ClaimSandbox(context.Background(), sb.ID, "node-a"); err != nil {
		t.Fatal(err)
	}
	if run := report(t, s, sb.ID, SandboxRunning, "vmm started"); run.StatusDetail != "" || run.StopReason != "" {
		t.Fatalf("running after the resume: %+v", run)
	}

	// Anything else that says it is not an exit: a stopped report with that text
	// from a sandbox that was not running, or a different text.
	other := runningOn(t, s, "node-a")
	if got := report(t, s, other.ID, SandboxStopped, "vmm stopped, disk kept"); got.StopReason != "" || got.StatusDetail != "" {
		t.Fatalf("plain stop: %+v", got)
	}
}

func TestMemoryVMMExitedReport(t *testing.T)   { testVMMExitedReport(t, NewMemoryStore()) }
func TestPostgresVMMExitedReport(t *testing.T) { testVMMExitedReport(t, newPostgresTestStore(t)) }

// The idle reaper stops; the stopped sandbox is resumable.
func testIdleReapedSandboxResumes(t *testing.T, s Store, touch func(id string, at time.Time)) {
	lifecycleStore(t, s, 0, "node-a")
	sb := runningOn(t, s, "node-a")
	touch(sb.ID, time.Now().Add(-3*time.Hour))
	reaped, err := s.StopIdleSandboxes(context.Background(), time.Now().UTC(), 2*time.Hour)
	if err != nil || len(reaped) != 1 || reaped[0].State != SandboxStopping || reaped[0].StopReason != StopReasonIdle {
		t.Fatalf("reaper: %+v %v", reaped, err)
	}
	stopped := report(t, s, sb.ID, SandboxStopped, "vmm stopped, disk kept")
	if stopped.StopReason != StopReasonIdle || stopped.StoppedAt == nil {
		t.Fatalf("stopped: %+v", stopped)
	}
	resumed, err := s.ResumeSandbox(context.Background(), sb.ID, "")
	if err != nil || resumed.StopReason != "" || resumed.State != SandboxRequested {
		t.Fatalf("resume after the reaper: %+v %v", resumed, err)
	}
}

func TestMemoryIdleReapedSandboxResumes(t *testing.T) {
	s := NewMemoryStore()
	testIdleReapedSandboxResumes(t, s, s.SetLastActivityForTest)
}

func TestPostgresIdleReapedSandboxResumes(t *testing.T) {
	s := newPostgresTestStore(t)
	testIdleReapedSandboxResumes(t, s, func(id string, at time.Time) {
		if _, err := s.pool.Exec(context.Background(), `UPDATE sandboxes SET last_activity_at=$2 WHERE id=$1`, id, at); err != nil {
			t.Fatal(err)
		}
	})
}

// A lost node takes the VMs and disks it held: what was being deleted is
// deleted, what a stop kept stays stopped (the disk is back if the node is).
func testLostNodeEndsDeletes(t *testing.T, s Store, setLastSeen func(string, time.Time)) {
	lifecycleStore(t, s, 0, "node-a")
	deleting := runningOn(t, s, "node-a")
	if _, err := s.DeleteSandbox(context.Background(), deleting.ID, ""); err != nil {
		t.Fatal(err)
	}
	kept := runningOn(t, s, "node-a")
	if _, err := s.StopSandbox(context.Background(), kept.ID, ""); err != nil {
		t.Fatal(err)
	}
	report(t, s, kept.ID, SandboxStopped, "")
	running := runningOn(t, s, "node-a")

	now := time.Now().UTC()
	setLastSeen("node-a", now.Add(-10*time.Minute))
	if _, err := s.FailNodeSandboxes(context.Background(), "node-a", StopReasonNodeLost, now.Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := sandboxOf(t, s, deleting.ID); got.State != SandboxDeleted {
		t.Fatalf("a delete on a lost node: %s", got.State)
	}
	if got := sandboxOf(t, s, kept.ID); got.State != SandboxStopped {
		t.Fatalf("a stopped sandbox on a lost node: %s", got.State)
	}
	if got := sandboxOf(t, s, running.ID); got.State != SandboxFailed {
		t.Fatalf("a running sandbox on a lost node: %s", got.State)
	}
}

func TestMemoryLostNodeEndsDeletes(t *testing.T) {
	s := NewMemoryStore()
	testLostNodeEndsDeletes(t, s, s.SetNodeLastSeenForTest)
}

func TestPostgresLostNodeEndsDeletes(t *testing.T) {
	s := newPostgresTestStore(t)
	testLostNodeEndsDeletes(t, s, func(id string, ts time.Time) {
		if _, err := s.pool.Exec(context.Background(), `UPDATE nodes SET last_seen_at=$2 WHERE id=$1`, id, ts); err != nil {
			t.Fatal(err)
		}
	})
}

// The memory store lists newest first, like Postgres.
func TestMemoryListSandboxesNewestFirst(t *testing.T) {
	s := NewMemoryStore()
	lifecycleStore(t, s, 0, "node-a")
	var ids []string
	for i := 0; i < 4; i++ {
		sb, err := s.CreateSandbox(context.Background(), CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64, NodeID: "node-a"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append([]string{sb.ID}, ids...)
		time.Sleep(2 * time.Millisecond)
	}
	list, _ := s.ListSandboxes(context.Background(), "")
	for i, sb := range list {
		if sb.ID != ids[i] {
			t.Fatalf("position %d: %s, want %s", i, sb.ID, ids[i])
		}
	}
}

// A node with one slot: deleting the sandbox in it does not free the slot until
// the node reports it deleted, so a create racing the delete cannot overcommit
// the node while the VM is still being torn down (#116).
func testDeletingHoldsItsSlotUntilDeleted(t *testing.T, s Store) {
	lifecycleStore(t, s, 1, "node-a")
	sb := runningOn(t, s, "node-a")
	if _, err := s.DeleteSandbox(context.Background(), sb.ID, ""); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreateSandbox(context.Background(), CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 512, NodeID: "node-a"})
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("create while the VM is still being removed: %v, want no capacity", err)
	}
	report(t, s, sb.ID, SandboxDeleted, "vmm and disk removed")
	if _, err := s.CreateSandbox(context.Background(), CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 512, NodeID: "node-a"}); err != nil {
		t.Fatalf("create after the node reported deleted: %v", err)
	}
}

func TestMemoryDeletingHoldsItsSlotUntilDeleted(t *testing.T) {
	testDeletingHoldsItsSlotUntilDeleted(t, NewMemoryStore())
}
func TestPostgresDeletingHoldsItsSlotUntilDeleted(t *testing.T) {
	testDeletingHoldsItsSlotUntilDeleted(t, newPostgresTestStore(t))
}
