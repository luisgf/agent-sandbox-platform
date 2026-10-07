package reconciler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/cpclient"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/tap"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

const testUIDBase = 5_000_000

// chownLog records the ownership changes a start and a stop ask for; a test is
// not root and could not make them.
type chownLog struct {
	mu    sync.Mutex
	calls []chownCall
}

type chownCall struct {
	path     string
	uid, gid int
}

func (c *chownLog) chown(path string, uid, gid int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, chownCall{path, uid, gid})
	return nil
}

// last is the user the last change gave path to.
func (c *chownLog) last(path string) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.calls) - 1; i >= 0; i-- {
		if c.calls[i].path == path {
			return c.calls[i].uid, true
		}
	}
	return 0, false
}

// userAgent is an agent whose VMMs run as users of their own.
func userAgent(t *testing.T, cp *fakeCP, eng vmm.MicroVM) (*Reconciler, *chownLog, *tap.RecordingRunner) {
	t.Helper()
	rec, _ := adoptAgent(t, cp, eng, nil)
	runner := &tap.RecordingRunner{}
	rec.Tap = &tap.Manager{Runner: runner, SysClassNet: t.TempDir()}
	rec.Confine = &vmm.Confinement{Unprivileged: &vmm.Unprivileged{UIDBase: testUIDBase, RunDir: filepath.Join(shortTempDir(t), "vm")}}
	rec.GuestHost = &recordingGuestHost{}
	log := &chownLog{}
	rec.Chown = log.chown
	return rec, log, runner
}

// A VM whose VMM runs as a user of its own is given a directory, the sockets
// the VMM connects to, its disk and its TAP, and loses them when it stops.
func TestAVMThatRunsAsAUserIsGivenItsFilesAndLosesThemWhenItStops(t *testing.T) {
	cp := newFakeCP(t, idA)
	fake := vmm.NewFakeVMM(nil)
	rec, log, runner := userAgent(t, cp, fake)
	rec.tick(context.Background())

	h, ok := rec.HandleOf(idA)
	if !ok {
		t.Fatal("the sandbox did not start")
	}
	uid := testUIDBase + int(h.CID)
	if h.UID != uint32(uid) {
		t.Fatalf("handle user %d, want %d", h.UID, uid)
	}
	dir := rec.unprivileged().Dir(idA)
	if h.RunDir != dir {
		t.Fatalf("handle directory %s, want %s", h.RunDir, dir)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("the VM directory: %v %v", fi, err)
	}

	cfg, _ := fake.RunningConfig(idA)
	if cfg.RunDir != dir || cfg.VsockPath != filepath.Join(dir, vmm.RunVsockSocket) || cfg.SerialSocket != filepath.Join(dir, vmm.RunSerialSocket) {
		t.Fatalf("the VMM's sockets are not in its directory: %+v", cfg)
	}
	if !strings.HasPrefix(cfg.VsockPath, rec.unprivileged().RunDir) {
		t.Fatalf("vsock %s is outside the VM directory root", cfg.VsockPath)
	}

	disk := diskOf(rec.DiskDir, idA)
	for _, path := range []string{
		dir, disk,
		filepath.Join(dir, vmm.RunVsockSocket) + "_26501", // the SSH agent acceptor
		filepath.Join(dir, vmm.RunVsockSocket) + "_26502", // the identity acceptor
	} {
		if got, ok := log.last(path); !ok || got != uid {
			t.Errorf("%s was given to %d (changed: %v), want %d", path, got, ok, uid)
		}
	}
	if want := "ip tuntap add dev asp-aaaaaaaa mode tap user " + strconv.Itoa(uid); runner.Calls[0] != want {
		t.Errorf("TAP created with %q, want %q", runner.Calls[0], want)
	}
	if st := readRecord(t, rec, idA); st.UID != uint32(uid) || st.RunDir != dir {
		t.Fatalf("record %+v lacks the VM's user and directory", st)
	}

	cp.setState(idA, "stopping")
	rec.tick(context.Background())
	if exists(dir) {
		t.Fatal("the VM directory stayed after the stop")
	}
	// The disk of a stopped sandbox stays, and is root's again.
	if !exists(disk) {
		t.Fatal("the disk of a stopped sandbox was removed")
	}
	if got, ok := log.last(disk); !ok || got != 0 {
		t.Fatalf("the disk was left to user %d (changed: %v)", got, ok)
	}
}

// Two VMs running at once never share a user.
func TestVMsThatRunAtOnceHaveDifferentUsers(t *testing.T) {
	cp := newFakeCP(t, idA, idB)
	rec, _, _ := userAgent(t, cp, vmm.NewFakeVMM(nil))
	rec.tick(context.Background())
	a, okA := rec.HandleOf(idA)
	b, okB := rec.HandleOf(idB)
	if !okA || !okB || a.UID == 0 || a.UID == b.UID || a.RunDir == b.RunDir {
		t.Fatalf("handles %+v %+v", a, b)
	}
}

// A VMM that cannot start leaves no directory and no disk behind (a first boot),
// and gives a retained disk back to root (a resume).
func TestAFailedStartTakesBackWhatItGave(t *testing.T) {
	cp := newFakeCP(t, idA)
	rec, log, _ := userAgent(t, cp, failingEngine{vmm.NewFakeVMM(nil)})
	rec.tick(context.Background())
	if st, _ := cp.state(idA); st != "failed" {
		t.Fatalf("state %s", st)
	}
	if exists(rec.unprivileged().Dir(idA)) {
		t.Fatal("the VM directory of a failed start stayed")
	}
	if exists(diskOf(rec.DiskDir, idA)) {
		t.Fatal("the disk of a failed first boot stayed")
	}

	// A resume: the disk is the sandbox's and survives, in root's hands.
	cp2 := newFakeCP(t, idB)
	cp2.boxes[idB].BootCount = 2
	now := time.Now()
	cp2.boxes[idB].BootedAt = &now
	rec2, log2, _ := userAgent(t, cp2, failingEngine{vmm.NewFakeVMM(nil)})
	if err := os.MkdirAll(rec2.DiskDir, 0o700); err != nil {
		t.Fatal(err)
	}
	disk := diskOf(rec2.DiskDir, idB)
	if err := os.WriteFile(disk, []byte("guest data"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec2.tick(context.Background())
	if !exists(disk) {
		t.Fatal("a failed resume removed the retained disk")
	}
	if got, ok := log2.last(disk); !ok || got != 0 {
		t.Fatalf("the retained disk was left to user %d (changed: %v)", got, ok)
	}
	if exists(rec2.unprivileged().Dir(idB)) {
		t.Fatal("the VM directory of a failed resume stayed")
	}
	_ = log
}

// An agent that restarts takes over a VM that runs as a user, with its directory,
// and releases all of it when the VM ends.
func TestAnAgentThatRestartsAdoptsAVMThatRunsAsAUser(t *testing.T) {
	cp := newFakeCP(t, idA)
	old, _, _ := userAgent(t, cp, vmm.NewFakeVMM(nil))
	old.tick(context.Background())
	before, _ := old.HandleOf(idA)
	st := readRecord(t, old, idA)
	if st.UID == 0 || st.RunDir == "" {
		t.Fatalf("record %+v", st)
	}

	fresh := vmm.NewFakeVMM(nil)
	fresh.Running[idA] = vmm.MicroVMConfig{ID: idA}
	next, log, _ := userAgent(t, cp, fresh)
	next.VsockDir, next.DiskDir, next.RootFSPath, next.StateDir = old.VsockDir, old.DiskDir, old.RootFSPath, old.StateDir
	next.Confine = old.Confine
	if got := next.Adopt(context.Background()); len(got) != 1 || got[0] != idA {
		t.Fatalf("adopted %v", got)
	}
	// The VMM is asked about the API socket in the VM's own directory.
	if vm := fresh.Adopted[idA]; vm.APISocket != filepath.Join(before.RunDir, vmm.RunAPISocket) {
		t.Fatalf("adopted with %+v", vm)
	}
	h, _ := next.HandleOf(idA)
	if h.UID != before.UID || h.RunDir != before.RunDir {
		t.Fatalf("handle %+v, was %+v", h, before)
	}
	// The acceptors the new agent opened are the VMM's to connect to.
	for _, port := range []string{"_26501", "_26502"} {
		if got, ok := log.last(filepath.Join(before.RunDir, vmm.RunVsockSocket) + port); !ok || got != int(before.UID) {
			t.Errorf("acceptor %s given to %d (changed: %v), want %d", port, got, ok, before.UID)
		}
	}

	cp.setState(idA, "stopping")
	next.tick(context.Background())
	if exists(before.RunDir) {
		t.Fatal("the directory of an adopted VM stayed after its stop")
	}
	if got, ok := log.last(diskOf(next.DiskDir, idA)); !ok || got != 0 {
		t.Fatalf("the disk of an adopted VM was left to user %d (changed: %v)", got, ok)
	}
}

// Without Unprivileged nothing about a VM changes: no directory, no ownership.
func TestVMsThatRunAsRootAreAsBefore(t *testing.T) {
	cp := newFakeCP(t, idA)
	rec, _ := adoptAgent(t, cp, vmm.NewFakeVMM(nil), nil)
	log := &chownLog{}
	rec.Chown = log.chown
	rec.GuestHost = &recordingGuestHost{}
	rec.tick(context.Background())
	h, ok := rec.HandleOf(idA)
	if !ok || h.UID != 0 || h.RunDir != "" {
		t.Fatalf("handle %+v", h)
	}
	if len(log.calls) != 0 {
		t.Fatalf("ownership changed: %v", log.calls)
	}
	if !strings.HasPrefix(h.VsockPath, rec.VsockDir) || !strings.HasSuffix(h.VsockPath, "vsock-"+idA+".sock") {
		t.Fatalf("vsock %s", h.VsockPath)
	}
}

// The workspace's virtiofsd serves its socket in the VM's directory, and the
// VMM's user is given it: virtiofsd is root, the VMM is not.
func TestTheWorkspaceSocketIsInTheVMDirectoryAndGivenToItsUser(t *testing.T) {
	roots, host := workspaceUnder(t, "acme")
	fake := vmm.NewFakeVMM(nil)
	rec := newWorkspaceRec(t, fake, shortTempDir(t), cpclient.Sandbox{
		ID: idA, TenantID: "acme", State: "requested", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 128,
		WorkspaceHostPath: host,
	})
	rec.WorkspaceRoots = roots
	rec.Confine = &vmm.Confinement{Unprivileged: &vmm.Unprivileged{UIDBase: testUIDBase, RunDir: filepath.Join(shortTempDir(t), "vm")}}
	log := &chownLog{}
	rec.Chown = log.chown
	var sock string
	rec.FSLauncher = func(_ context.Context, _, _, s string) (func(), error) {
		sock = s
		return func() { _ = os.Remove(s) }, os.WriteFile(s, nil, 0o600)
	}
	rec.tick(context.Background())

	h, ok := rec.HandleOf(idA)
	if !ok {
		t.Fatal("the sandbox did not start")
	}
	if want := filepath.Join(h.RunDir, vmm.RunFSSocket); sock != want {
		t.Fatalf("virtiofsd serves %s, want %s", sock, want)
	}
	cfg, _ := fake.RunningConfig(idA)
	if cfg.WorkspaceFSSocket != sock {
		t.Fatalf("the VMM is given %q", cfg.WorkspaceFSSocket)
	}
	if got, ok := log.last(sock); !ok || got != int(h.UID) {
		t.Fatalf("the socket was given to %d (changed: %v), want %d", got, ok, h.UID)
	}
}

// The cleanup after an agent that died removes the directory of every VM that ran
// as a user of its own, and the processes that serve sockets in it, except the VMs
// that are adopted.
func TestReapRemovesTheDirectoriesOfVMsThatRanAsUsers(t *testing.T) {
	sockDir, runDir := shortTempDir(t), shortTempDir(t)
	procs := &fakeProcs{alive: map[int]bool{}, stubborn: map[int]bool{}, unkillable: map[int]bool{}}
	in := filepath.Join
	api := func(id string) string { return in(runDir, id, vmm.RunAPISocket) }
	for _, id := range []string{leftID, otherID} {
		if err := os.Mkdir(in(runDir, id), 0o700); err != nil {
			t.Fatal(err)
		}
		staleSocket(t, api(id))
		staleSocket(t, in(runDir, id, vmm.RunVsockSocket))
		writeFile(t, api(id)+vmm.APISocketLockSuffix, "")
	}
	// Look-alikes: a directory that is not a sandbox's, and a file that is.
	if err := os.Mkdir(in(runDir, "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, in(runDir, keptID), "")
	procs.add(201, "/usr/local/bin/cloud-hypervisor", "--api-socket", api(leftID), "--seccomp", "true")
	procs.add(202, "/usr/libexec/virtiofsd", "--socket-path", in(runDir, leftID, vmm.RunFSSocket), "--shared-dir", "/home/u/proj", "--sandbox", "chroot")
	procs.add(203, "cloud-hypervisor", "--api-socket", api(otherID)) // adopted: spared
	procs.add(204, "cloud-hypervisor", "--api-socket", in(runDir, "notes", vmm.RunAPISocket))
	procs.add(205, "cloud-hypervisor", "--api-socket", in(shortTempDir(t), leftID, vmm.RunAPISocket)) // another directory
	// A VMM that outlives the kill still uses its directory: it stays.
	const stuckID = "7c7c7c7c-0000-4000-8000-000000000007"
	if err := os.Mkdir(in(runDir, stuckID), 0o700); err != nil {
		t.Fatal(err)
	}
	procs.add(206, "cloud-hypervisor", "--api-socket", api(stuckID))
	procs.unkillable[206] = true

	rep := Reap(context.Background(), ReapConfig{SocketDir: sockDir, RunDir: runDir, Adopt: []string{otherID}, Procs: procs, Grace: 50 * time.Millisecond})
	// The one error is the VMM that cannot be killed.
	if rep.Err != nil && !strings.Contains(rep.Err.Error(), "pid 206 still running") {
		t.Fatal(rep.Err)
	}
	var got []string
	for _, p := range rep.Processes {
		got = append(got, fmt.Sprintf("%s %d %s", p.Kind, p.PID, p.SandboxID))
	}
	if want := []string{"cloud-hypervisor 201 " + leftID, "virtiofsd 202 " + leftID, "cloud-hypervisor 206 " + stuckID}; !slices.Equal(got, want) {
		t.Fatalf("processes %q, want %q", got, want)
	}
	if !slices.Equal(rep.RunDirs, []string{in(runDir, leftID)}) {
		t.Fatalf("directories %q", rep.RunDirs)
	}
	if exists(in(runDir, leftID)) {
		t.Fatal("the directory of the dead VM stayed")
	}
	for _, kept := range []string{in(runDir, otherID), in(runDir, "notes"), in(runDir, keptID), api(otherID), in(runDir, stuckID)} {
		if _, err := os.Lstat(kept); err != nil {
			t.Errorf("%s was removed: %v", kept, err)
		}
	}
	// The report mode removes nothing.
	if err := os.Mkdir(in(runDir, leftID), 0o700); err != nil {
		t.Fatal(err)
	}
	rep = Reap(context.Background(), ReapConfig{SocketDir: sockDir, RunDir: runDir, Adopt: []string{otherID, stuckID}, Report: true})
	if len(rep.RunDirs) != 1 || !exists(in(runDir, leftID)) {
		t.Fatalf("report: %q, directory kept: %v", rep.RunDirs, exists(in(runDir, leftID)))
	}
	// No directory at all (nothing ever ran as a user) is not an error.
	if rep := Reap(context.Background(), ReapConfig{SocketDir: sockDir, RunDir: in(runDir, "absent")}); rep.Err != nil {
		t.Fatal(rep.Err)
	}
}

// A disk that a dead VMM's user still owns is found, and a disk of an adopted VM, or
// one that is root's, or a file that is not a disk, is not.
func TestDisksOfAUserAreTheOnesRootDoesNotOwn(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("files made by root are root's")
	}
	dir := t.TempDir()
	for _, name := range []string{rootfsName(leftID), rootfsName(otherID), "rootfs.img", rootfsPrefix + leftID + ".qcow2"} {
		writeFile(t, filepath.Join(dir, name), "x")
	}
	got, err := disksOfAUser(dir, newSpareSet([]string{otherID}))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(dir, rootfsName(leftID))}; !slices.Equal(got, want) {
		t.Fatalf("disks %q, want %q", got, want)
	}
	// Listed, not touched, in report mode.
	rep := Reap(context.Background(), ReapConfig{SocketDir: shortTempDir(t), ReclaimDisks: dir, Report: true})
	if rep.Err != nil || len(rep.Reclaimed) != 2 {
		t.Fatalf("report %q err %v", rep.Reclaimed, rep.Err)
	}
}
