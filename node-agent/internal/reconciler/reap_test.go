package reconciler

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/cpclient"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/hostproc"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/hostvsock"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/localnet"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/tap"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/virtiofs"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

const (
	leftID  = "0a1b2c3d-1111-4222-8333-444455556666" // a sandbox of the previous agent
	otherID = "9f8e7d6c-aaaa-4bbb-8ccc-ddddeeeeffff"
	keptID  = "5e5e5e5e-0000-4000-8000-000000000001" // a Keep socket named like ours
)

// shortTempDir keeps socket paths under the 104-byte limit of macOS.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// staleSocket leaves a socket file nobody listens on, as a dead process does.
func staleSocket(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close()
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeProcs is a process table. A signal kills, except SIGTERM on a
// stubborn process and anything on an unkillable one (state D).
type fakeProcs struct {
	mu         sync.Mutex
	procs      []hostproc.Proc
	alive      map[int]bool
	stubborn   map[int]bool
	unkillable map[int]bool
	signals    []string
}

func (f *fakeProcs) add(pid int, argv ...string) {
	f.procs = append(f.procs, hostproc.Proc{PID: pid, Argv: argv, Start: uint64(pid) * 10})
	f.alive[pid] = true
}

func (f *fakeProcs) List() ([]hostproc.Proc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []hostproc.Proc
	for _, p := range f.procs {
		if f.alive[p.PID] {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeProcs) Signal(p hostproc.Proc, sig syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.alive[p.PID] {
		return hostproc.ErrGone
	}
	name := "TERM"
	if sig == syscall.SIGKILL {
		name = "KILL"
	}
	f.signals = append(f.signals, fmt.Sprintf("%s %d", name, p.PID))
	if !f.unkillable[p.PID] && (sig == syscall.SIGKILL || !f.stubborn[p.PID]) {
		f.alive[p.PID] = false
	}
	return nil
}

func (f *fakeProcs) Alive(p hostproc.Proc) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alive[p.PID]
}

// mockNetTools puts ip, wg and nft scripts that log their argv on PATH, as
// the localnet tests do. iptables stays missing, which localnet skips.
func mockNetTools(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv.log")
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"n=$0; n=${n##*/}\n" +
		"printf '%s' \"$n\" >> \"$ASP_MOCK_LOG\"\n" +
		"for a in \"$@\"; do printf ' %s' \"$a\" >> \"$ASP_MOCK_LOG\"; done\n" +
		"printf '\\n' >> \"$ASP_MOCK_LOG\"\n" +
		"exit 0\n"
	for _, name := range []string{"ip", "wg", "nft"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ASP_MOCK_LOG", logPath)
	t.Setenv("PATH", bin)
	return logPath
}

type reapFixture struct {
	cfg     ReapConfig
	procs   *fakeProcs
	taps    *tap.RecordingRunner
	argvLog string
	gone    []string // what the previous agent left: Reap removes it
	kept    []string // look-alikes: Reap leaves them
}

// newReapFixture is a host after an agent died running leftID with a
// workspace, local-net and a TAP, next to things that only look similar.
func newReapFixture(t *testing.T) *reapFixture {
	t.Helper()
	sockDir, diskDir, keyDir, sys := shortTempDir(t), t.TempDir(), t.TempDir(), t.TempDir()
	f := &reapFixture{
		procs:   &fakeProcs{alive: map[int]bool{}, stubborn: map[int]bool{}, unkillable: map[int]bool{}},
		taps:    &tap.RecordingRunner{},
		argvLog: mockNetTools(t),
	}
	in := filepath.Join

	for _, name := range []string{
		vmm.APISocketName(leftID),
		vsockName(leftID),
		vsockName(leftID) + "_26501",
		vsockName(leftID) + "_26502",
		virtiofsName(leftID),
	} {
		staleSocket(t, in(sockDir, name))
		f.gone = append(f.gone, in(sockDir, name))
	}
	link := in(sockDir, sshAgentName(leftID))
	if err := os.Symlink("/nonexistent/agent.sock", link); err != nil {
		t.Fatal(err)
	}
	disk := in(diskDir, rootfsName(leftID))
	key := in(keyDir, leftID+".key")
	writeFile(t, disk, "copy")
	writeFile(t, key, "private\n")
	f.gone = append(f.gone, link, disk, key)
	// Cloud Hypervisor and virtiofsd leave these next to their sockets.
	for _, name := range []string{
		vmm.APISocketName(leftID) + vmm.APISocketLockSuffix,
		virtiofsName(leftID) + virtiofs.PIDFileSuffix,
	} {
		writeFile(t, in(sockDir, name), "")
		f.gone = append(f.gone, in(sockDir, name))
	}

	for _, name := range []string{
		"ssh-agent.sock",          // --ssh-agent-bridge
		"identity.sock",           // --identity-listen
		"host-vsock-26501.sock",   // host-vsock unix fallback
		"ch-debug.sock",           // not a sandbox id
		vmm.APISocketName(keptID), // the shared --ch-api-socket (Keep)
		vsockName(leftID) + "_x",  // not a port
	} {
		staleSocket(t, in(sockDir, name))
		f.kept = append(f.kept, in(sockDir, name))
	}
	if err := os.Mkdir(in(sockDir, vmm.APISocketName(otherID)), 0o755); err != nil { // a directory, not a socket
		t.Fatal(err)
	}
	writeFile(t, in(sockDir, virtiofsName(otherID)), "") // a regular file, not a socket
	writeFile(t, in(sockDir, lockFileName), "1\n")
	for _, name := range []string{
		vmm.APISocketName(keptID) + vmm.APISocketLockSuffix, // the lock of the Keep socket
		"ch-debug.sock" + vmm.APISocketLockSuffix,           // not a sandbox id
		vsockName(leftID) + vmm.APISocketLockSuffix,         // nobody locks the muxer
		vmm.APISocketName(leftID) + virtiofs.PIDFileSuffix,  // CH writes no pid file
	} {
		writeFile(t, in(sockDir, name), "")
		f.kept = append(f.kept, in(sockDir, name))
	}
	// A directory, not the pid file virtiofsd writes.
	if err := os.Mkdir(in(sockDir, virtiofsName(otherID)+virtiofs.PIDFileSuffix), 0o755); err != nil {
		t.Fatal(err)
	}
	// The base image under a copy's name, kept through a symlinked Keep path.
	base := in(diskDir, rootfsName(otherID))
	writeFile(t, base, "base")
	baseLink := in(t.TempDir(), "rootfs.img")
	if err := os.Symlink(base, baseLink); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"rootfs.img", "rootfs-debug.img", rootfsPrefix + leftID + ".qcow2"} {
		writeFile(t, in(diskDir, name), "")
		f.kept = append(f.kept, in(diskDir, name))
	}
	writeFile(t, in(keyDir, "server.key"), "not a node key\n")
	f.kept = append(f.kept,
		in(sockDir, vmm.APISocketName(otherID)), in(sockDir, virtiofsName(otherID)), in(sockDir, lockFileName),
		in(sockDir, virtiofsName(otherID)+virtiofs.PIDFileSuffix), base, in(keyDir, "server.key"))

	for name, file := range map[string]string{
		"asp-0a1b2c3d":    "tun_flags",                // leftID's TAP
		"asp-demo0000":    "tun_flags",                // not a sandbox id
		"wg-asp-0a1b2c3d": "uevent:DEVTYPE=wireguard", // leftID's tunnel: it has a key
		"wg-asp-7777aaaa": "uevent:DEVTYPE=wireguard", // a tunnel whose key is gone
		"eth0":            "uevent:INTERFACE=eth0",
	} {
		file, content, _ := strings.Cut(file, ":")
		writeFile(t, in(sys, name, file), content+"\n")
	}

	sock := func(name string) string { return in(sockDir, name) }
	f.procs.add(101, "/usr/local/bin/cloud-hypervisor", "--api-socket", sock(vmm.APISocketName(leftID)))
	f.procs.add(102, "/usr/libexec/virtiofsd", "--socket-path", sock(virtiofsName(leftID)), "--shared-dir", "/home/u/proj", "--cache", "never", "--sandbox", "none")
	f.procs.add(103, "cloud-hypervisor", "--api-socket", "/run/other-agent/"+vmm.APISocketName(otherID))
	f.procs.add(104, "cloud-hypervisor", "--api-socket", sock(vmm.APISocketName(keptID)))
	f.procs.add(105, "cloud-hypervisor", "--api-socket", sock("ch-debug.sock"))
	f.procs.add(106, "/usr/sbin/sshd", "-D")
	f.procs.add(os.Getpid(), "node-agent", "--api-socket", sock(vmm.APISocketName(leftID))) // never itself

	f.cfg = ReapConfig{
		SocketDir:   sockDir,
		DiskDir:     diskDir,
		Keep:        []string{baseLink, sock(vmm.APISocketName(keptID))},
		Procs:       f.procs,
		SysClassNet: sys,
		Tap:         &tap.Manager{Runner: f.taps},
		LocalNet:    localnet.NewHost(keyDir),
		Grace:       50 * time.Millisecond,
	}
	return f
}

func (f *reapFixture) assertKept(t *testing.T, paths []string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
}

func (f *reapFixture) assertGone(t *testing.T) {
	t.Helper()
	for _, p := range f.gone {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("leftover %s is still there (%v)", p, err)
		}
	}
}

func TestReapRemovesWhatAPreviousAgentLeft(t *testing.T) {
	f := newReapFixture(t)
	rep := Reap(context.Background(), f.cfg)
	if rep.Err != nil {
		t.Fatal(rep.Err)
	}

	var procs []string
	for _, p := range rep.Processes {
		procs = append(procs, fmt.Sprintf("%s %d %s", p.Kind, p.PID, p.SandboxID))
	}
	if want := []string{"cloud-hypervisor 101 " + leftID, "virtiofsd 102 " + leftID}; !slices.Equal(procs, want) {
		t.Fatalf("processes=%q want %q", procs, want)
	}
	if want := []string{"TERM 101", "TERM 102"}; !slices.Equal(f.procs.signals, want) {
		t.Fatalf("signals=%q want %q", f.procs.signals, want)
	}

	if want := []string{leftID, "7777aaaa"}; !slices.Equal(rep.LocalNet, want) {
		t.Fatalf("local-net=%q want %q", rep.LocalNet, want)
	}
	log := string(mustRead(t, f.argvLog))
	table := strconv.Itoa(localnet.TableID(leftID))
	for _, want := range []string{
		"ip link delete dev wg-asp-0a1b2c3d",
		"ip rule del iif asp-0a1b2c3d lookup " + table + " priority " + table,
		"ip route flush table " + table,
		"ip link delete dev wg-asp-7777aaaa",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("missing %q\n%s", want, log)
		}
	}

	if want := []string{"ip link delete asp-0a1b2c3d"}; !slices.Equal(f.taps.Calls, want) {
		t.Fatalf("tap calls=%q want %q", f.taps.Calls, want)
	}
	if len(rep.Sockets) != 8 || len(rep.Disks) != 1 {
		t.Fatalf("sockets=%q disks=%q", rep.Sockets, rep.Disks)
	}
	f.assertGone(t)
	f.assertKept(t, f.kept)
}

// --reap-leftovers=report and --dry-run: the same findings, nothing touched.
func TestReapReportOnlyTouchesNothing(t *testing.T) {
	f := newReapFixture(t)
	f.cfg.Report = true
	rep := Reap(context.Background(), f.cfg)
	if rep.Err != nil {
		t.Fatal(rep.Err)
	}
	if len(rep.Processes) != 2 || len(rep.LocalNet) != 2 || len(rep.Taps) != 1 || len(rep.Sockets) != 8 || len(rep.Disks) != 1 {
		t.Fatalf("report=%+v", rep)
	}
	if len(f.procs.signals) != 0 || len(f.taps.Calls) != 0 {
		t.Fatalf("report mode signalled %q and ran %q", f.procs.signals, f.taps.Calls)
	}
	if _, err := os.Stat(f.argvLog); !os.IsNotExist(err) {
		t.Fatalf("report mode ran ip/wg/nft:\n%s", mustRead(t, f.argvLog))
	}
	f.assertKept(t, f.gone)
	f.assertKept(t, f.kept)
}

// A VM that ignores SIGTERM gets SIGKILL, and a TAP that cannot be deleted
// does not stop the rest of the cleanup.
func TestReapKeepsGoingAfterAFailure(t *testing.T) {
	f := newReapFixture(t)
	f.procs.stubborn[101] = true
	f.taps.InjectedErr = errors.New("RTNETLINK answers: Operation not permitted")
	rep := Reap(context.Background(), f.cfg)
	if rep.Err == nil || !strings.Contains(rep.Err.Error(), "delete TAP asp-0a1b2c3d") {
		t.Fatalf("err=%v", rep.Err)
	}
	if want := []string{"TERM 101", "TERM 102", "KILL 101"}; !slices.Equal(f.procs.signals, want) {
		t.Fatalf("signals=%q want %q", f.procs.signals, want)
	}
	f.assertGone(t)
}

// A VM that outlives SIGKILL still holds its lock: the lock stays for the next
// cleanup, while the virtiofsd that did exit loses its pid file.
func TestReapKeepsTheLockOfAVMThatSurvives(t *testing.T) {
	f := newReapFixture(t)
	f.procs.unkillable[101] = true
	// Do not wait the full SIGKILL grace for a process that never exits.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	rep := Reap(ctx, f.cfg)
	if rep.Err == nil || !strings.Contains(rep.Err.Error(), "pid 101 still running") {
		t.Fatalf("err=%v", rep.Err)
	}
	lock := filepath.Join(f.cfg.SocketDir, vmm.APISocketName(leftID)+vmm.APISocketLockSuffix)
	if slices.Contains(rep.Sockets, lock) {
		t.Fatalf("listed the lock of a running VM: %q", rep.Sockets)
	}
	f.assertKept(t, []string{lock})
	pid := filepath.Join(f.cfg.SocketDir, virtiofsName(leftID)+virtiofs.PIDFileSuffix)
	if _, err := os.Lstat(pid); !os.IsNotExist(err) {
		t.Fatalf("pid file of an exited virtiofsd is still there (%v)", err)
	}
}

// Whatever a start leaves on the host, the reaper finds: the agent dies
// without stopping the sandbox, and the next process cleans up.
func TestReapRemovesWhatEnsureRunningCreates(t *testing.T) {
	sockDir, diskDir := shortTempDir(t), t.TempDir()
	base := filepath.Join(t.TempDir(), "rootfs.img")
	writeFile(t, base, "base")
	fake := vmm.NewFakeVMM(nil)
	wr := newWorkspaceRec(t, fake, sockDir, cpclient.Sandbox{
		ID: leftID, State: "requested", ImageRef: "img", CPUMillis: 1000, MemoryMiB: 128,
		WorkspaceHostPath: t.TempDir(),
	})
	wr.RootFSPath = base
	wr.DiskDir = diskDir
	wr.CloneDisk = func(_, dst string) error { return os.WriteFile(dst, []byte("copy"), 0o600) }
	wr.SSHAgentShared = "/nonexistent/agent.sock"
	wr.FSLauncher = func(_ context.Context, _, _, sock string) (func(), error) {
		staleSocket(t, sock)
		writeFile(t, sock+virtiofs.PIDFileSuffix, "")
		return func() {}, nil
	}
	wr.tick(context.Background())
	h, ok := wr.HandleOf(leftID)
	if !ok {
		t.Fatal("the sandbox did not start")
	}
	cfg, _ := fake.RunningConfig(leftID)

	// Cloud Hypervisor binds its API socket and vsock muxer, and hostvsock
	// the hybrid listeners next to it; FakeVMM does neither.
	left := []string{
		filepath.Join(sockDir, vmm.APISocketName(leftID)),
		h.VsockPath,
		hostvsock.HybridGuestPath(h.VsockPath, hostvsock.PortSSHAgent),
		hostvsock.HybridGuestPath(h.VsockPath, hostvsock.PortIdentity),
	}
	for _, p := range left {
		staleSocket(t, p)
	}
	chLock := left[0] + vmm.APISocketLockSuffix
	writeFile(t, chLock, "")
	left = append(left, chLock, cfg.WorkspaceFSSocket, cfg.WorkspaceFSSocket+virtiofs.PIDFileSuffix, h.SSHSock, h.RootFS)

	rep := Reap(context.Background(), ReapConfig{SocketDir: sockDir, DiskDir: diskDir, Keep: []string{base}})
	if rep.Err != nil {
		t.Fatal(rep.Err)
	}
	for _, p := range left {
		if p == "" {
			t.Fatalf("the start left an empty path: %q", left)
		}
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived the reaper", p)
		}
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("base image: %v", err)
	}
}

func TestSandboxIDShapes(t *testing.T) {
	for s, want := range map[string]bool{
		leftID:                                 true,
		strings.ToUpper(leftID):                false,
		"0a1b2c3d111142228333444455556666":     false,
		"0a1b2c3d-1111-4222-8333-44445555666":  false,
		"0a1b2c3d-1111-4222-8333-44445555666g": false,
		"":                                     false,
	} {
		if got := isSandboxID(s); got != want {
			t.Errorf("isSandboxID(%q) = %v", s, got)
		}
	}
	for s, want := range map[string]bool{"0a1b2c3d": true, "demo0000": false, "0a1b2c3": false, "0A1B2C3D": false} {
		if got := isShortID(s); got != want {
			t.Errorf("isShortID(%q) = %v", s, got)
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A session from this agent version has an allocation file: Reap clears its
// allocated table (not the hashed one) and releases the allocation.
func TestReapReleasesLocalNetAllocations(t *testing.T) {
	argvLog := mockNetTools(t)
	keyDir := t.TempDir()
	id := "0b0b0b0b-1111-2222-3333-444444444444"
	writeFile(t, filepath.Join(keyDir, id+".key"), "private\n")
	writeFile(t, filepath.Join(keyDir, id+".alloc"), `{"table_id":12345,"listen_port":50001,"slot":9}`)
	cfg := ReapConfig{SocketDir: shortTempDir(t), DiskDir: t.TempDir(), Procs: &fakeProcs{alive: map[int]bool{}, stubborn: map[int]bool{}},
		SysClassNet: t.TempDir(), Tap: &tap.Manager{Runner: &tap.RecordingRunner{}}, LocalNet: localnet.NewHost(keyDir), Grace: 10 * time.Millisecond}
	rep := Reap(context.Background(), cfg)
	if rep.Err != nil {
		t.Fatal(rep.Err)
	}
	if !slices.Equal(rep.LocalNet, []string{id}) {
		t.Fatalf("local-net=%q", rep.LocalNet)
	}
	log := string(mustRead(t, argvLog))
	if !strings.Contains(log, "ip route flush table 12345") {
		t.Fatalf("want the allocated table cleared:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(keyDir, id+".alloc")); !os.IsNotExist(err) {
		t.Fatalf("allocation file still there: %v", err)
	}
}
