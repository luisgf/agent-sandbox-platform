package hostproc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// writeProc adds a process to a fake /proc tree.
func writeProc(t *testing.T, root string, pid int, comm string, state byte, start uint64, argv ...string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var cmdline strings.Builder
	for _, a := range argv {
		cmdline.WriteString(a + "\x00")
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	stat := fmt.Sprintf("%d (%s) %c 1 %d %d 0 -1 4194560 120 0 0 0 3 1 0 0 20 0 4 0 %d 1048576 256 18446744073709551615\n",
		pid, comm, state, pid, pid, start)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProcFSListReadsArgvAndStartTime(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, 1, "systemd", 'S', 2, "/sbin/init")
	// The name may hold spaces and ')': fields are counted after the last one.
	writeProc(t, root, 77, "cloud hyper)visor", 'S', 900, "/usr/bin/cloud-hypervisor", "--api-socket", "/run/asp/ch-x.sock")
	writeProc(t, root, 2, "kthreadd", 'S', 3)    // kernel thread: no command line
	for _, dir := range []string{"self", "88"} { // not a PID; a process that exited mid-walk
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	procs, err := ProcFS{Root: root}.List()
	if err != nil {
		t.Fatal(err)
	}
	byPID := map[int]Proc{}
	for _, p := range procs {
		byPID[p.PID] = p
	}
	if len(byPID) != 2 {
		t.Fatalf("want pids 1 and 77, got %+v", procs)
	}
	ch := byPID[77]
	if want := []string{"/usr/bin/cloud-hypervisor", "--api-socket", "/run/asp/ch-x.sock"}; !slices.Equal(ch.Argv, want) {
		t.Fatalf("argv=%q want %q", ch.Argv, want)
	}
	if ch.Start != 900 {
		t.Fatalf("start=%d want 900", ch.Start)
	}
}

func TestProcFSAliveSeesExitReuseAndZombies(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, 10, "cloud-hypervisor", 'S', 500, "cloud-hypervisor")
	writeProc(t, root, 11, "virtiofsd", 'Z', 600, "virtiofsd")
	fs := ProcFS{Root: root}
	if !fs.Alive(Proc{PID: 10, Start: 500}) {
		t.Fatal("a running process is not alive")
	}
	if fs.Alive(Proc{PID: 10, Start: 499}) {
		t.Fatal("a reused PID counts as the old process")
	}
	if fs.Alive(Proc{PID: 11, Start: 600}) {
		t.Fatal("a zombie counts as alive")
	}
	if fs.Alive(Proc{PID: 12, Start: 1}) {
		t.Fatal("a missing PID counts as alive")
	}
}

func TestFlagValue(t *testing.T) {
	argv := []string{"cloud-hypervisor", "--api-socket", "/run/asp/ch-a.sock", "-v", "--seccomp=true"}
	if v, ok := FlagValue(argv, "--api-socket"); !ok || v != "/run/asp/ch-a.sock" {
		t.Fatalf("--api-socket = %q %v", v, ok)
	}
	if v, ok := FlagValue(argv, "--seccomp"); !ok || v != "true" {
		t.Fatalf("--seccomp = %q %v", v, ok)
	}
	for _, miss := range [][]string{
		argv[:1],                             // absent
		{"cloud-hypervisor", "--api-socket"}, // no value
		{"--api-socket", "x"},                // argv[0] is the program
	} {
		if v, ok := FlagValue(miss, "--api-socket"); ok {
			t.Fatalf("FlagValue(%q) = %q, want none", miss, v)
		}
	}
}

// fakeTable is a process table whose processes react to signals as told.
type fakeTable struct {
	mu       sync.Mutex
	alive    map[int]bool
	stubborn map[int]bool // ignores SIGTERM
	immortal map[int]bool // survives SIGKILL too (uninterruptible sleep)
	denied   map[int]bool // EPERM
	sent     []string
}

func (f *fakeTable) List() ([]Proc, error) { return nil, nil }

func (f *fakeTable) Signal(p Proc, sig syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.denied[p.PID] {
		return syscall.EPERM
	}
	if !f.alive[p.PID] {
		return ErrGone
	}
	name := "TERM"
	if sig == syscall.SIGKILL {
		name = "KILL"
	}
	f.sent = append(f.sent, fmt.Sprintf("%s %d", name, p.PID))
	if !f.immortal[p.PID] && (sig == syscall.SIGKILL || !f.stubborn[p.PID]) {
		f.alive[p.PID] = false
	}
	return nil
}

func (f *fakeTable) Alive(p Proc) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alive[p.PID]
}

func TestTerminateEscalatesToSIGKILL(t *testing.T) {
	f := &fakeTable{
		alive:    map[int]bool{1: true, 2: true, 4: true},
		stubborn: map[int]bool{2: true},
		denied:   map[int]bool{4: true},
	}
	procs := []Proc{{PID: 1}, {PID: 2}, {PID: 3}, {PID: 4}} // 3 already exited
	err := Terminate(context.Background(), f, procs, 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "signal pid 4") || !errors.Is(err, syscall.EPERM) {
		t.Fatalf("err=%v, want the EPERM on pid 4", err)
	}
	if want := []string{"TERM 1", "TERM 2", "KILL 2"}; !slices.Equal(f.sent, want) {
		t.Fatalf("signals=%q want %q", f.sent, want)
	}
}

func TestTerminateReportsSurvivors(t *testing.T) {
	f := &fakeTable{alive: map[int]bool{5: true}, immortal: map[int]bool{5: true}}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := Terminate(ctx, f, []Proc{{PID: 5}}, 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "pid 5 still running") {
		t.Fatalf("err=%v", err)
	}
	if want := []string{"TERM 5", "KILL 5"}; !slices.Equal(f.sent, want) {
		t.Fatalf("signals=%q want %q", f.sent, want)
	}
}

// The real /proc: a child that dies on SIGTERM, one that needs SIGKILL, and a
// stale start time that must not be signalled. Terminate returns before the
// test waits for the child, so the zombie must count as gone.
func TestProcFSTerminatesRealProcesses(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs a Linux /proc")
	}
	for _, tc := range []struct {
		name, script string
		want         syscall.Signal
	}{
		{"exits on SIGTERM", "while :; do sleep 1; done", syscall.SIGTERM},
		{"ignores SIGTERM", `trap "" TERM; while :; do sleep 1; done`, syscall.SIGKILL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sh", "-c", tc.script)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			t.Cleanup(func() {
				if !waited {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			fs := ProcFS{}
			// Start returns after fork; until the child execs sh, /proc shows an
			// empty or inherited command line. Wait for its own argv.
			var child Proc
			for deadline := time.Now().Add(2 * time.Second); ; {
				procs, err := fs.List()
				if err != nil {
					t.Fatal(err)
				}
				child = Proc{}
				for _, p := range procs {
					if p.PID == cmd.Process.Pid {
						child = p
					}
				}
				if child.PID != 0 && slices.Contains(child.Argv, tc.script) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("child %d not listed with its argv: %+v", cmd.Process.Pid, child)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := fs.Signal(Proc{PID: child.PID, Start: child.Start + 1}, syscall.SIGTERM); !errors.Is(err, ErrGone) {
				t.Fatalf("signal with a stale start time = %v, want ErrGone", err)
			}
			if !fs.Alive(child) {
				t.Fatal("the child died from a signal meant for another process")
			}

			if err := Terminate(context.Background(), fs, []Proc{child}, 300*time.Millisecond); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			waited = true
			ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !ws.Signaled() || ws.Signal() != tc.want {
				t.Fatalf("child ended with %v, want %v", cmd.ProcessState, tc.want)
			}
		})
	}
}
