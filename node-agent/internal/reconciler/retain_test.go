package reconciler

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/poddaemon"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

const (
	idA = "aaaaaaaa-0001-4000-8000-000000000001"
	idB = "bbbbbbbb-0002-4000-8000-000000000002"
	idC = "cccccccc-0003-4000-8000-000000000003"
)

// retainRec is a reconciler with a disk directory whose clone writes "base",
// against a control plane that keeps stopped sandboxes' disks.
func retainRec(t *testing.T, cp *fakeCP, eng vmm.MicroVM) (*Reconciler, string) {
	t.Helper()
	dir := t.TempDir()
	rec := New(cp.client(), "n1", eng, nil, time.Hour)
	rec.VsockDir = shortTempDir(t)
	rec.RootFSPath = filepath.Join(dir, "base.img")
	rec.DiskDir = filepath.Join(dir, "disks")
	rec.CloneDisk = func(_, dst string) error { return os.WriteFile(dst, []byte("base"), 0o644) }
	cp.retains = true
	return rec, rec.DiskDir
}

func diskOf(dir, id string) string { return filepath.Join(dir, "rootfs-"+id+".img") }

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Stopping a sandbox keeps its disk when the control plane keeps stopped
// sandboxes, and the control plane hears that the VM is stopped.
func TestStopKeepsTheDiskWhenTheControlPlaneRetains(t *testing.T) {
	cp := newFakeCP(t, idA)
	fake := vmm.NewFakeVMM(nil)
	rec, disks := retainRec(t, cp, fake)
	rec.tick(context.Background())
	if _, ok := rec.HandleOf(idA); !ok || !exists(diskOf(disks, idA)) {
		t.Fatal("the sandbox did not start with a disk")
	}
	if err := os.WriteFile(diskOf(disks, idA), []byte("guest data"), 0o600); err != nil {
		t.Fatal(err)
	}

	cp.setState(idA, "stopping")
	rec.tick(context.Background())

	if got, _ := cp.state(idA); got != "stopped" {
		t.Fatalf("state=%s, want stopped", got)
	}
	if _, ok := rec.HandleOf(idA); ok {
		t.Fatal("handle kept after the stop")
	}
	if len(fake.Running) != 0 {
		t.Fatalf("VM still running: %v", fake.Running)
	}
	b, err := os.ReadFile(diskOf(disks, idA))
	if err != nil || string(b) != "guest data" {
		t.Fatalf("the stop did not keep the disk: %v %q", err, b)
	}
}

// A control plane that does not send the retained list still means stop =
// delete: the disk goes, as it always did.
func TestStopRemovesTheDiskWithAnOlderControlPlane(t *testing.T) {
	cp := newFakeCP(t, idA)
	rec, disks := retainRec(t, cp, vmm.NewFakeVMM(nil))
	cp.retains = false
	rec.tick(context.Background())
	if !exists(diskOf(disks, idA)) {
		t.Fatal("no disk")
	}
	cp.setState(idA, "stopping")
	rec.tick(context.Background())
	if exists(diskOf(disks, idA)) {
		t.Fatal("an older control plane's stop must remove the disk")
	}
}

// Deleting removes the disk of a sandbox that was stopped (no VM, no handle)
// and of one that is still running, and reports deleted.
func TestDeleteRemovesTheDisk(t *testing.T) {
	cp := newFakeCP(t, idA, idB)
	fake := vmm.NewFakeVMM(nil)
	rec, disks := retainRec(t, cp, fake)
	rec.tick(context.Background())

	cp.setState(idA, "stopping")
	rec.tick(context.Background())
	if !exists(diskOf(disks, idA)) {
		t.Fatal("the stop did not keep the disk")
	}
	cp.setState(idA, "deleting") // stopped, then deleted: no VM left
	cp.setState(idB, "deleting") // running, deleted
	rec.tick(context.Background())

	for _, id := range []string{idA, idB} {
		if got, _ := cp.state(id); got != "deleted" {
			t.Fatalf("%s: state=%s, want deleted", id, got)
		}
		if exists(diskOf(disks, id)) {
			t.Fatalf("%s: disk survived the delete", id)
		}
	}
	if len(fake.Running) != 0 || len(rec.Handles()) != 0 {
		t.Fatalf("running=%v handles=%v", fake.Running, rec.Handles())
	}
}

// A resume (boot_count above 1) boots the disk its stop kept: no new clone,
// and what the guest wrote is still there.
func TestResumeReusesTheDisk(t *testing.T) {
	cp := newFakeCP(t, idA)
	fake := vmm.NewFakeVMM(nil)
	rec, disks := retainRec(t, cp, fake)
	clones := 0
	rec.CloneDisk = func(_, dst string) error { clones++; return os.WriteFile(dst, []byte("base"), 0o644) }
	rec.tick(context.Background())
	if clones != 1 {
		t.Fatalf("first boot cloned %d times", clones)
	}
	if err := os.WriteFile(diskOf(disks, idA), []byte("guest data"), 0o600); err != nil {
		t.Fatal(err)
	}
	cp.setState(idA, "stopping")
	rec.tick(context.Background())

	cp.mu.Lock()
	cp.boxes[idA].State, cp.boxes[idA].BootCount = "requested", 2 // what the control plane's resume does
	cp.mu.Unlock()
	rec.tick(context.Background())

	if got, _ := cp.state(idA); got != "running" {
		t.Fatalf("state=%s, want running", got)
	}
	if clones != 1 {
		t.Fatalf("the resume cloned a new disk (%d clones)", clones)
	}
	cfg, ok := fake.RunningConfig(idA)
	if !ok || cfg.RootFSPath != diskOf(disks, idA) {
		t.Fatalf("booted %q, want the retained disk", cfg.RootFSPath)
	}
	if b, _ := os.ReadFile(diskOf(disks, idA)); string(b) != "guest data" {
		t.Fatalf("disk content=%q", b)
	}
}

// A resume whose disk is gone fails for good: it never boots a blank one.
func TestResumeWithoutItsDiskFails(t *testing.T) {
	cp := newFakeCP(t, idA)
	fake := vmm.NewFakeVMM(nil)
	rec, disks := retainRec(t, cp, fake)
	cp.mu.Lock()
	cp.boxes[idA].BootCount = 2
	cp.mu.Unlock()
	rec.tick(context.Background())

	got, detail := cp.state(idA)
	if got != "failed" || !strings.Contains(detail, "disk_lost") {
		t.Fatalf("state=%s detail=%q, want failed with disk_lost", got, detail)
	}
	if len(fake.Configs) != 0 || exists(diskOf(disks, idA)) {
		t.Fatal("a VM or a blank disk was made for a resume")
	}
}

// failingEngine cannot start a VM.
type failingEngine struct{ *vmm.FakeVMM }

func (failingEngine) Start(context.Context, vmm.MicroVMConfig) error {
	return errors.New("cloud-hypervisor: vm.boot failed")
}

// A resume whose VM does not start goes back to stopped with its disk, so it
// can be tried again; a first start that fails is failed and loses its disk.
func TestFailedResumeReturnsToStoppedWithItsDisk(t *testing.T) {
	cp := newFakeCP(t, idA, idB)
	rec, disks := retainRec(t, cp, failingEngine{vmm.NewFakeVMM(nil)})
	if err := os.MkdirAll(disks, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(diskOf(disks, idA), []byte("guest data"), 0o600); err != nil {
		t.Fatal(err)
	}
	cp.mu.Lock()
	cp.boxes[idA].BootCount = 2
	cp.mu.Unlock()
	rec.tick(context.Background())

	got, detail := cp.state(idA)
	if got != "stopped" || !strings.Contains(detail, "resume failed") {
		t.Fatalf("resume: state=%s detail=%q, want stopped with 'resume failed'", got, detail)
	}
	if b, _ := os.ReadFile(diskOf(disks, idA)); string(b) != "guest data" {
		t.Fatalf("the failed resume lost the disk: %q", b)
	}
	got, _ = cp.state(idB)
	if got != "failed" || exists(diskOf(disks, idB)) {
		t.Fatalf("first start: state=%s, disk exists=%v; want failed and no disk", got, exists(diskOf(disks, idB)))
	}
}

// The GC removes the rootfs copies no sandbox owns, and only those.
func TestDiskGCRemovesOnlyUnownedDisks(t *testing.T) {
	cp := newFakeCP(t, idA, idB)
	rec, disks := retainRec(t, cp, vmm.NewFakeVMM(nil))
	if err := os.MkdirAll(disks, 0o700); err != nil {
		t.Fatal(err)
	}
	cp.setState(idB, "stopped") // retained, no VM
	for id, content := range map[string]string{idB: "kept by its stop", idC: "nobody's"} {
		if err := os.WriteFile(diskOf(disks, id), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	other := filepath.Join(disks, "notes.txt")
	if err := os.WriteFile(other, []byte("not a disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec.tick(context.Background())

	if !exists(diskOf(disks, idA)) {
		t.Fatal("the disk of a sandbox this node is running was removed")
	}
	if !exists(diskOf(disks, idB)) {
		t.Fatal("the disk of a retained sandbox was removed")
	}
	if exists(diskOf(disks, idC)) {
		t.Fatal("a disk nobody owns survived the GC")
	}
	if !exists(other) {
		t.Fatal("a file that is not a rootfs copy was removed")
	}
}

// The GC runs after a poll, at most once a minute, and not at all against a
// control plane that does not say what is assigned.
func TestDiskGCIsRateLimitedAndNeedsAssigned(t *testing.T) {
	cp := newFakeCP(t)
	rec, disks := retainRec(t, cp, vmm.NewFakeVMM(nil))
	now := time.Now()
	rec.now = func() time.Time { return now }
	if err := os.MkdirAll(disks, 0o700); err != nil {
		t.Fatal(err)
	}
	orphan := func(id string) string {
		p := diskOf(disks, id)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	cp.legacy = true // no assigned set: nothing may be called missing
	a := orphan(idA)
	rec.tick(context.Background())
	if !exists(a) {
		t.Fatal("GC ran against a control plane that sent no assigned set")
	}

	cp.legacy = false
	rec.tick(context.Background())
	if exists(a) {
		t.Fatal("GC did not remove an unowned disk")
	}
	b := orphan(idB)
	rec.tick(context.Background())
	if !exists(b) {
		t.Fatal("GC ran twice within a minute")
	}
	now = now.Add(diskGCEvery + time.Second)
	rec.tick(context.Background())
	if exists(b) {
		t.Fatal("GC did not run after a minute")
	}
}

// Clone and resume refuse to run on a nearly full --disk-dir.
func TestLowDiskSpaceStopsAStart(t *testing.T) {
	cp := newFakeCP(t, idA)
	fake := vmm.NewFakeVMM(nil)
	rec, disks := retainRec(t, cp, fake)
	rec.MinFreeDiskMiB = 1000
	rec.FreeDisk = func(string) (uint64, error) { return 100 << 20, nil }
	rec.tick(context.Background())

	got, detail := cp.state(idA)
	if got != "failed" || !strings.Contains(detail, "100 MiB free") {
		t.Fatalf("state=%s detail=%q", got, detail)
	}
	if len(fake.Configs) != 0 || exists(diskOf(disks, idA)) {
		t.Fatal("a disk was made on a full filesystem")
	}

	// With room, it starts.
	rec.FreeDisk = func(string) (uint64, error) { return 5000 << 20, nil }
	cp.setState(idA, "requested")
	rec.tick(context.Background())
	if got, _ := cp.state(idA); got != "running" {
		t.Fatalf("state=%s, want running", got)
	}
}

// startPowerGuest plays a guest behind Cloud Hypervisor's hybrid vsock muxer:
// it answers /healthz and records the exec that powers it off.
func startPowerGuest(t *testing.T, path string, record func(string)) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				br := bufio.NewReader(conn)
				if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "CONNECT ") {
					return
				}
				_, _ = fmt.Fprintf(conn, "OK 1073741824\n")
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				switch req.URL.Path {
				case "/healthz":
					_, _ = fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
				case "/v1/exec":
					body, _ := io.ReadAll(req.Body)
					record("poweroff:" + string(body))
					// The guest goes down before it answers.
				}
			}()
		}
	}()
}

// powerEngine is FakeVMM with a guest on the muxer socket and a Shutdowner
// that records when the reconciler waits for the power-off.
type powerEngine struct {
	*vmm.FakeVMM
	t      *testing.T
	mu     sync.Mutex
	events []string
	down   bool // what WaitShutdown reports
}

func (e *powerEngine) Start(ctx context.Context, cfg vmm.MicroVMConfig) error {
	if err := e.FakeVMM.Start(ctx, cfg); err != nil {
		return err
	}
	startPowerGuest(e.t, cfg.VsockPath, e.record)
	return nil
}

func (e *powerEngine) record(event string) {
	e.mu.Lock()
	e.events = append(e.events, event)
	e.mu.Unlock()
}

func (e *powerEngine) Stop(ctx context.Context, id string) error {
	e.record("stop")
	return e.FakeVMM.Stop(ctx, id)
}

func (e *powerEngine) WaitShutdown(context.Context, string, time.Duration) bool {
	e.record("wait")
	return e.down
}

func (e *powerEngine) seen() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string{}, e.events...)
}

// A stop that keeps the disk first asks the guest to power off, waits for its
// VM to go down, and only then stops the VMM.
func TestStopAsksTheGuestToPowerOffFirst(t *testing.T) {
	cp := newFakeCP(t, idA)
	eng := &powerEngine{FakeVMM: vmm.NewFakeVMM(nil), t: t, down: true}
	rec, disks := retainRec(t, cp, eng)
	rec.Registry = poddaemon.NewRegistry(nil)
	rec.GuestReadyTimeout = 5 * time.Second
	rec.StopGrace = 5 * time.Second
	rec.tick(context.Background())
	if got, _ := cp.state(idA); got != "running" {
		t.Fatalf("state=%s, want running", got)
	}

	cp.setState(idA, "stopping")
	rec.tick(context.Background())

	events := eng.seen()
	if len(events) != 3 || !strings.HasPrefix(events[0], "poweroff:") || events[1] != "wait" || events[2] != "stop" {
		t.Fatalf("events=%q, want poweroff, wait, stop in that order", events)
	}
	if !strings.Contains(events[0], "systemctl poweroff") || !strings.Contains(events[0], "sync") {
		t.Fatalf("poweroff request=%q", events[0])
	}
	if got, _ := cp.state(idA); got != "stopped" || !exists(diskOf(disks, idA)) {
		t.Fatalf("state=%s, disk exists=%v", got, exists(diskOf(disks, idA)))
	}
}

// A guest that does not power off in time is stopped hard anyway, and the stop
// still completes with its disk kept.
func TestStopFallsBackToAHardStop(t *testing.T) {
	cp := newFakeCP(t, idA)
	eng := &powerEngine{FakeVMM: vmm.NewFakeVMM(nil), t: t, down: false}
	rec, disks := retainRec(t, cp, eng)
	rec.Registry = poddaemon.NewRegistry(nil)
	rec.GuestReadyTimeout = 5 * time.Second
	rec.StopGrace = time.Second
	rec.tick(context.Background())

	cp.setState(idA, "stopping")
	rec.tick(context.Background())

	events := eng.seen()
	if len(events) != 3 || events[1] != "wait" || events[2] != "stop" {
		t.Fatalf("events=%q", events)
	}
	if got, _ := cp.state(idA); got != "stopped" || !exists(diskOf(disks, idA)) {
		t.Fatalf("state=%s, disk exists=%v", got, exists(diskOf(disks, idA)))
	}
}

// Deleting, self-fencing and a control plane that does not keep disks stop
// hard: there is no disk worth a clean shutdown.
func TestOnlyAStopThatKeepsTheDiskPowersTheGuestOff(t *testing.T) {
	cp := newFakeCP(t, idA)
	eng := &powerEngine{FakeVMM: vmm.NewFakeVMM(nil), t: t, down: true}
	rec, _ := retainRec(t, cp, eng)
	rec.Registry = poddaemon.NewRegistry(nil)
	rec.GuestReadyTimeout = 5 * time.Second
	rec.StopGrace = 5 * time.Second
	rec.tick(context.Background())

	cp.setState(idA, "deleting")
	rec.tick(context.Background())
	if got := eng.seen(); !reflect.DeepEqual(got, []string{"stop"}) {
		t.Fatalf("events=%q, want only a hard stop", got)
	}
}

// The node can say how much room its disks have left.
func TestDiskFreeMiB(t *testing.T) {
	mib, ok := DiskFreeMiB(t.TempDir())
	if !ok || mib <= 0 {
		t.Fatalf("DiskFreeMiB of a temp dir: %d %v", mib, ok)
	}
	if _, ok := DiskFreeMiB(filepath.Join(t.TempDir(), "missing", "dir")); ok {
		t.Fatal("a missing directory has no free space to report")
	}
}

// A stop sent while the VM is still booting makes the running report fail with a
// conflict, and the node tears the VM down. With a control plane that keeps
// disks the disk stays, so the stopped sandbox can be resumed; the disk GC
// removes it later if the sandbox turns out not to be anyone's.
func TestStopWhileBootingKeepsTheDisk(t *testing.T) {
	cp := newFakeCP(t, idA)
	cp.conflict[idA] = true // the sandbox is stopping by the time the node reports running
	fake := vmm.NewFakeVMM(nil)
	rec, disks := retainRec(t, cp, fake)
	rec.tick(context.Background())

	if _, ok := rec.HandleOf(idA); ok || len(fake.Running) != 0 {
		t.Fatal("the VM kept running after its running report was refused")
	}
	if !exists(diskOf(disks, idA)) {
		t.Fatal("the self-fence removed a disk the control plane keeps")
	}

	// With an older control plane nothing keeps it, so it goes as it always did.
	cp2 := newFakeCP(t, idB)
	cp2.conflict[idB] = true
	rec2, disks2 := retainRec(t, cp2, vmm.NewFakeVMM(nil))
	cp2.retains = false
	rec2.tick(context.Background())
	if exists(diskOf(disks2, idB)) {
		t.Fatal("an older control plane's self-fence must remove the disk")
	}
}

// The same boot, then the CP's own stop: the node ends the stop and the disk is
// there for the resume.
func TestStopWhileBootingCanBeResumed(t *testing.T) {
	cp := newFakeCP(t, idA)
	fake := vmm.NewFakeVMM(nil)
	rec, disks := retainRec(t, cp, fake)
	cp.mu.Lock()
	cp.conflict[idA] = true
	cp.mu.Unlock()
	rec.tick(context.Background()) // boots, the report is refused, the VM is torn down

	cp.mu.Lock()
	cp.conflict[idA] = false
	cp.mu.Unlock()
	cp.setState(idA, "stopping")
	rec.tick(context.Background()) // the stop completes
	if got, _ := cp.state(idA); got != "stopped" || !exists(diskOf(disks, idA)) {
		t.Fatalf("state=%s, disk exists=%v; want stopped with its disk", got, exists(diskOf(disks, idA)))
	}

	cp.mu.Lock()
	cp.boxes[idA].State, cp.boxes[idA].BootCount = "requested", 2
	cp.mu.Unlock()
	rec.tick(context.Background())
	if got, _ := cp.state(idA); got != "running" {
		t.Fatalf("the resume ended %s", got)
	}
	if cfg, ok := fake.RunningConfig(idA); !ok || cfg.RootFSPath != diskOf(disks, idA) {
		t.Fatalf("resumed on %q, want the kept disk", cfg.RootFSPath)
	}
}
