package vmm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/unit"
)

const adoptID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

// adoptRig is a Cloud Hypervisor that "an earlier agent started": a fake API on
// the socket the agent would use, and a systemctl whose answers the test controls.
type adoptRig struct {
	dir    string
	runner *fakeCHRunner
	proc   *fakeCHProc
	launch unit.Launcher
}

func newAdoptRig(t *testing.T) *adoptRig {
	t.Helper()
	dir := shortTemp(t)
	rig := &adoptRig{dir: dir, runner: &fakeCHRunner{}}
	p, err := rig.runner.Start("cloud-hypervisor", "--api-socket", filepath.Join(dir, "ch-"+adoptID+".sock"))
	if err != nil {
		t.Fatal(err)
	}
	rig.proc = p.(*fakeCHProc)
	t.Cleanup(func() { _ = rig.proc.exit() })

	ctl := filepath.Join(dir, "systemctl")
	script := `#!/bin/sh
echo "$@" >> "` + dir + `/ctl.log"
case "$1" in
  show) cat "` + dir + `/show.out" ;;
  is-active) [ -f "` + dir + `/active" ] ;;
  kill) cp "` + dir + `/ended.out" "` + dir + `/show.out"; rm -f "` + dir + `/active" ;;
esac
`
	if err := os.WriteFile(ctl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	rig.write("show.out", "LoadState=loaded\nActiveState=active\nMainPID=4242\nResult=success\nExecMainCode=0\nExecMainStatus=0\n")
	rig.write("ended.out", "LoadState=not-found\nActiveState=inactive\nMainPID=0\nResult=success\nExecMainCode=0\nExecMainStatus=0\n")
	rig.write("active", "")
	rig.launch = unit.Launcher{Systemctl: ctl}
	return rig
}

func (r *adoptRig) write(name, text string) {
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(text), 0o644); err != nil {
		panic(err)
	}
}

// ch is the new agent's VMM.
func (r *adoptRig) ch() *CloudHypervisor {
	ch := NewSpawningCloudHypervisor("cloud-hypervisor", r.dir)
	ch.Confine = &Confinement{Launcher: r.launch}
	ch.ReadyTimeout = 2 * time.Second
	return ch
}

// A VM that kept running while the agent restarted is taken over: the new agent
// supervises it as if it had started it, tells when its process ends, and stops it.
func TestAdoptTakesOverARunningVM(t *testing.T) {
	rig := newAdoptRig(t)
	ch := rig.ch()
	got := newExits()
	ch.SetExitHandler(got.handle)
	ctx := context.Background()
	if err := ch.Alive(ctx, adoptID, ""); err != nil {
		t.Fatalf("Alive: %v", err)
	}
	booted := time.Now().Add(-time.Hour)
	if err := ch.Adopt(ctx, adoptID, AdoptedVM{BootedAt: booted}); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if ch.InstanceCount() != 1 {
		t.Fatalf("instances=%d", ch.InstanceCount())
	}
	if err := ch.Adopt(ctx, adoptID, AdoptedVM{BootedAt: booted}); err == nil {
		t.Fatal("the same VM was adopted twice")
	}

	// Its service ending is reported like any exit nobody asked for, with how long it had run.
	rig.write("show.out", "LoadState=not-found\nActiveState=inactive\nMainPID=0\nResult=success\nExecMainCode=0\nExecMainStatus=0\n")
	select {
	case who := <-got.ch:
		if who != adoptID {
			t.Fatalf("reported %q", who)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the end of an adopted VM was never reported")
	}
	info := got.got[adoptID][0]
	if info.Err == nil || info.Lived < 59*time.Minute {
		t.Fatalf("info=%+v", info)
	}
}

func TestAdoptedVMIsStoppedThroughItsService(t *testing.T) {
	rig := newAdoptRig(t)
	ch := rig.ch()
	ctx := context.Background()
	if err := ch.Adopt(ctx, adoptID, AdoptedVM{BootedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := ch.Stop(ctx, adoptID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	log, _ := os.ReadFile(filepath.Join(rig.dir, "ctl.log"))
	if !strings.Contains(string(log), "kill --signal=SIGKILL --kill-whom=all asp-vm-"+adoptID+".service") {
		t.Fatalf("systemctl saw %q", log)
	}
	rig.runner.mu.Lock()
	deletes := rig.runner.deletes
	rig.runner.mu.Unlock()
	if deletes != 1 || ch.InstanceCount() != 0 {
		t.Fatalf("vm.delete sent %d times, %d instances left", deletes, ch.InstanceCount())
	}
}

// Anything that stops a VM being what it was is a reason not to take it over: the
// reconciler then removes what is left of it.
func TestAliveSaysWhyAVMCannotBeAdopted(t *testing.T) {
	ctx := context.Background()
	t.Run("service not active", func(t *testing.T) {
		rig := newAdoptRig(t)
		_ = os.Remove(filepath.Join(rig.dir, "active"))
		if err := rig.ch().Alive(ctx, adoptID, ""); err == nil || !strings.Contains(err.Error(), "not active") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("API gone", func(t *testing.T) {
		rig := newAdoptRig(t)
		_ = rig.proc.exit()
		if err := rig.ch().Alive(ctx, adoptID, ""); err == nil || !strings.Contains(err.Error(), "API does not answer") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("the VM shut down", func(t *testing.T) {
		rig := newAdoptRig(t)
		rig.runner.mu.Lock()
		rig.runner.vmInfoState = "Shutdown"
		rig.runner.mu.Unlock()
		if err := rig.ch().Alive(ctx, adoptID, ""); err == nil || !strings.Contains(err.Error(), "Shutdown") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("paused is alive", func(t *testing.T) {
		rig := newAdoptRig(t)
		rig.runner.mu.Lock()
		rig.runner.vmInfoState = "Paused"
		rig.runner.mu.Unlock()
		if err := rig.ch().Alive(ctx, adoptID, ""); err != nil {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("not confined", func(t *testing.T) {
		rig := newAdoptRig(t)
		ch := rig.ch()
		ch.Confine = nil
		if err := ch.Alive(ctx, adoptID, ""); err == nil || !strings.Contains(err.Error(), "children of the agent") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("shared mode", func(t *testing.T) {
		rig := newAdoptRig(t)
		if err := NewCloudHypervisor("cloud-hypervisor", filepath.Join(rig.dir, "x.sock")).Alive(ctx, adoptID, ""); err == nil {
			t.Fatal("a shared Cloud Hypervisor can be adopted")
		}
	})
}
