package main

import (
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

// goodHost is a host that can run VMMs as users of their own, and what was asked
// of it.
type goodHost struct {
	p       vmUserProbes
	ran     [][]string
	ensured []string
	// failOn makes the probe whose argv contains it fail.
	failOn string
}

func newGoodHost() *goodHost {
	h := &goodHost{}
	h.p = vmUserProbes{
		LookPath: func(name string) (string, error) {
			if name == "setpriv" {
				return "/usr/bin/setpriv", nil
			}
			return "/usr/local/bin/" + name, nil
		},
		KVMGroup: func() (uint32, bool, error) { return 991, true, nil },
		RunAs: func(_ *vmm.Unprivileged, argv ...string) error {
			h.ran = append(h.ran, argv)
			if h.failOn != "" && strings.Contains(strings.Join(argv, " "), h.failOn) {
				return errors.New("exit status 1")
			}
			return nil
		},
		EnsureDir: func(dir string) error { h.ensured = append(h.ensured, dir); return nil },
	}
	return h
}

func unprivCfg(mode string) config {
	return config{
		VMUnprivileged: mode, VMUIDBase: uint(vmm.DefaultUIDBase), CHSocketDir: "/run/asp", VMMBinary: "cloud-hypervisor",
		GuestKernel: "/opt/sandbox/vmlinux", DiskDir: "/var/lib/asp/disks",
	}
}

func TestVMUnprivilegedModes(t *testing.T) {
	confine := &vmm.Confinement{}
	for _, mode := range []string{"off", "OFF"} {
		if u, err := vmUnprivileged(unprivCfg(mode), confine, newGoodHost().p); u != nil || err != nil {
			t.Errorf("%s: %v %v", mode, u, err)
		}
	}
	if _, err := vmUnprivileged(unprivCfg("maybe"), confine, newGoodHost().p); err == nil || !strings.Contains(err.Error(), "unknown mode") {
		t.Errorf("unknown mode: %v", err)
	}
	for _, mode := range []string{"", "auto", "on"} {
		u, err := vmUnprivileged(unprivCfg(mode), confine, newGoodHost().p)
		if err != nil || u == nil {
			t.Fatalf("%q on a good host: %v %v", mode, u, err)
		}
		if u.UIDBase != vmm.DefaultUIDBase || u.RunDir != "/run/asp-vm" || u.Setpriv != "/usr/bin/setpriv" || !slices.Equal(u.Groups, []uint32{991}) {
			t.Fatalf("%q: %+v", mode, u)
		}
	}
}

// The VMM user is checked against everything the VMM opens, by trying as that user.
func TestVMUnprivilegedProbesWhatTheVMMOpens(t *testing.T) {
	h := newGoodHost()
	if _, err := vmUnprivileged(unprivCfg("on"), &vmm.Confinement{}, h.p); err != nil {
		t.Fatal(err)
	}
	var ran []string
	for _, argv := range h.ran {
		ran = append(ran, strings.Join(argv, " "))
	}
	for _, want := range []string{
		"true",
		"/usr/local/bin/cloud-hypervisor --version",
		"sh -c exec 3<\"$1\" sh /opt/sandbox/vmlinux",
		"sh -c exec 3<>\"$1\" sh /dev/kvm",
		"sh -c exec 3<>\"$1\" sh /dev/net/tun",
		"sh -c cd \"$1\" sh /run/asp-vm",
		"sh -c cd \"$1\" sh /var/lib/asp/disks",
	} {
		if !slices.Contains(ran, want) {
			t.Errorf("not probed: %q (probed %q)", want, ran)
		}
	}
	// The directories the VMs work in are made searchable before they are probed.
	if !slices.Contains(h.ensured, "/run/asp-vm") || !slices.Contains(h.ensured, "/var/lib/asp/disks") {
		t.Errorf("ensured %q", h.ensured)
	}
}

// With "on" a host that cannot refuses to start; with "auto" the VMMs stay root.
func TestVMUnprivilegedWhenTheHostCannot(t *testing.T) {
	confine := &vmm.Confinement{}
	cases := map[string]func(h *goodHost){
		"no setpriv": func(h *goodHost) {
			h.p.LookPath = func(name string) (string, error) {
				if name == "setpriv" {
					return "", exec.ErrNotFound
				}
				return "/x/" + name, nil
			}
		},
		"no hypervisor": func(h *goodHost) {
			old := h.p.LookPath
			h.p.LookPath = func(name string) (string, error) {
				if name == "cloud-hypervisor" {
					return "", exec.ErrNotFound
				}
				return old(name)
			}
		},
		"no /dev/kvm": func(h *goodHost) {
			h.p.KVMGroup = func() (uint32, bool, error) { return 0, false, errors.New("no such file") }
		},
		"kernel unreadable":      func(h *goodHost) { h.failOn = "/opt/sandbox/vmlinux" },
		"kvm closed":             func(h *goodHost) { h.failOn = "/dev/kvm" },
		"tun closed":             func(h *goodHost) { h.failOn = "/dev/net/tun" },
		"the VM directory":       func(h *goodHost) { h.failOn = "sh /run/asp-vm" },
		"cannot make the dir":    func(h *goodHost) { h.p.EnsureDir = func(string) error { return errors.New("read-only file system") } },
		"setpriv without --flag": func(h *goodHost) { h.failOn = "true" },
	}
	for name, mutate := range cases {
		h := newGoodHost()
		mutate(h)
		if u, err := vmUnprivileged(unprivCfg("auto"), confine, h.p); u != nil || err != nil {
			t.Errorf("%s, auto: %v %v (the VMMs should stay root)", name, u, err)
		}
		h = newGoodHost()
		mutate(h)
		if u, err := vmUnprivileged(unprivCfg("on"), confine, h.p); u != nil || err == nil || !strings.Contains(err.Error(), "cannot run microVMs as unprivileged users") {
			t.Errorf("%s, on: %v %v (should refuse to start)", name, u, err)
		}
	}
	// Without a unit of their own there is nothing to restrict.
	if u, err := vmUnprivileged(unprivCfg("auto"), nil, newGoodHost().p); u != nil || err != nil {
		t.Errorf("no confinement, auto: %v %v", u, err)
	}
	if _, err := vmUnprivileged(unprivCfg("on"), nil, newGoodHost().p); err == nil || !strings.Contains(err.Error(), "--vm-confine") {
		t.Errorf("no confinement, on: %v", err)
	}
	// A user base in the system's range is a mistake, whatever the mode.
	bad := unprivCfg("auto")
	bad.VMUIDBase = 100
	if _, err := vmUnprivileged(bad, confine, newGoodHost().p); err == nil {
		t.Error("a user base of 100 was accepted")
	}
	bad.VMUIDBase = 1 << 33
	if _, err := vmUnprivileged(bad, confine, newGoodHost().p); err == nil {
		t.Error("a user base that is not a user id was accepted")
	}
}

func TestVMUnprivilegedKVMGroup(t *testing.T) {
	h := newGoodHost()
	h.p.KVMGroup = func() (uint32, bool, error) { return 0, false, nil } // /dev/kvm is 0666
	u, err := vmUnprivileged(unprivCfg("on"), &vmm.Confinement{}, h.p)
	if err != nil || len(u.Groups) != 0 {
		t.Fatalf("a world-accessible /dev/kvm needs no group: %+v %v", u, err)
	}
}

func TestVMRunDir(t *testing.T) {
	for _, tc := range []struct{ socketDir, flag, want string }{
		{"/run/asp", "", "/run/asp-vm"},
		{"/run/asp/", "", "/run/asp-vm"},
		{"/sandbox/t/run", "", "/sandbox/t/run-vm"},
		{"/run/asp", "/run/vms/", "/run/vms"},
	} {
		if got := vmRunDir(config{CHSocketDir: tc.socketDir, VMRunDir: tc.flag}); got != tc.want {
			t.Errorf("socket dir %q flag %q: %q, want %q", tc.socketDir, tc.flag, got, tc.want)
		}
	}
}
