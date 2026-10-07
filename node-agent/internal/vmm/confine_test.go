package vmm

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/unit"
)

func testConfinement() *Confinement {
	return &Confinement{
		Slice:              DefaultSlice,
		MemoryOverheadMiB:  256,
		CPUOverheadPercent: 50,
		TasksMax:           1024,
		FSMemoryMiB:        512,
		FSTasksMax:         256,
	}
}

func TestConfinementSpecs(t *testing.T) {
	c := testConfinement()
	spec := c.Spec("abc", MicroVMConfig{CPUs: 2, MemoryMiB: 512})
	if spec.Name != "asp-vm-abc" || spec.Slice != "asp-vms.slice" || spec.Description != "ASP microVM abc" {
		t.Fatalf("spec identity: %+v", spec)
	}
	// 512 MiB of guest plus 256 of VMM; two vCPUs plus half a CPU.
	if spec.MemoryMax != 768<<20 || spec.CPUQuota != 250 || spec.TasksMax != 1024 {
		t.Fatalf("limits: %+v", spec)
	}
	// A config with no size gets what createVMWith would give it.
	def := c.Spec("abc", MicroVMConfig{})
	if def.MemoryMax != (256+256)<<20 || def.CPUQuota != 150 {
		t.Fatalf("defaults: %+v", def)
	}
	if !slices.Equal(spec.Properties, []string{"TimeoutStopSec=15"}) {
		t.Fatalf("an agent that is not a service binds nothing: %v", spec.Properties)
	}
	c.BindTo = "asp-node-agent.service"
	bound := c.Spec("abc", MicroVMConfig{})
	if !slices.Contains(bound.Properties, "BindsTo=asp-node-agent.service") || !slices.Contains(bound.Properties, "After=asp-node-agent.service") {
		t.Fatalf("the VM unit is not bound to the agent's: %v", bound.Properties)
	}
	if fsBound := c.FSSpec("abc"); !slices.Contains(fsBound.Properties, "BindsTo=asp-node-agent.service") {
		t.Fatalf("the virtiofsd unit is not bound to the agent's: %v", fsBound.Properties)
	}
	c.BindTo = ""
	fs := c.FSSpec("abc")
	if fs.Name != "asp-vm-abc-fs" || fs.MemoryMax != 512<<20 || fs.TasksMax != 256 || fs.CPUQuota != 0 {
		t.Fatalf("fs spec: %+v", fs)
	}
	if UnitName("x") == FSUnitName("x") {
		t.Fatal("the VMM and its virtiofsd share a unit name")
	}
}

// With confinement the VMM starts through systemd-run, in a unit with limits
// from the VM's config, and stopping it kills the unit.
func TestPerSandboxStartAndStopConfined(t *testing.T) {
	dir, err := os.MkdirTemp("", "ch") // short: unix socket paths are limited to 104 bytes on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	fake := &fakeCHRunner{}
	var mu sync.Mutex
	var runArgs, ctlArgs [][]string
	var client *exec.Cmd
	launcher := unit.Launcher{Command: func(name string, args ...string) *exec.Cmd {
		mu.Lock()
		defer mu.Unlock()
		switch name {
		case "systemd-run":
			runArgs = append(runArgs, append([]string{}, args...))
			// The unit would run the command after "--": do it in this process.
			if i := slices.Index(args, "--"); i >= 0 {
				if _, err := fake.Start(args[i+1], args[i+2:]...); err != nil {
					t.Errorf("fake CH: %v", err)
				}
			}
			client = exec.Command("sleep", "30")
			return client
		case "systemctl":
			ctlArgs = append(ctlArgs, append([]string{}, args...))
			if len(args) > 0 && args[0] == "kill" {
				// Killing the unit ends the command and with it systemd-run.
				for _, p := range fake.procs {
					_ = p.exit()
				}
				if client != nil && client.Process != nil {
					_ = client.Process.Kill()
				}
			}
			return exec.Command("true")
		}
		return exec.Command("true")
	}}
	ch := NewSpawningCloudHypervisor("/usr/local/bin/cloud-hypervisor", dir)
	ch.ReadyTimeout = 5 * time.Second
	ch.Confine = testConfinement()
	ch.Confine.Launcher = launcher

	const id = "11111111-2222-4333-8444-555555555555"
	cfg := MicroVMConfig{ID: id, KernelPath: "/k", RootFSPath: "/d", CPUs: 2, MemoryMiB: 512}
	if err := ch.Start(context.Background(), cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(runArgs) != 1 {
		t.Fatalf("systemd-run ran %d times", len(runArgs))
	}
	a := strings.Join(runArgs[0], " ")
	sock := dir + "/" + APISocketName(id)
	for _, want := range []string{
		"--unit=asp-vm-" + id,
		"--slice=asp-vms.slice",
		"--property=MemoryMax=805306368",
		"--property=CPUQuota=250%",
		"--property=TasksMax=1024",
		"-- /usr/local/bin/cloud-hypervisor --api-socket " + sock + " --seccomp true",
	} {
		if !strings.Contains(a, want) {
			t.Errorf("systemd-run args lack %q:\n%s", want, a)
		}
	}
	if fake.creates != 1 || fake.boots != 1 {
		t.Fatalf("creates=%d boots=%d", fake.creates, fake.boots)
	}

	if err := ch.Stop(context.Background(), id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	var killed bool
	for _, c := range ctlArgs {
		if len(c) > 0 && c[0] == "kill" && slices.Contains(c, "asp-vm-"+id+".service") && slices.Contains(c, "--signal=SIGKILL") {
			killed = true
		}
	}
	if !killed {
		t.Fatalf("Stop did not kill the unit: %v", ctlArgs)
	}
}

// Without confinement the VMM is still started as before, now with the seccomp
// filter spelled out.
func TestPerSandboxStartUnconfinedAsksForSeccomp(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeCHRunner{}
	ch := NewSpawningCloudHypervisor("cloud-hypervisor", dir)
	ch.Runner = fake
	ch.ReadyTimeout = 5 * time.Second
	if err := ch.Start(context.Background(), MicroVMConfig{ID: "sb-1", KernelPath: "/k"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = ch.Stop(context.Background(), "sb-1") }()
	if len(fake.starts) != 1 {
		t.Fatalf("starts: %v", fake.starts)
	}
	got := strings.Join(fake.starts[0].Args, " ")
	if got != "--api-socket "+dir+"/"+APISocketName("sb-1")+" --seccomp true" {
		t.Fatalf("args: %s", got)
	}
}
