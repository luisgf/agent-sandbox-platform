package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/doctor"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

func TestDoctorConfigFollowsTheNodesSettings(t *testing.T) {
	cfg := config{
		NodeID: "n1", GuestKernel: "/k", GuestRootFS: "/missing/rootfs.img", DiskDir: "/d", DiskMinFreeMiB: 4096,
		VMMBinary: "ch", VirtiofsdBin: "vfsd", VMConfine: " ON ", TapAuto: true, HostVsock: true, HostVsockDir: "/hv",
		EgressNFTRedirect: true, EgressProxyListen: ":8888", NFTEgressMode: "enforce",
	}
	dc := doctorConfig(cfg, nil)
	if dc.NodeID != "n1" || dc.GuestKernel != "/k" || dc.DiskDir != "/d" || dc.DiskMinFreeMiB != 4096 || dc.CHBinary != "ch" ||
		dc.VirtiofsdBin != "vfsd" || dc.Confine != "on" || !dc.TapAuto || !dc.HostVsock || dc.HostVsockDir != "/hv" ||
		!dc.EgressRedirect || dc.EgressProxyListen != ":8888" || dc.NFTMode != "enforce" {
		t.Fatalf("%+v", dc)
	}
	// With no control-plane client the check says so instead of crashing.
	if err := dc.ControlPlane(t.Context()); err == nil {
		t.Fatal("no client, no error")
	}
	// A negative --disk-min-free-mib means twice the base image, resolved like the agent does.
	cfg.DiskMinFreeMiB = -1
	if got := doctorConfig(cfg, nil).DiskMinFreeMiB; got != 0 {
		t.Fatalf("the base image is not there to measure: %d", got)
	}
}

func TestDoctorChecksEndWithTheControlPlaneAndIncludeTheVMMUser(t *testing.T) {
	var names []string
	for _, c := range doctorChecks(config{DiskMinFreeMiB: 1}, nil) {
		names = append(names, c.Name)
	}
	if names[len(names)-1] != "control-plane" || names[len(names)-2] != "vmm-user" {
		t.Fatalf("order: %v", names)
	}
}

// doctorCfg is the configuration of a node with the VMM settings the agent defaults to.
func doctorCfg(mode string) config {
	cfg := unprivCfg(mode)
	cfg.VMConfine, cfg.VMTasksMax, cfg.VMSlice = "auto", 1024, vmm.DefaultSlice
	return cfg
}

func TestCheckVMMUser(t *testing.T) {
	good := newGoodHost()
	cfg := doctorCfg("auto")
	unavailable := errors.New("systemd is not the init system")

	cfg.DryRun = true
	if r := checkVMMUser(cfg, nil, good.p); r.Status != doctor.Skip {
		t.Errorf("dry-run: %+v", r)
	}
	cfg.DryRun = false
	if r := checkVMMUser(doctorCfg("off"), nil, good.p); r.Status != doctor.Skip {
		t.Errorf("off: %+v", r)
	}
	// Not confined (auto on a host without systemd): the VMMs are root, and say why elsewhere.
	if r := checkVMMUser(cfg, unavailable, good.p); r.Status != doctor.Skip || !strings.Contains(r.Detail, "run as root") {
		t.Errorf("not confined: %+v", r)
	}
	if r := checkVMMUser(doctorCfg("on"), unavailable, good.p); r.Status != doctor.Fail || !strings.Contains(r.Detail, "systemd is not the init system") {
		t.Errorf("confinement required and absent: %+v", r)
	}
	if r := checkVMMUser(cfg, nil, good.p); r.Status != doctor.OK || !strings.Contains(r.Detail, "its own user") {
		t.Errorf("good host: %+v", r)
	}
	// A host that cannot: a warning when the mode is auto (the VMMs stay root), a failure
	// when the operator asked for on.
	bad := newGoodHost()
	bad.failOn = "/opt/sandbox/vmlinux"
	r := checkVMMUser(cfg, nil, bad.p)
	if r.Status != doctor.Warn || !strings.Contains(r.Detail, "cannot read the kernel") || r.Fix == "" {
		t.Errorf("auto on a bad host: %+v", r)
	}
	if r := checkVMMUser(doctorCfg("on"), nil, bad.p); r.Status != doctor.Fail {
		t.Errorf("on, bad host: %+v", r)
	}
}
