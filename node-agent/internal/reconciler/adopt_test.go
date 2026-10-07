package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/egress"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/metrics"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/poddaemon"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/tap"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

// adoptAgent is an agent process: a reconciler with its own engine, in the
// directories every process of the agent shares.
func adoptAgent(t *testing.T, cp *fakeCP, eng vmm.MicroVM, share *Reconciler) (*Reconciler, *egress.PolicyCache) {
	t.Helper()
	rec, _ := retainRec(t, cp, eng)
	cache := &egress.PolicyCache{}
	rec.Egress = cache
	rec.TapAuto = true
	rec.Tap = &tap.Manager{Runner: &tap.RecordingRunner{}}
	rec.Registry = poddaemon.NewRegistry(nil)
	if share != nil {
		rec.VsockDir, rec.DiskDir, rec.RootFSPath, rec.StateDir = share.VsockDir, share.DiskDir, share.RootFSPath, share.StateDir
	} else {
		rec.StateDir = filepath.Join(rec.VsockDir, "state")
	}
	return rec, cache
}

func readRecord(t *testing.T, rec *Reconciler, id string) vmState {
	t.Helper()
	b, err := os.ReadFile(rec.statePath(id))
	if err != nil {
		t.Fatalf("no record of %s: %v", id, err)
	}
	var st vmState
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// A VM that is up is recorded for an agent that comes after, and forgotten when it ends.
func TestAVMThatIsUpIsRecordedAndForgottenWhenItEnds(t *testing.T) {
	cp := newFakeCP(t, idA)
	rec, _ := adoptAgent(t, cp, vmm.NewFakeVMM(nil), nil)
	if _, err := os.Stat(rec.statePath(idA)); !os.IsNotExist(err) {
		t.Fatal("a record before the VM exists")
	}
	rec.tick(context.Background())

	st := readRecord(t, rec, idA)
	h, _ := rec.HandleOf(idA)
	if st.Version != stateVersion || st.SandboxID != idA || st.TenantID != "t1" || st.CID != h.CID || st.Slot != 0 ||
		st.TapName != "asp-aaaaaaaa" || st.VsockPath != h.VsockPath || st.RootFS != h.RootFS || st.StartedAt.IsZero() {
		t.Fatalf("record %+v for handle %+v", st, h)
	}
	fi, err := os.Stat(rec.statePath(idA))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("record mode %v %v", fi.Mode(), err)
	}

	cp.setState(idA, "stopping")
	rec.tick(context.Background())
	if _, err := os.Stat(rec.statePath(idA)); !os.IsNotExist(err) {
		t.Fatalf("a stopped VM keeps its record: %v", err)
	}
}

// The new agent takes over what the old one left running and does not touch it.
func TestAnAgentThatRestartsAdoptsTheVMsThatAreStillRunning(t *testing.T) {
	cp := newFakeCP(t, idA, idB)
	old, _ := adoptAgent(t, cp, vmm.NewFakeVMM(nil), nil)
	old.tick(context.Background())
	if got, _ := cp.state(idA); got != "running" {
		t.Fatalf("start: %s", got)
	}
	recA, recB := readRecord(t, old, idA), readRecord(t, old, idB)
	if recA.CID == recB.CID || recA.Slot == recB.Slot {
		t.Fatalf("two VMs share a CID or a /30: %+v %+v", recA, recB)
	}

	// The agent process is replaced. idA's VM kept running; idB's died with it.
	fresh := vmm.NewFakeVMM(nil)
	fresh.Running[idA] = vmm.MicroVMConfig{ID: idA}
	reg := metrics.NewRegistry()
	next, cache := adoptAgent(t, cp, fresh, old)
	next.Metrics = NewMetrics(reg)
	adopted := next.Adopt(context.Background())

	if !slices.Equal(adopted, []string{idA}) {
		t.Fatalf("adopted %v, want [%s]", adopted, idA)
	}
	if got := next.Handles(); !slices.Equal(got, []string{idA}) {
		t.Fatalf("handles %v", got)
	}
	h, _ := next.HandleOf(idA)
	if h.CID != recA.CID || h.TapName != recA.TapName || h.Slot != recA.Slot || h.RootFS != recA.RootFS || h.VsockPath != recA.VsockPath {
		t.Fatalf("handle %+v does not match the record %+v", h, recA)
	}
	if _, err := os.Stat(next.statePath(idB)); !os.IsNotExist(err) {
		t.Fatal("the record of a VM that is gone stayed")
	}
	wantMetrics(t, reg, `asp_agent_vms_adopted_total{result="ok"} 1`, `asp_agent_vms_adopted_total{result="stale"} 1`)

	// What the VM held is kept out of reach of new VMs, and put back where the agent looks for it.
	if cid := next.allocCID(); cid == recA.CID {
		t.Fatalf("the CID %d of an adopted VM was handed out again", cid)
	}
	if n, _, err := next.allocSlot(); err != nil || n == recA.Slot {
		t.Fatalf("slot %d (err %v): the /30 of an adopted VM was handed out again", n, err)
	}
	guest, err := tap.Slot(netip.MustParsePrefix(tap.DefaultGuestSubnet), recA.Slot)
	if err != nil {
		t.Fatal(err)
	}
	if got := cache.SandboxFor(guest.Guest); got != idA {
		t.Fatalf("the egress proxy attributes %s to %q, want %s", guest.Guest, got, idA)
	}
	if ep, ok := next.Registry.Lookup(idA); !ok || ep.Mode != poddaemon.ModeHybrid || ep.VsockPath != recA.VsockPath {
		t.Fatalf("exec endpoint %+v %v", ep, ok)
	}

	// Its first poll finds nothing to start and nothing to stop.
	cp.setState(idB, "failed") // idB's VM died with the old agent
	next.tick(context.Background())
	if got, _ := cp.state(idA); got != "running" {
		t.Fatalf("the adopted sandbox is %s after a poll", got)
	}
	for _, call := range fresh.Calls {
		if strings.HasPrefix(call, "start:") || strings.HasPrefix(call, "stop:") {
			t.Fatalf("the new agent touched the adopted VM: %q", fresh.Calls)
		}
	}

	// And it is a VM like any other from then on: a stop works through the adopted handle.
	cp.setState(idA, "stopping")
	next.tick(context.Background())
	if got, _ := cp.state(idA); got != "stopped" {
		t.Fatalf("stop: %s", got)
	}
	if !slices.Contains(fresh.Calls, "stop:"+idA) {
		t.Fatalf("the adopted VM was not stopped: %q", fresh.Calls)
	}
	if _, err := os.Stat(next.statePath(idA)); !os.IsNotExist(err) {
		t.Fatal("record of a stopped VM")
	}
	if _, ok := next.HandleOf(idA); ok {
		t.Fatal("handle of a stopped VM")
	}
}

// An adopted VM that later dies is reported like any other.
func TestAnAdoptedVMThatDiesIsReported(t *testing.T) {
	cp := newFakeCP(t, idA)
	old, _ := adoptAgent(t, cp, vmm.NewFakeVMM(nil), nil)
	old.tick(context.Background())
	fresh := vmm.NewFakeVMM(nil)
	fresh.Running[idA] = vmm.MicroVMConfig{ID: idA}
	next, _ := adoptAgent(t, cp, fresh, old)
	next.Adopt(context.Background())
	fresh.Crash(idA, errors.New("the service ended: result=signal (code 2, status 9)"), time.Hour)
	next.tick(context.Background())
	if state, detail := cp.state(idA); state != "stopped" || !strings.HasPrefix(detail, "vmm_exited: the service ended") {
		t.Fatalf("state=%s detail=%q", state, detail)
	}
}

func TestAdoptRemovesRecordsItCannotUse(t *testing.T) {
	cp := newFakeCP(t, idA)
	rec, _ := adoptAgent(t, cp, vmm.NewFakeVMM(nil), nil)
	good := vmState{Version: stateVersion, SandboxID: idB, VsockPath: "/run/x.sock", Slot: -1, CID: 7, StartedAt: time.Now()}
	if err := writeState(rec.statePath(idB), good); err != nil {
		t.Fatal(err)
	}
	junk := rec.statePath(idC)
	writeFile(t, junk, "{not json")
	otherVersion := rec.statePath("cccccccc-0004-4000-8000-000000000004")
	writeFile(t, otherVersion, `{"version":99,"sandbox_id":"cccccccc-0004-4000-8000-000000000004","vsock_path":"/x"}`)
	misnamed := rec.statePath("dddddddd-0005-4000-8000-000000000005")
	writeFile(t, misnamed, `{"version":1,"sandbox_id":"someone-else","vsock_path":"/x"}`)
	notOurs := filepath.Join(rec.StateDir, "notes.json")
	writeFile(t, notOurs, "{}")

	// idB has a good record but no VM behind it (FakeVMM has nothing running).
	if got := rec.Adopt(context.Background()); len(got) != 0 {
		t.Fatalf("adopted %v", got)
	}
	for _, p := range []string{junk, otherVersion, misnamed, rec.statePath(idB)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s was kept", filepath.Base(p))
		}
	}
	if _, err := os.Stat(notOurs); err != nil {
		t.Error("a file that is not a record of a sandbox was removed")
	}
}

func TestAdoptNeedsAStateDirAndAnEngineThatCan(t *testing.T) {
	cp := newFakeCP(t, idA)
	rec, _ := adoptAgent(t, cp, vmm.NewFakeVMM(nil), nil)
	rec.StateDir = ""
	if rec.Adopt(context.Background()) != nil {
		t.Fatal("adopted with no state dir")
	}
	rec.StateDir = t.TempDir()
	rec.Engine = onlyStarts{rec.Engine}
	if rec.Adopt(context.Background()) != nil {
		t.Fatal("adopted with an engine that cannot")
	}
}

// onlyStarts hides everything but vmm.MicroVM of an engine.
type onlyStarts struct{ vmm.MicroVM }

func TestAdoptableIDsAreTheRecordsWithALiveVM(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{idA, idB, idC} {
		if err := writeState(filepath.Join(dir, id+".json"), vmState{Version: stateVersion, SandboxID: id, VsockPath: "/x", StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	alive := func(_ context.Context, id string) error {
		if id == idB {
			return errors.New("its service is not active")
		}
		return nil
	}
	got := AdoptableIDs(context.Background(), dir, alive, nil)
	slices.Sort(got)
	if !slices.Equal(got, []string{idA, idC}) {
		t.Fatalf("adoptable %v", got)
	}
	// Probing does not remove anything: the cleanup that follows does.
	if _, err := os.Stat(filepath.Join(dir, idB+".json")); err != nil {
		t.Fatal("the probe removed a record")
	}
	if AdoptableIDs(context.Background(), "", alive, nil) != nil || AdoptableIDs(context.Background(), dir, nil, nil) != nil {
		t.Fatal("no dir or no probe, nothing adoptable")
	}
	if AdoptableIDs(context.Background(), filepath.Join(dir, "missing"), alive, nil) != nil {
		t.Fatal("a missing dir")
	}
}

func TestReserveCIDAndSlotKeepHolesFree(t *testing.T) {
	r := &Reconciler{}
	r.reserveCID(2) // not a guest CID
	r.reserveCID(6)
	// 3, 4 and 5 are free for new VMs; 6 is taken.
	got := map[uint32]bool{}
	for i := 0; i < 4; i++ {
		got[r.allocCID()] = true
	}
	for _, want := range []uint32{3, 4, 5, 7} {
		if !got[want] {
			t.Errorf("CID %d not handed out: %v", want, got)
		}
	}
	if got[6] {
		t.Error("CID 6 is held by an adopted VM")
	}
	r2 := &Reconciler{}
	r2.reserveCID(4)
	r2.reserveCID(3) // a hole being filled
	if c := r2.allocCID(); c != 5 {
		t.Errorf("next CID %d, want 5", c)
	}
	r3 := &Reconciler{}
	r3.reserveSlot(2)
	r3.reserveSlot(-1)
	slots := map[int]bool{}
	for i := 0; i < 3; i++ {
		n, _, err := r3.allocSlot()
		if err != nil {
			t.Fatal(err)
		}
		slots[n] = true
	}
	if slots[2] || !slots[0] || !slots[1] || !slots[3] {
		t.Errorf("slots handed out: %v", slots)
	}
}
