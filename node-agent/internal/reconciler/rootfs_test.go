package reconciler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

// Each VM must boot its own copy of the rootfs. One shared writable image
// corrupts under two guests and carries one sandbox's writes into the next.
func TestReconcilerClonesRootFSPerSandbox(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.img")
	if err := os.WriteFile(base, []byte("rootfs"), 0o644); err != nil {
		t.Fatal(err)
	}
	cp := newFakeCP(t, "aaaaaaaa-0001", "bbbbbbbb-0002")
	fake := vmm.NewFakeVMM(nil)
	rec := New(cp.client(), "n1", fake, nil, time.Hour)
	rec.VsockDir = t.TempDir()
	rec.RootFSPath = base
	rec.DiskDir = filepath.Join(dir, "disks")
	rec.CloneDisk = func(src, dst string) error {
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	}
	rec.tick(context.Background())

	if len(fake.Configs) != 2 {
		t.Fatalf("want 2 VMs, got %d", len(fake.Configs))
	}
	seen := map[string]bool{}
	for _, cfg := range fake.Configs {
		if cfg.RootFSPath == base {
			t.Fatalf("VM %s booted the shared image", cfg.ID)
		}
		if want := filepath.Join(rec.DiskDir, "rootfs-"+cfg.ID+".img"); cfg.RootFSPath != want {
			t.Fatalf("rootfs=%s want %s", cfg.RootFSPath, want)
		}
		st, err := os.Stat(cfg.RootFSPath)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("clone mode %v, want 0600", st.Mode().Perm())
		}
		seen[cfg.RootFSPath] = true
	}
	if len(seen) != 2 {
		t.Fatal("sandboxes share a rootfs path")
	}

	cp.setState("aaaaaaaa-0001", "stopping")
	rec.tick(context.Background())
	if _, err := os.Stat(filepath.Join(rec.DiskDir, "rootfs-aaaaaaaa-0001.img")); !os.IsNotExist(err) {
		t.Fatalf("clone not removed on stop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rec.DiskDir, "rootfs-bbbbbbbb-0002.img")); err != nil {
		t.Fatalf("other sandbox's clone removed: %v", err)
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("base image touched: %v", err)
	}
}

func TestReconcilerCloneFailureFailsStart(t *testing.T) {
	cp := newFakeCP(t, "cccccccc-0003")
	fake := vmm.NewFakeVMM(nil)
	rec := New(cp.client(), "n1", fake, nil, time.Hour)
	rec.VsockDir = t.TempDir()
	rec.DiskDir = t.TempDir()
	rec.CloneDisk = func(string, string) error { return errors.New("no space left on device") }
	rec.tick(context.Background())

	if len(fake.Configs) != 0 {
		t.Fatal("VM started without its own disk")
	}
	state, detail := cp.state("cccccccc-0003")
	if state != "failed" || !strings.Contains(detail, "rootfs") {
		t.Fatalf("state=%s detail=%q", state, detail)
	}
	if len(rec.Handles()) != 0 {
		t.Fatal("handle kept after failed start")
	}
}
