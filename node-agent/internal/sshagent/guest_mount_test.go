package sshagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuestSSHAgentUnitFilesExist(t *testing.T) {
	root := findRepoRoot(t)
	files := []string{
		"images/guest/systemd/ssh-agent-vsock.service",
		"images/guest/openrc/ssh-agent-vsock",
		"images/guest/cmd/vsock-ssh-agent-proxy/main.go",
		"images/guest/helpers/ssh-agent-vsock-socat.sh",
		"docs/why-2e-ssh-guest-mount.md",
		"docs/why-2e-nft-redirect.md",
		"docs/adr/0006-fase-2e-nft-ssh-guest.md",
	}
	for _, rel := range files {
		p := filepath.Join(root, rel)
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			t.Fatalf("missing %s: %v", rel, err)
		}
	}
	unit, err := os.ReadFile(filepath.Join(root, "images/guest/systemd/ssh-agent-vsock.service"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(unit)
	for _, want := range []string{"vsock-ssh-agent-proxy", "26501", "/run/agent-sandbox/ssh-agent.sock"} {
		if !strings.Contains(body, want) {
			t.Fatalf("unit missing %q", want)
		}
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := wd
	for i := 0; i < 8; i++ {
		if st, err := os.Stat(filepath.Join(dir, "images", "guest", "systemd", "ssh-agent-vsock.service")); err == nil && !st.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("repo root not found from", wd)
	return ""
}
