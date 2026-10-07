package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

func confineCfg(mode string) config {
	return config{VMConfine: mode, VMSlice: "asp-vms.slice", VMMemoryOverheadMiB: 256, VMCPUOverheadPercent: 50, VMTasksMax: 1024}
}

func TestVMConfinementModes(t *testing.T) {
	cannot := errors.New("systemd is not the init system")

	if c, err := vmConfinement(confineCfg("off"), nil); c != nil || err != nil {
		t.Errorf("off: %v %v", c, err)
	}
	if c, err := vmConfinement(confineCfg("auto"), cannot); c != nil || err != nil {
		t.Errorf("auto on a host that cannot: %v %v", c, err)
	}
	c, err := vmConfinement(confineCfg("auto"), nil)
	if err != nil || c == nil || c.Slice != "asp-vms.slice" || c.MemoryOverheadMiB != 256 || c.CPUOverheadPercent != 50 || c.TasksMax != 1024 || c.FSMemoryMiB == 0 || c.FSTasksMax == 0 {
		t.Errorf("auto on a host that can: %+v %v", c, err)
	}
	if c, err := vmConfinement(confineCfg(""), nil); err != nil || c == nil {
		t.Errorf("an empty mode is auto: %v %v", c, err)
	}
	if c, err := vmConfinement(confineCfg(" ON "), nil); err != nil || c == nil {
		t.Errorf("on: %v %v", c, err)
	}
	// on refuses to run unconfined by accident.
	if c, err := vmConfinement(confineCfg("on"), cannot); err == nil || c != nil || !strings.Contains(err.Error(), "init system") {
		t.Errorf("on on a host that cannot: %v %v", c, err)
	}
}

func TestVMConfinementValidatesItsNumbers(t *testing.T) {
	for name, mod := range map[string]func(*config){
		"unknown mode":    func(c *config) { c.VMConfine = "maybe" },
		"negative memory": func(c *config) { c.VMMemoryOverheadMiB = -1 },
		"negative cpu":    func(c *config) { c.VMCPUOverheadPercent = -1 },
		"no tasks":        func(c *config) { c.VMTasksMax = 0 },
		"not a slice":     func(c *config) { c.VMSlice = "asp-vms" },
	} {
		cfg := confineCfg("on")
		mod(&cfg)
		if _, err := vmConfinement(cfg, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// An empty slice leaves the unit in the default one.
	cfg := confineCfg("on")
	cfg.VMSlice = ""
	if c, err := vmConfinement(cfg, nil); err != nil || c.Slice != "" {
		t.Errorf("empty slice: %+v %v", c, err)
	}
}

// A VM survives the agent unless it is bound to it: the units are bound only when
// --vm-survive-restart is off, and only a confined VM is recorded to be adopted.
func TestVMsSurviveTheAgentByDefault(t *testing.T) {
	cfg := confineCfg("on")
	cfg.VMSurviveRestart = true
	c, err := vmConfinement(cfg, nil)
	if err != nil || c.BindTo != "" {
		t.Fatalf("survive: %+v %v", c, err)
	}
	cfg.VMSurviveRestart = false
	c, err = vmConfinement(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.BindTo != bindTo(cfg) {
		t.Fatalf("bound to %q, want the agent's own service %q", c.BindTo, bindTo(cfg))
	}
	// Under systemd that is the agent's service; from a shell there is none.
	if bindTo(config{VMSurviveRestart: true}) != "" {
		t.Fatal("survive still binds the VMs")
	}
}

func TestVMStateDirOnlyForVMsThatCanSurvive(t *testing.T) {
	confine := &vmm.Confinement{}
	base := config{CHSocketDir: "/run/asp", VMSurviveRestart: true}
	if got := vmStateDir(base, confine); got != "/run/asp/state" {
		t.Fatalf("state dir %q", got)
	}
	for name, mod := range map[string]func(*config){
		"dry-run":     func(c *config) { c.DryRun = true },
		"shared CH":   func(c *config) { c.CHAPISocket = "/run/ch.sock" },
		"not survive": func(c *config) { c.VMSurviveRestart = false },
		"unconfined":  nil,
	} {
		cfg, c := base, confine
		if mod == nil {
			c = nil
		} else {
			mod(&cfg)
		}
		if got := vmStateDir(cfg, c); got != "" {
			t.Errorf("%s: state dir %q, want none", name, got)
		}
	}
}

func TestRegisterRequestCarriesTheAdoptedVMs(t *testing.T) {
	cfg := config{NodeID: "n1", AdoptedSandboxes: []string{"a", "b"}}
	if got := registerRequest(cfg).AdoptedSandboxes; len(got) != 2 || got[0] != "a" {
		t.Fatalf("adopted %v", got)
	}
	if registerRequest(config{NodeID: "n1"}).AdoptedSandboxes != nil {
		t.Fatal("nothing adopted, nothing sent")
	}
}
