package unit

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestArgs(t *testing.T) {
	spec := Spec{
		Name: "asp-vm-abc", Slice: "asp-vms.slice", Description: "ASP microVM abc",
		MemoryMax: 1 << 30, CPUQuota: 250, TasksMax: 512,
		Properties: []string{"Nice=5"},
	}
	got := spec.Args("/usr/bin/vmm", "--api-socket", "/run/x.sock")
	want := []string{
		"--quiet", "--collect", "--wait", "--unit=asp-vm-abc", "--slice=asp-vms.slice",
		"--description=ASP microVM abc",
		"--property=MemoryMax=1073741824", "--property=CPUQuota=250%", "--property=TasksMax=512",
		"--property=Nice=5",
		"--", "/usr/bin/vmm", "--api-socket", "/run/x.sock",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args:\n got %q\nwant %q", got, want)
	}
	// Limits at zero set nothing, and the command is last.
	bare := Spec{Name: "u"}.Args("c", "a")
	if strings.Contains(strings.Join(bare, " "), "--property") || bare[len(bare)-2] != "c" || bare[len(bare)-1] != "a" {
		t.Fatalf("bare args: %q", bare)
	}
}

// fakeTools writes a systemd-run and a systemctl that run on any machine:
// systemd-run records its arguments and runs the command after "--"; systemctl
// records its arguments and prints what a test file says.
func fakeTools(t *testing.T) (l Launcher, dir string) {
	t.Helper()
	dir = t.TempDir()
	run := filepath.Join(dir, "systemd-run")
	ctl := filepath.Join(dir, "systemctl")
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(run, `#!/bin/sh
echo "$@" >> "`+dir+`/run.log"
while [ "$1" != "--" ]; do shift; done
shift
exec "$@"
`)
	write(ctl, `#!/bin/sh
echo "$@" >> "`+dir+`/ctl.log"
case "$1" in
  show) cat "`+dir+`/mainpid" 2>/dev/null || echo 0 ;;
  kill) if [ -f "`+dir+`/gone" ]; then echo "Failed to kill unit: Unit asp-vm-x.service not loaded." >&2; exit 5; fi
        if [ -f "`+dir+`/broken" ]; then echo "Access denied" >&2; exit 1; fi
        [ -f "`+dir+`/child.pid" ] && kill -9 "$(cat "`+dir+`/child.pid")" ;;
esac
`)
	return Launcher{SystemdRun: run, Systemctl: ctl}, dir
}

func TestStartRunsTheCommandAndWaits(t *testing.T) {
	l, dir := fakeTools(t)
	p, err := l.Start(Spec{Name: "asp-vm-x", MemoryMax: 1 << 20}, "/bin/sh", "-c", "exit 0")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "run.log"))
	if !strings.Contains(string(log), "--unit=asp-vm-x") || !strings.Contains(string(log), "-- /bin/sh -c exit 0") {
		t.Fatalf("systemd-run saw %q", log)
	}
	if p.Unit() != "asp-vm-x.service" {
		t.Fatalf("unit %q", p.Unit())
	}
}

func TestWaitReportsAFailureWithWhatSystemdRunSaid(t *testing.T) {
	l, _ := fakeTools(t)
	p, err := l.Start(Spec{Name: "u"}, "/bin/sh", "-c", "echo 'Unit u.service already exists.' >&2; exit 1")
	if err != nil {
		t.Fatal(err)
	}
	err = p.Wait()
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Wait: %v", err)
	}
}

func TestKillStopsTheUnitAndToleratesAGoneOne(t *testing.T) {
	l, dir := fakeTools(t)
	p, err := l.Start(Spec{Name: "asp-vm-x"}, "/bin/sh", "-c", "echo $$ > "+dir+"/child.pid; exec sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "child.pid")); return err == nil })
	done := make(chan error, 1)
	go func() { done <- p.Wait() }()
	if err := p.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after Kill")
	}
	ctl, _ := os.ReadFile(filepath.Join(dir, "ctl.log"))
	if !strings.Contains(string(ctl), "kill --signal=SIGKILL --kill-whom=all asp-vm-x.service") {
		t.Fatalf("systemctl saw %q", ctl)
	}

	// A unit that is already gone is not an error; any other failure is.
	if err := os.WriteFile(filepath.Join(dir, "gone"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("Kill of a gone unit: %v", err)
	}
	_ = os.Remove(filepath.Join(dir, "gone"))
	if err := os.WriteFile(filepath.Join(dir, "broken"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Kill(); err == nil || !strings.Contains(err.Error(), "Access denied") {
		t.Fatalf("Kill with systemctl failing: %v", err)
	}
}

func TestPid(t *testing.T) {
	l, dir := fakeTools(t)
	p := &Process{l: l, unit: "asp-vm-x.service"}
	if pid := p.Pid(); pid != 0 {
		t.Fatalf("Pid with no main process = %d", pid)
	}
	if err := os.WriteFile(filepath.Join(dir, "mainpid"), []byte("4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pid := p.Pid(); pid != 4242 {
		t.Fatalf("Pid = %d", pid)
	}
	if err := os.WriteFile(filepath.Join(dir, "mainpid"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pid := p.Pid(); pid != 0 {
		t.Fatalf("Pid of garbage = %d", pid)
	}
}

func TestStartNeedsAName(t *testing.T) {
	if _, err := (Launcher{}).Start(Spec{}, "true"); err == nil {
		t.Fatal("a unit without a name started")
	}
	if _, err := (Launcher{SystemdRun: "/nonexistent/systemd-run"}).Start(Spec{Name: "u"}, "true"); err == nil {
		t.Fatal("a missing systemd-run started")
	}
}

func TestGone(t *testing.T) {
	for out, want := range map[string]bool{
		"Failed to kill unit: Unit asp-vm-x.service not loaded.": true,
		"Unit asp-vm-x.service not found.":                       true,
		"No such unit":                                           true,
		"Access denied":                                          false,
		"":                                                       false,
	} {
		if gone(out) != want {
			t.Errorf("gone(%q) = %v", out, !want)
		}
	}
}

func TestCommandIsInjectable(t *testing.T) {
	var saw []string
	l := Launcher{Command: func(name string, args ...string) *exec.Cmd {
		saw = append([]string{name}, args...)
		return exec.Command("true")
	}}
	p, err := l.Start(Spec{Name: "u"}, "vmm", "--x")
	if err != nil {
		t.Fatal(err)
	}
	_ = p.Wait()
	if saw[0] != "systemd-run" || saw[len(saw)-1] != "--x" {
		t.Fatalf("saw %q", saw)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func TestSelfUnitFrom(t *testing.T) {
	for in, want := range map[string]string{
		"0::/system.slice/asp-node-agent.service\n":        "asp-node-agent.service",
		"0::/system.slice/asp-node-agent.service":          "asp-node-agent.service",
		"0::/user.slice/user-1000.slice/session-3.scope\n": "",
		"0::/\n": "",
		"12:memory:/system.slice/x.service\n1:name=systemd:/\n": "", // cgroup v1 lines are not read
		"":                                      "",
		"0::/system.slice/with space.service\n": "",
	} {
		if got := selfUnitFrom(in); got != want {
			t.Errorf("selfUnitFrom(%q) = %q, want %q", in, got, want)
		}
	}
}
