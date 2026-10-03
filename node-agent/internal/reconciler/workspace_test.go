package reconciler

import (
	"testing"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/cpclient"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

func TestVMConfigRecordsWorkspaceWithoutVirtiofsSocket(t *testing.T) {
	r := &Reconciler{VsockDir: t.TempDir(), KernelPath: "/k", RootFSPath: "/r"}
	cfg := r.vmConfig(cpclient.Sandbox{
		ID:                "sb-ws",
		CPUMillis:         1000,
		MemoryMiB:         128,
		WorkspaceHostPath: "/data/proj",
	})
	if cfg.WorkspaceHostPath != "/data/proj" {
		t.Fatalf("host=%q", cfg.WorkspaceHostPath)
	}
	if cfg.WorkspaceFSSocket != "" {
		t.Fatalf("socket should stay empty, got %q", cfg.WorkspaceFSSocket)
	}
	if cfg.ID != "sb-ws" {
		t.Fatalf("id=%s", cfg.ID)
	}
	_ = vmm.WorkspaceGuestMount
}
