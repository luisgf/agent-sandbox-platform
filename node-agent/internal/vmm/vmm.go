package vmm

import "context"

// Workspace share constants. The guest mount is a convention for when a
// virtiofs device actually exists. Today the reconciler records the host
// path and leaves WorkspaceFSSocket empty, so Cloud Hypervisor does not
// get an fs device.
const (
	WorkspaceVirtiofsTag = "workspace"
	WorkspaceGuestMount  = "/workspace"
)

type MicroVMConfig struct {
	ID          string
	KernelPath  string
	Cmdline     string
	RootFSPath  string
	CPUs        int
	MemoryMiB   int
	VsockCID    uint32
	VsockPath   string
	TapDevice   string
	ExtraParams []string
	// WorkspaceHostPath is the host directory requested by the sandbox spec.
	// FakeVMM stores it. It is not a mount by itself.
	WorkspaceHostPath string
	// WorkspaceFSSocket is a virtiofsd socket. Empty means do not add an fs
	// device to Cloud Hypervisor (the reconciler never fills this).
	WorkspaceFSSocket string
}

// MicroVM is the lifecycle interface used by the node-agent reconciler.
type MicroVM interface {
	Start(ctx context.Context, config MicroVMConfig) error
	Stop(ctx context.Context, id string) error
	Pause(ctx context.Context, id string) error
}

// VMM is a lower-level Cloud Hypervisor-oriented interface for create/boot/delete.
type VMM interface {
	Ping(ctx context.Context) error
	Info(ctx context.Context) (map[string]any, error)
	CreateVM(ctx context.Context, config MicroVMConfig) error
	Boot(ctx context.Context) error
	Delete(ctx context.Context) error
}
