package main

import (
	"flag"
	"os"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/reconciler"
)

// The kernel and the base image default to the paths every host has had, and
// --guest-kernel / --guest-rootfs (or their variables) replace them.
func TestGuestImageFlags(t *testing.T) {
	old := flag.CommandLine
	oldArgs := os.Args
	defer func() { flag.CommandLine, os.Args = old, oldArgs }()

	flag.CommandLine = flag.NewFlagSet("node-agent", flag.ContinueOnError)
	os.Args = []string{"node-agent"}
	cfg := loadConfig()
	if cfg.GuestKernel != reconciler.DefaultKernelPath || cfg.GuestRootFS != reconciler.DefaultRootFSPath {
		t.Fatalf("defaults: %q %q", cfg.GuestKernel, cfg.GuestRootFS)
	}

	t.Setenv("ASP_GUEST_ROOTFS", "/srv/from-env.img")
	flag.CommandLine = flag.NewFlagSet("node-agent", flag.ContinueOnError)
	os.Args = []string{"node-agent", "--guest-kernel=/srv/vmlinux-new"}
	cfg = loadConfig()
	if cfg.GuestKernel != "/srv/vmlinux-new" || cfg.GuestRootFS != "/srv/from-env.img" {
		t.Fatalf("overrides: %q %q", cfg.GuestKernel, cfg.GuestRootFS)
	}
}
