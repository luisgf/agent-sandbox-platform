// Package doctor checks that a host can run sandboxes, and says what is wrong when
// it cannot. Each check reads one thing about the machine and its configuration
// and answers ok, warn, fail or skip with what it found and how to fix it, so that
// a missing /dev/kvm, an nft table that was never applied or a full disk shows up as
// a line of a report, not as a start error three layers down (or, worse, as a
// guest that quietly runs without the egress rules).
//
// Checks read the host through Host, so each is tested with a fake one.
package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Status is how a check went.
type Status string

const (
	OK   Status = "ok"
	Warn Status = "warn" // works, but not as it should (or for some sandboxes only)
	Fail Status = "fail" // sandboxes will not start, or will not be confined
	Skip Status = "skip" // does not apply to this node's configuration
)

// Result is the answer of one check.
type Result struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Fix says what to do about a warn or a fail.
	Fix string `json:"fix,omitempty"`
}

// Report is the answer of all checks.
type Report struct {
	NodeID  string    `json:"node_id,omitempty"`
	At      time.Time `json:"at"`
	Results []Result  `json:"results"`
}

// Failed reports whether any check failed.
func (r Report) Failed() bool {
	for _, c := range r.Results {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

// Counts is how many checks ended in each status.
func (r Report) Counts() map[Status]int {
	m := map[Status]int{}
	for _, c := range r.Results {
		m[c.Status]++
	}
	return m
}

// Text renders the report for a terminal: one line per check, the fix under it.
func (r Report) Text() string {
	var b strings.Builder
	title := "node doctor"
	if r.NodeID != "" {
		title += " (" + r.NodeID + ")"
	}
	fmt.Fprintln(&b, title)
	width := 0
	for _, c := range r.Results {
		width = max(width, len(c.Name))
	}
	for _, c := range r.Results {
		fmt.Fprintf(&b, "  %-4s  %-*s  %s\n", c.Status, width, c.Name, c.Detail)
		if c.Fix != "" && c.Status != OK {
			fmt.Fprintf(&b, "        %-*s  fix: %s\n", width, "", c.Fix)
		}
	}
	n := r.Counts()
	fmt.Fprintf(&b, "%d ok, %d warn, %d fail, %d skipped\n", n[OK], n[Warn], n[Fail], n[Skip])
	return b.String()
}

// Check is one thing to look at.
type Check struct {
	Name string
	Run  func(ctx context.Context) Result
}

// Run runs the checks in order, each with a timeout of its own, and recovers a
// check that panics: a doctor that crashes on the host that is broken is no use.
func Run(ctx context.Context, nodeID string, checks []Check, timeout time.Duration) Report {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	rep := Report{NodeID: nodeID, At: time.Now().UTC()}
	for _, c := range checks {
		rep.Results = append(rep.Results, runOne(ctx, c, timeout))
	}
	return rep
}

func runOne(ctx context.Context, c Check, timeout time.Duration) (res Result) {
	defer func() {
		if v := recover(); v != nil {
			res = Result{Name: c.Name, Status: Fail, Detail: fmt.Sprintf("the check crashed: %v", v)}
		}
	}()
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res = c.Run(cctx)
	res.Name = c.Name
	if res.Status == "" {
		res.Status = Fail
		res.Detail = "the check gave no answer"
	}
	if cctx.Err() == context.DeadlineExceeded && res.Status == OK {
		res.Status, res.Detail = Warn, "took longer than "+timeout.String()+": "+res.Detail
	}
	return res
}

// Host is what the checks do to the machine.
type Host struct {
	GOOS string
	Euid int
	// Stat, ReadFile and LookPath are os.Stat, os.ReadFile and exec.LookPath.
	Stat     func(path string) (os.FileInfo, error)
	ReadFile func(path string) ([]byte, error)
	LookPath func(name string) (string, error)
	// OpenRW opens path for reading and writing and closes it.
	OpenRW func(path string) error
	// Run runs a command and returns its output (stdout and stderr together).
	Run func(ctx context.Context, name string, args ...string) (string, error)
	// DiskFreeMiB is the free space of dir's filesystem.
	DiskFreeMiB func(dir string) (int64, bool)
	// SHA256 is the digest of a file, "sha256:<hex>".
	SHA256 func(path string) (string, error)
	// Writable checks that a file can be created in dir.
	Writable func(dir string) error
}

// RealHost is the machine this process runs on. diskFree and sha256 come from the
// caller (they live next to the code that already does the same for the agent).
func RealHost(diskFree func(string) (int64, bool), sha256 func(string) (string, error)) Host {
	return Host{
		GOOS: goos(), Euid: os.Geteuid(),
		Stat: os.Stat, ReadFile: os.ReadFile, LookPath: exec.LookPath,
		OpenRW: func(path string) error {
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				return err
			}
			return f.Close()
		},
		Run: func(ctx context.Context, name string, args ...string) (string, error) {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			return strings.TrimSpace(string(out)), err
		},
		DiskFreeMiB: diskFree, SHA256: sha256,
		Writable: func(dir string) error {
			f, err := os.CreateTemp(dir, ".doctor-*")
			if err != nil {
				return err
			}
			name := f.Name()
			_ = f.Close()
			return os.Remove(name)
		},
	}
}
