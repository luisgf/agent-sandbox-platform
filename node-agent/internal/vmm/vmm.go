package vmm

import (
	"context"
	"strings"
	"time"
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
	// SerialSocket is a unix socket Cloud Hypervisor serves the guest's serial
	// console on, which the agent reads into a bounded buffer (Console). Empty
	// leaves the console alone, as before.
	SerialSocket string
}

// MicroVM is the lifecycle interface used by the node-agent reconciler.
type MicroVM interface {
	Start(ctx context.Context, config MicroVMConfig) error
	Stop(ctx context.Context, id string) error
	Pause(ctx context.Context, id string) error
}

// ConsoleReader is implemented by VMMs that keep the last of a guest's serial
// console, for the log of a guest that never came up.
type ConsoleReader interface {
	// ConsoleTail returns up to max bytes of the console of sandbox id, or "".
	ConsoleTail(id string, max int) string
}

// Shutdowner is implemented by VMMs that can tell when a guest has powered
// itself off. The reconciler asks the guest to power off, then waits here
// before Stop, so the disk is left clean.
type Shutdowner interface {
	// WaitShutdown blocks until the VM for id reports Shutdown or its process is
	// gone, or grace passes, or ctx ends. It returns true when the VM is down.
	WaitShutdown(ctx context.Context, id string, grace time.Duration) bool
}

// VMM is a lower-level Cloud Hypervisor-oriented interface for create/boot/delete.
type VMM interface {
	Ping(ctx context.Context) error
	Info(ctx context.Context) (map[string]any, error)
	CreateVM(ctx context.Context, config MicroVMConfig) error
	Boot(ctx context.Context) error
	Delete(ctx context.Context) error
}
