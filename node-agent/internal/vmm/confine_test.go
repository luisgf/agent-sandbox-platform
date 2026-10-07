package vmm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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

func testUnprivileged() *Unprivileged {
	return &Unprivileged{UIDBase: 0x70000000, Groups: []uint32{991}, RunDir: "/run/asp-vm", Setpriv: "/usr/bin/setpriv"}
}

// The VMM of a VM runs as the user UIDBase+CID, in the group of the same number
// and the supplementary ones it needs, with no capability and no way to get one.
func TestUnprivilegedWrapDropsEverythingButTheUser(t *testing.T) {
	u := testUnprivileged()
	name, args := u.Wrap(7, "/usr/local/bin/cloud-hypervisor", "--api-socket", "/run/asp-vm/x/api.sock", "--seccomp", "true")
	if name != "/usr/bin/setpriv" {
		t.Fatalf("command: %s", name)
	}
	want := []string{"--reuid=1879048199", "--regid=1879048199", "--groups=991",
		"--inh-caps=-all", "--ambient-caps=-all", "--bounding-set=-all", "--no-new-privs", "--",
		"/usr/local/bin/cloud-hypervisor", "--api-socket", "/run/asp-vm/x/api.sock", "--seccomp", "true"}
	if !slices.Equal(args, want) {
		t.Fatalf("args:\n got %v\nwant %v", args, want)
	}
	if u.UID(7) == u.UID(8) {
		t.Fatal("two CIDs share a user")
	}
	// No supplementary group to add means none are kept (setpriv refuses to switch
	// users without being told what to do with the groups).
	u.Groups = nil
	if _, args := u.Wrap(3, "ch"); !slices.Contains(args, "--clear-groups") || strings.Contains(strings.Join(args, " "), "--groups") {
		t.Fatalf("groups: %v", args)
	}
	u.Setpriv = ""
	if name, _ := u.Wrap(3, "ch"); name != "setpriv" {
		t.Fatalf("default command: %s", name)
	}
}

// The unit of an unprivileged VMM may write its directory and its disk, opens no
// IP socket and cannot change namespaces.
func TestUnprivilegedSpecRestrictsTheUnit(t *testing.T) {
	c := testConfinement()
	c.Unprivileged = testUnprivileged()
	disk := filepath.Join(t.TempDir(), "rootfs-abc.img")
	if err := os.WriteFile(disk, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := c.Spec("abc", MicroVMConfig{RunDir: "/run/asp-vm/abc", RootFSPath: disk})
	for _, want := range []string{
		"NoNewPrivileges=yes", "ProtectSystem=strict", "RestrictAddressFamilies=AF_UNIX", "IPAddressDeny=any",
		"RestrictNamespaces=yes", "ReadWritePaths=/run/asp-vm/abc " + disk, "TimeoutStopSec=15",
		"LimitCORE=0", "LimitFSIZE=4096", // no file larger than the disk it was given
	} {
		if !slices.Contains(spec.Properties, want) {
			t.Errorf("the unit lacks %s: %v", want, spec.Properties)
		}
	}
	// A disk that cannot be measured sets no limit rather than a wrong one.
	for _, p := range c.Spec("abc", MicroVMConfig{RunDir: "/x", RootFSPath: "/nonexistent/disk.img"}).Properties {
		if strings.HasPrefix(p, "LimitFSIZE") {
			t.Errorf("limit set for a disk that does not exist: %s", p)
		}
	}
	// virtiofsd stays root, so its unit is not made read-only around the workspace
	// (it chroots there), but it gets no network either.
	fsProps := c.FSSpec("abc").Properties
	for _, p := range fsProps {
		if strings.HasPrefix(p, "ReadWritePaths") || strings.HasPrefix(p, "ProtectSystem") || strings.HasPrefix(p, "RestrictNamespaces") {
			t.Errorf("virtiofsd's unit got %s", p)
		}
	}
	for _, want := range []string{"RestrictAddressFamilies=AF_UNIX", "IPAddressDeny=any", "NoNewPrivileges=yes", "LimitCORE=0"} {
		if !slices.Contains(fsProps, want) {
			t.Errorf("virtiofsd's unit lacks %s: %v", want, fsProps)
		}
	}
	if plain := testConfinement().FSSpec("abc").Properties; !slices.Equal(plain, []string{"TimeoutStopSec=15"}) {
		t.Errorf("virtiofsd's unit without Unprivileged: %v", plain)
	}
	// A confinement without it keeps the units as before.
	plain := testConfinement().Spec("abc", MicroVMConfig{RunDir: "/x"})
	if !slices.Equal(plain.Properties, []string{"TimeoutStopSec=15"}) {
		t.Fatalf("plain confinement: %v", plain.Properties)
	}
}

func TestUnprivilegedValidate(t *testing.T) {
	if err := testUnprivileged().Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (&Unprivileged{UIDBase: 0x70000000, RunDir: "run"}).Validate(); err == nil {
		t.Error("a relative VM directory was accepted")
	}
	if err := (&Unprivileged{UIDBase: 10, RunDir: "/run/x"}).Validate(); err == nil {
		t.Error("a user base inside the system users was accepted")
	}
	if err := (&Unprivileged{UIDBase: 0xFFFFFFF0, RunDir: "/run/x"}).Validate(); err == nil {
		t.Error("a user base with no room for the CIDs was accepted")
	}
}

// With Unprivileged the VMM is started through setpriv, with its API socket in the
// VM's own directory.
func TestPerSandboxStartUnprivileged(t *testing.T) {
	dir, err := os.MkdirTemp("", "ch") // short: unix socket paths are limited to 104 bytes on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	const id = "11111111-2222-4333-8444-555555555555"
	runDir := filepath.Join(dir, "vm", id)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := &fakeCHRunner{}
	var mu sync.Mutex
	var runArgs [][]string
	var client *exec.Cmd
	launcher := unit.Launcher{Command: func(name string, args ...string) *exec.Cmd {
		mu.Lock()
		defer mu.Unlock()
		switch name {
		case "systemd-run":
			runArgs = append(runArgs, append([]string{}, args...))
			if i := slices.Index(args, "--"); i >= 0 {
				if _, err := fake.Start(args[i+1], args[i+2:]...); err != nil {
					t.Errorf("fake CH: %v", err)
				}
			}
			client = exec.Command("sleep", "30")
			return client
		case "systemctl":
			if len(args) > 0 && args[0] == "kill" {
				for _, p := range fake.procs {
					_ = p.exit()
				}
				if client != nil && client.Process != nil {
					_ = client.Process.Kill()
				}
			}
		}
		return exec.Command("true")
	}}
	ch := NewSpawningCloudHypervisor("/usr/local/bin/cloud-hypervisor", dir)
	ch.ReadyTimeout = 5 * time.Second
	ch.Confine = testConfinement()
	ch.Confine.Launcher = launcher
	ch.Confine.Unprivileged = testUnprivileged()

	// The driver refuses a VM it cannot give a user and a directory.
	if err := ch.Start(context.Background(), MicroVMConfig{ID: id, KernelPath: "/k"}); err == nil {
		t.Fatal("started without a directory or a CID")
	}

	cfg := MicroVMConfig{ID: id, KernelPath: "/k", RootFSPath: "/d", CPUs: 1, MemoryMiB: 256, VsockCID: 9, RunDir: runDir}
	if err := ch.Start(context.Background(), cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = ch.Stop(context.Background(), id) })
	if len(runArgs) != 1 {
		t.Fatalf("systemd-run ran %d times", len(runArgs))
	}
	a := strings.Join(runArgs[0], " ")
	sock := filepath.Join(runDir, RunAPISocket)
	for _, want := range []string{
		"--unit=asp-vm-" + id,
		"--property=ReadWritePaths=" + runDir + " /d",
		"--property=IPAddressDeny=any",
		"-- /usr/bin/setpriv --reuid=1879048201 --regid=1879048201 --groups=991 ",
		" -- /usr/local/bin/cloud-hypervisor --api-socket " + sock + " --seccomp true",
	} {
		if !strings.Contains(a, want) {
			t.Errorf("systemd-run args lack %q:\n%s", want, a)
		}
	}
	if got := ch.SocketPath(id); got != sock {
		t.Fatalf("API socket %s, want %s", got, sock)
	}
	if fake.creates != 1 || fake.boots != 1 {
		t.Fatalf("creates=%d boots=%d", fake.creates, fake.boots)
	}
	// The reaper finds the VM by that socket.
	argv := append([]string{"cloud-hypervisor"}, fake.starts[0].Args[len(fake.starts[0].Args)-4:]...)
	if got, ok := SpawnedSandbox(argv, dir, filepath.Join(dir, "vm")); !ok || got != id {
		t.Fatalf("SpawnedSandbox(%q) = %q %v", argv, got, ok)
	}
	if _, ok := SpawnedSandbox(argv, dir, ""); ok {
		t.Fatal("a socket in a VM directory matched without that directory being named")
	}
	if _, ok := SpawnedSandbox([]string{"cloud-hypervisor", "--api-socket", filepath.Join(dir, "other", id, RunAPISocket)}, dir, filepath.Join(dir, "vm")); ok {
		t.Fatal("a socket under another directory matched")
	}
	if _, ok := SpawnedSandbox([]string{"cloud-hypervisor", "--api-socket", filepath.Join(runDir, "x.sock")}, dir, filepath.Join(dir, "vm")); ok {
		t.Fatal("a socket that is not the API socket matched")
	}
}
