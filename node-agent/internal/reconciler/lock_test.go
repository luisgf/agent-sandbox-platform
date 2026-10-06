//go:build unix

package reconciler

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLockSocketDirAdmitsOneAgent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "asp")
	first, err := LockSocketDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	if _, err := LockSocketDir(dir); !errors.Is(err, ErrSocketDirLocked) || !strings.Contains(err.Error(), "pid "+strconv.Itoa(os.Getpid())) {
		t.Fatalf("second lock = %v, want ErrSocketDirLocked naming pid %d", err, os.Getpid())
	}
	if st, err := os.Stat(filepath.Join(dir, lockFileName)); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("lock file: %v %v", st, err)
	}

	// The agent's children (cloud-hypervisor, virtiofsd) must not inherit it:
	// they outlive a crashed agent and would keep the next one out.
	child := exec.Command("sleep", "5")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	first.Release()
	again, err := LockSocketDir(dir)
	if err != nil {
		t.Fatalf("lock after release, with a child still running: %v", err)
	}
	again.Release()
}
