package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/measure"
)

// vmmVersion is what the hypervisor binary reports, or "" (with a warning) when
// it cannot be asked.
func vmmVersion(ctx context.Context, binary string) string {
	v, err := measure.VMMVersion(ctx, binary)
	if err != nil {
		slog.Warn("boot attestation: hypervisor version unknown", "error", err)
		return ""
	}
	return v
}

// allowedImage is one entry of the control plane's ASP_ATTEST_ALLOWED_IMAGES
// file: a kernel, a base image and the hypervisor that boots them.
type allowedImage struct {
	Name   string `json:"name"`
	Kernel string `json:"kernel"`
	Rootfs string `json:"rootfs"`
	VMM    string `json:"vmm,omitempty"`
}

// printMeasurement writes the entry that makes this node's current kernel and
// base image acceptable to a control plane with an image allowlist, and returns
// the process exit code. The operator names it and adds it to the file.
func printMeasurement(cfg config, kernelPath, rootfsPath string, stdout, stderr io.Writer) int {
	cache := measure.NewCache()
	kernel, err := cache.SHA256(kernelPath)
	if err != nil {
		fmt.Fprintf(stderr, "kernel: %v\n", err)
		return 1
	}
	rootfs, err := cache.SHA256(rootfsPath)
	if err != nil {
		fmt.Fprintf(stderr, "base image: %v\n", err)
		return 1
	}
	entry := allowedImage{Name: "<name this image>", Kernel: kernel, Rootfs: rootfs}
	if v, err := measure.VMMVersion(context.Background(), cfg.VMMBinary); err != nil {
		fmt.Fprintf(stderr, "hypervisor version: %v (the entry will accept any version)\n", err)
	} else {
		entry.VMM = v
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(entry); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}
