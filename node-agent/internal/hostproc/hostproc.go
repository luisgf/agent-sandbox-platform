// Package hostproc lists host processes from /proc and stops them. The
// node-agent reaper uses it to find the cloud-hypervisor and virtiofsd
// processes a previous agent process left running.
package hostproc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Proc is one process of the host process table.
type Proc struct {
	PID  int
	Argv []string
	// Start is the start time from /proc/PID/stat (clock ticks after boot).
	// A PID reused by another process has another start time.
	Start uint64
}

// Table lists host processes and signals them. ProcFS is the Linux table;
// tests use fakes.
type Table interface {
	List() ([]Proc, error)
	// Signal delivers sig to p. It returns ErrGone when p has exited or its
	// PID now belongs to another process.
	Signal(p Proc, sig syscall.Signal) error
	// Alive reports whether p still runs. A zombie has exited.
	Alive(p Proc) bool
}

// ErrGone means the process exited (or its PID was reused).
var ErrGone = errors.New("process is gone")

// ProcFS reads a Linux /proc. Root "" means /proc.
type ProcFS struct {
	Root string
}

func (fs ProcFS) root() string {
	if fs.Root == "" {
		return "/proc"
	}
	return fs.Root
}

// List returns every process that has a command line. Kernel threads (empty
// cmdline) and processes that exit during the walk are left out.
func (fs ProcFS) List() ([]Proc, error) {
	entries, err := os.ReadDir(fs.root())
	if err != nil {
		return nil, err
	}
	var out []Proc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(fs.root(), e.Name(), "cmdline"))
		if err != nil || len(raw) == 0 {
			continue
		}
		st, err := fs.stat(pid)
		if err != nil {
			continue
		}
		out = append(out, Proc{PID: pid, Argv: splitCmdline(raw), Start: st.start})
	}
	return out, nil
}

// Alive reports whether p runs: same PID, same start time, not a zombie.
func (fs ProcFS) Alive(p Proc) bool {
	st, err := fs.stat(p.PID)
	return err == nil && st.start == p.Start && st.state != 'Z' && st.state != 'X'
}

// Signal pins the process before it checks its start time: on Linux
// os.FindProcess holds a pidfd, so the signal cannot reach a process that
// reused the PID after the check.
func (fs ProcFS) Signal(p Proc, sig syscall.Signal) error {
	proc, err := os.FindProcess(p.PID)
	if err != nil {
		return ErrGone
	}
	defer proc.Release()
	if !fs.Alive(p) {
		return ErrGone
	}
	if err := proc.Signal(sig); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return ErrGone
		}
		return err
	}
	return nil
}

type procStat struct {
	state byte
	start uint64
}

// stat parses /proc/PID/stat. The command name is in parentheses and may
// itself hold spaces and parentheses, so fields are counted after the last ')'.
func (fs ProcFS) stat(pid int) (procStat, error) {
	raw, err := os.ReadFile(filepath.Join(fs.root(), strconv.Itoa(pid), "stat"))
	if err != nil {
		return procStat{}, err
	}
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 {
		return procStat{}, fmt.Errorf("pid %d: malformed stat", pid)
	}
	// After the name come field 3 (state) onwards; starttime is field 22.
	f := strings.Fields(string(raw[i+1:]))
	if len(f) < 20 || f[0] == "" {
		return procStat{}, fmt.Errorf("pid %d: short stat", pid)
	}
	start, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return procStat{}, fmt.Errorf("pid %d: starttime: %w", pid, err)
	}
	return procStat{state: f[0][0], start: start}, nil
}

func splitCmdline(raw []byte) []string {
	parts := bytes.Split(bytes.TrimRight(raw, "\x00"), []byte{0})
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = string(p)
	}
	return out
}

// FlagValue returns the value argv gives a long option, as "--name value" or
// "--name=value". argv[0] is the program and is not looked at.
func FlagValue(argv []string, name string) (string, bool) {
	for i := 1; i < len(argv); i++ {
		if argv[i] == name && i+1 < len(argv) {
			return argv[i+1], true
		}
		if v, ok := strings.CutPrefix(argv[i], name+"="); ok {
			return v, true
		}
	}
	return "", false
}

const (
	// killWait bounds the wait after SIGKILL (a process in uninterruptible
	// sleep only dies when it leaves it).
	killWait  = 5 * time.Second
	pollEvery = 50 * time.Millisecond
)

// Terminate stops procs: SIGTERM, up to grace for them to exit, then SIGKILL
// for the rest. The error names every process it could not signal or that is
// still running.
func Terminate(ctx context.Context, t Table, procs []Proc, grace time.Duration) error {
	var errs []error
	live := signalAll(t, procs, syscall.SIGTERM, &errs)
	live = waitExit(ctx, t, live, grace)
	if len(live) > 0 {
		live = signalAll(t, live, syscall.SIGKILL, &errs)
		live = waitExit(ctx, t, live, killWait)
	}
	for _, p := range live {
		errs = append(errs, fmt.Errorf("pid %d still running after SIGKILL", p.PID))
	}
	return errors.Join(errs...)
}

// signalAll returns the processes that got sig. Gone ones are dropped; the
// ones it cannot signal are reported and dropped too.
func signalAll(t Table, procs []Proc, sig syscall.Signal, errs *[]error) []Proc {
	var sent []Proc
	for _, p := range procs {
		switch err := t.Signal(p, sig); {
		case err == nil:
			sent = append(sent, p)
		case errors.Is(err, ErrGone):
		default:
			*errs = append(*errs, fmt.Errorf("signal pid %d: %w", p.PID, err))
		}
	}
	return sent
}

// waitExit polls until every process has exited or d passes, and returns the
// ones still alive.
func waitExit(ctx context.Context, t Table, procs []Proc, d time.Duration) []Proc {
	deadline := time.Now().Add(d)
	for {
		var live []Proc
		for _, p := range procs {
			if t.Alive(p) {
				live = append(live, p)
			}
		}
		if len(live) == 0 || !time.Now().Before(deadline) {
			return live
		}
		procs = live
		select {
		case <-ctx.Done():
			return live
		case <-time.After(pollEvery):
		}
	}
}
