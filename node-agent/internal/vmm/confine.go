package vmm

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/unit"
)

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
	// Unprivileged, when set, runs each VMM as a user of its own instead of root
	// (see Unprivileged). Nil keeps the VMM root, as it was.
	Unprivileged *Unprivileged
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
	props := c.lifetime()
	if c.Unprivileged != nil {
		props = append(props, c.Unprivileged.Properties(cfg)...)
	}
	return unit.Spec{
		Name:        UnitName(sandboxID),
		Slice:       c.Slice,
		Description: "ASP microVM " + sandboxID,
		MemoryMax:   int64(mem+c.MemoryOverheadMiB) << 20,
		CPUQuota:    cpus*100 + c.CPUOverheadPercent,
		TasksMax:    c.TasksMax,
		Properties:  props,
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
	props := c.lifetime()
	if c.Unprivileged != nil {
		props = append(props, fsHardening...)
	}
	return unit.Spec{
		Name:        FSUnitName(sandboxID),
		Slice:       c.Slice,
		Description: "ASP virtiofsd " + sandboxID,
		MemoryMax:   int64(c.FSMemoryMiB) << 20,
		TasksMax:    c.FSTasksMax,
		Properties:  props,
	}
}

// fsHardening is what virtiofsd's unit is kept from while it stays root (it must act
// as the owner of each file of the workspace): it serves a unix socket and the files
// of the workspace and has no use for the network, a core file, or a new privilege.
// What it may do inside the workspace is left to its own chroot, capability drop and
// seccomp filter.
var fsHardening = []string{
	"NoNewPrivileges=yes",
	"RestrictAddressFamilies=AF_UNIX",
	"IPAddressDeny=any",
	"PrivateTmp=yes",
	"RestrictRealtime=yes",
	"LockPersonality=yes",
	"SystemCallArchitectures=native",
	"LimitCORE=0",
}

// Unprivileged runs the VMM of each sandbox as a user and group of its own, with
// no capabilities and no way to get any, and fences its service in with what a
// VMM that only needs files, /dev/kvm and a TAP does not use: no IP sockets, no
// writes outside its directory and its disk. A flaw in Cloud Hypervisor that
// gives a guest code execution on the host then lands in a process that can
// reach the files of that one VM and nothing else, not in root.
//
// The VM's user is UIDBase plus its guest CID, so two VMs running at once never
// share one (the agent reserves the CID of a VM it adopts), and a VM can open only
// what the agent handed to its user: the directory under RunDir where it keeps
// its sockets, its disk and the TAP the agent created for it.
//
// virtiofsd stays root: it must read and write the workspace as whichever user
// owns each file, which an unprivileged daemon cannot do. It runs chrooted into
// the workspace instead (--virtiofsd-sandbox).
type Unprivileged struct {
	// UIDBase is the first user id of the VMs: a VM with guest CID n runs as
	// UIDBase+n, in the group of the same number.
	UIDBase uint32
	// Groups are supplementary groups the VMM needs: the one that owns /dev/kvm.
	Groups []uint32
	// RunDir is the directory that holds one directory per VM (RunDir/<sandbox>),
	// owned by the VM's user. The VMM creates its sockets there, so the path
	// through RunDir must be searchable by every user (0711) and RunDir itself
	// must not be a place others can create entries in.
	RunDir string
	// Setpriv is the setpriv(1) executable. Empty means "setpriv".
	Setpriv string
}

// Names of the files in a VM's directory (RunDir/<sandbox>). They are short: a
// unix socket path is limited to 107 bytes, and the guest to host sockets add
// "_<port>" to the vsock one.
const (
	RunAPISocket    = "api.sock"
	RunVsockSocket  = "vsock.sock"
	RunSerialSocket = "serial.sock"
	RunFSSocket     = "virtiofs.sock"
)

// DefaultUIDBase is above the ranges systemd and the container tools hand out
// (system users, dynamic users, containers at 524288 to 1878982655).
const DefaultUIDBase uint32 = 0x70000000

// UID is the user and group id of the VM with guest CID cid.
func (u *Unprivileged) UID(cid uint32) uint32 { return u.UIDBase + cid }

// Dir is the directory of sandbox id under RunDir.
func (u *Unprivileged) Dir(id string) string { return filepath.Join(u.RunDir, id) }

func (u *Unprivileged) setpriv() string {
	if u.Setpriv != "" {
		return u.Setpriv
	}
	return "setpriv"
}

// Wrap is the command that runs name args... as the user of the VM with CID cid:
// setpriv switches to that user and group, adds the supplementary groups, empties
// every capability set and forbids getting privileges back (a setuid binary), then
// executes the VMM in place, so the unit's main process is the VMM itself.
func (u *Unprivileged) Wrap(cid uint32, name string, args ...string) (string, []string) {
	id := strconv.FormatUint(uint64(u.UID(cid)), 10)
	groups := "--clear-groups"
	if len(u.Groups) > 0 {
		gs := make([]string, len(u.Groups))
		for i, g := range u.Groups {
			gs[i] = strconv.FormatUint(uint64(g), 10)
		}
		groups = "--groups=" + strings.Join(gs, ",")
	}
	a := []string{"--reuid=" + id, "--regid=" + id, groups,
		"--inh-caps=-all", "--ambient-caps=-all", "--bounding-set=-all", "--no-new-privs", "--", name}
	return u.setpriv(), append(a, args...)
}

// Properties are the systemd properties of a VMM's unit under Unprivileged. They
// restrict the service, not the user: the files the VMM may write are its
// directory and its disk, it opens no IP socket (its network is the TAP it was
// given, its other peers are unix sockets), and the service cannot change
// namespaces, personality or scheduling class.
func (u *Unprivileged) Properties(cfg MicroVMConfig) []string {
	rw := []string{}
	if cfg.RunDir != "" {
		rw = append(rw, cfg.RunDir)
	}
	if cfg.RootFSPath != "" {
		rw = append(rw, cfg.RootFSPath)
	}
	p := []string{
		"NoNewPrivileges=yes",
		"PrivateTmp=yes",
		"ProtectHome=yes",
		"ProtectSystem=strict",
		"RestrictAddressFamilies=AF_UNIX",
		"IPAddressDeny=any",
		"RestrictNamespaces=yes",
		"RestrictRealtime=yes",
		"RestrictSUIDSGID=yes",
		"LockPersonality=yes",
		"SystemCallArchitectures=native",
		"UMask=0077",
		"LimitCORE=0",
	}
	if len(rw) > 0 {
		p = append(p, "ReadWritePaths="+strings.Join(rw, " "))
	}
	// A raw disk never grows: the VMM writes inside the file it was given, so no
	// file it writes may be larger than that one. A VMM that is not itself any more
	// then cannot fill the disk of the node with it.
	if cfg.RootFSPath != "" {
		if fi, err := os.Stat(cfg.RootFSPath); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
			p = append(p, "LimitFSIZE="+strconv.FormatInt(fi.Size(), 10))
		}
	}
	return p
}

// Validate says why the settings cannot be used.
func (u *Unprivileged) Validate() error {
	if u.RunDir == "" || !filepath.IsAbs(u.RunDir) {
		return fmt.Errorf("the VM directory %q is not an absolute path", u.RunDir)
	}
	if u.UIDBase < 1000 {
		return fmt.Errorf("the VM user base %d is in the range of the system's own users", u.UIDBase)
	}
	// Room for a million CIDs below the largest user id (4294967295 is "none").
	if u.UIDBase > 0xFFFFFFFF-1<<20 {
		return fmt.Errorf("the VM user base %d leaves no room for the CIDs of the VMs", u.UIDBase)
	}
	return nil
}
