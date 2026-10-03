package vmm

import (
	"context"
	"strings"
)

// Workspace share constants. The guest does not auto-mount: when the device
// is present the operator (or guest init) mounts the tag at WorkspaceGuestMount.
const (
	WorkspaceVirtiofsTag = "workspace"
	WorkspaceGuestMount  = "/workspace"
)

// WorkspaceFS reports the virtiofs device Cloud Hypervisor should receive.
// ok is false when no socket is set, so vm.create omits fs entirely.
func WorkspaceFS(cfg MicroVMConfig) (tag, socket string, ok bool) {
	socket = strings.TrimSpace(cfg.WorkspaceFSSocket)
	if socket == "" {
		return "", "", false
	}
	return WorkspaceVirtiofsTag, socket, true
}

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
	// WorkspaceFSSocket is the virtiofsd socket for this sandbox. Empty means
	// do not add an fs device (no workspace, or virtiofsd was not started).
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
