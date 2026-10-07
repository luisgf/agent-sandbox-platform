package unit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// adoptTools is a systemctl whose answers a test writes into files: show.out is
// what `systemctl show` prints, active exists while the unit is active.
func adoptTools(t *testing.T) (Launcher, string) {
	t.Helper()
	dir := t.TempDir()
	ctl := filepath.Join(dir, "systemctl")
	body := `#!/bin/sh
echo "$@" >> "` + dir + `/ctl.log"
case "$1" in
  show) cat "` + dir + `/show.out" ;;
  is-active) [ -f "` + dir + `/active" ] ;;
  stop) [ -f "` + dir + `/gone" ] && { echo "Failed to stop x.service: Unit x.service not loaded." >&2; exit 5; }; exit 0 ;;
  kill) [ -f "` + dir + `/gone" ] && { echo "Failed to kill unit: Unit x.service not loaded." >&2; exit 5; }; exit 0 ;;
esac
`
	if err := os.WriteFile(ctl, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return Launcher{Systemctl: ctl}, dir
}

func setShow(t *testing.T, dir, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "show.out"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	showRunning   = "LoadState=loaded\nActiveState=active\nMainPID=4242\nResult=success\nExecMainCode=0\nExecMainStatus=0\n"
	showForgotten = "LoadState=not-found\nActiveState=inactive\nMainPID=0\nResult=success\nExecMainCode=0\nExecMainStatus=0\n"
	showKilled    = "LoadState=loaded\nActiveState=failed\nMainPID=0\nResult=signal\nExecMainCode=2\nExecMainStatus=9\n"
	showClean     = "LoadState=loaded\nActiveState=inactive\nMainPID=0\nResult=success\nExecMainCode=1\nExecMainStatus=0\n"
)

func TestActiveAndStop(t *testing.T) {
	l, dir := adoptTools(t)
	if l.Active("asp-vm-x") {
		t.Fatal("a unit that is not active reported as active")
	}
	if err := os.WriteFile(filepath.Join(dir, "active"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !l.Active("asp-vm-x") {
		t.Fatal("an active unit reported as inactive")
	}
	if err := l.Stop("asp-vm-x"); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "ctl.log"))
	if !strings.Contains(string(log), "stop asp-vm-x.service") || !strings.Contains(string(log), "is-active --quiet asp-vm-x.service") {
		t.Fatalf("systemctl saw %q", log)
	}
	// A unit that is already gone is not an error to stop.
	if err := os.WriteFile(filepath.Join(dir, "gone"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.Stop("asp-vm-x"); err != nil {
		t.Fatalf("stop of a gone unit: %v", err)
	}
}

// An adopted service is watched by asking systemd: Wait returns when it stops
// running, and what it says depends on what systemd still knows.
func TestAdoptedWaitEndsWhenTheServiceDoes(t *testing.T) {
	for name, c := range map[string]struct {
		after   string
		wantErr string // "" means nil
	}{
		"collected: nothing is known": {showForgotten, "forgotten how"},
		"killed, still loaded":        {showKilled, "result=signal (code 2, status 9)"},
		"ended cleanly, still loaded": {showClean, ""},
	} {
		t.Run(name, func(t *testing.T) {
			l, dir := adoptTools(t)
			setShow(t, dir, showRunning)
			a := l.Adopt("asp-vm-x")
			a.SetPollInterval(10 * time.Millisecond)
			done := make(chan error, 1)
			go func() { done <- a.Wait() }()
			select {
			case err := <-done:
				t.Fatalf("Wait returned while the service ran: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			if a.Pid() != 4242 {
				t.Fatalf("pid %d, want the main pid systemd reported", a.Pid())
			}
			setShow(t, dir, c.after)
			select {
			case err := <-done:
				switch {
				case c.wantErr == "" && err != nil:
					t.Fatalf("Wait: %v", err)
				case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
					t.Fatalf("Wait: %v, want %q", err, c.wantErr)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Wait did not return when the service ended")
			}
		})
	}
}

func TestAdoptedKillAndStopWatching(t *testing.T) {
	l, dir := adoptTools(t)
	setShow(t, dir, showRunning)
	a := l.Adopt("asp-vm-x")
	a.SetPollInterval(10 * time.Millisecond)
	if a.Unit() != "asp-vm-x.service" {
		t.Fatalf("unit %q", a.Unit())
	}
	if err := a.Kill(); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "ctl.log"))
	if !strings.Contains(string(log), "kill --signal=SIGKILL --kill-whom=all asp-vm-x.service") {
		t.Fatalf("systemctl saw %q", log)
	}
	if err := os.WriteFile(filepath.Join(dir, "gone"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Kill(); err != nil {
		t.Fatalf("kill of a gone unit: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- a.Wait() }()
	time.Sleep(50 * time.Millisecond)
	a.Stop()
	a.Stop() // twice is fine
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Wait of a watch that was stopped returned as if the service had ended cleanly")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not end the watch")
	}
}
