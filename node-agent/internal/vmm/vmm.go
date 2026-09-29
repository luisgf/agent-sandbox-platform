package vmm

import "context"

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
