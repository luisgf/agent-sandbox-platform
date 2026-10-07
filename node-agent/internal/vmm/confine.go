package vmm

import "github.com/luisgf/agent-sandbox-platform/node-agent/internal/unit"

// Confinement runs each VMM and each virtiofsd in a transient systemd service of
// its own: a cgroup per VM, with limits derived from what the VM was given, shown
// by systemd-cgls, with the process's output in the journal. Without it they are
// children of the agent, in its cgroup, free to compete with it for everything.
type Confinement struct {
	Launcher unit.Launcher
	// Slice is the slice the units live in (asp-vms.slice).
	Slice string
	// MemoryOverheadMiB is what Cloud Hypervisor itself needs on top of the guest's
	// memory (its own heap, device queues, the page cache of the disk it reads).
	MemoryOverheadMiB int
	// CPUOverheadPercent is added to the guest's vCPUs, in percent of one CPU, for
	// the VMM's own threads (I/O, the API).
	CPUOverheadPercent int
	// TasksMax bounds the processes and threads of one VMM.
	TasksMax int
	// FSMemoryMiB and FSTasksMax bound one virtiofsd.
	FSMemoryMiB int
	FSTasksMax  int
	// BindTo is the service the agent itself runs in. The VM units are bound to it:
	// when it stops or dies, systemd stops them, as KillMode=control-group did when
	// the VMs were its children, so no VM is left running for nobody. Empty (the
	// agent was started from a shell) binds them to nothing.
	BindTo string
}

// Defaults for a Confinement.
const (
	DefaultSlice              = "asp-vms.slice"
	DefaultMemoryOverheadMiB  = 256
	DefaultCPUOverheadPercent = 50
	DefaultTasksMax           = 1024
	DefaultFSMemoryMiB        = 512
	DefaultFSTasksMax         = 256
)

// UnitName is the service a sandbox's VMM runs in.
func UnitName(sandboxID string) string { return "asp-vm-" + sandboxID }

// FSUnitName is the service a sandbox's virtiofsd runs in.
func FSUnitName(sandboxID string) string { return "asp-vm-" + sandboxID + "-fs" }

// Spec is the unit of the VMM of one sandbox: its memory plus the overhead, its
// vCPUs plus the overhead.
func (c *Confinement) Spec(sandboxID string, cfg MicroVMConfig) unit.Spec {
	cpus := cfg.CPUs
	if cpus <= 0 {
		cpus = 1
	}
	mem := cfg.MemoryMiB
	if mem <= 0 {
		mem = 256
	}
	return unit.Spec{
		Name:        UnitName(sandboxID),
		Slice:       c.Slice,
		Description: "ASP microVM " + sandboxID,
		MemoryMax:   int64(mem+c.MemoryOverheadMiB) << 20,
		CPUQuota:    cpus*100 + c.CPUOverheadPercent,
		TasksMax:    c.TasksMax,
		Properties:  c.lifetime(),
	}
}

// lifetime ties a VM unit to the agent's, and gives a stop a short deadline: the
// VMM and virtiofsd end on SIGTERM at once, and a hung one must not hold up the
// agent's own stop for systemd's default 90 seconds.
func (c *Confinement) lifetime() []string {
	p := []string{"TimeoutStopSec=15"}
	if c.BindTo != "" {
		p = append(p, "BindsTo="+c.BindTo, "After="+c.BindTo)
	}
	return p
}

// FSSpec is the unit of the virtiofsd of one sandbox.
func (c *Confinement) FSSpec(sandboxID string) unit.Spec {
	return unit.Spec{
		Name:        FSUnitName(sandboxID),
		Slice:       c.Slice,
		Description: "ASP virtiofsd " + sandboxID,
		MemoryMax:   int64(c.FSMemoryMiB) << 20,
		TasksMax:    c.FSTasksMax,
		Properties:  c.lifetime(),
	}
}
