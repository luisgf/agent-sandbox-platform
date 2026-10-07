package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrintMeasurement(t *testing.T) {
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	rootfs := filepath.Join(dir, "rootfs.img")
	hv := filepath.Join(dir, "cloud-hypervisor")
	for path, content := range map[string]string{kernel: "kernel bytes", rootfs: "image bytes", hv: "#!/bin/sh\necho 'cloud-hypervisor v43.0'\n"} {
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return "sha256:" + hex.EncodeToString(h[:]) }

	var out, errOut bytes.Buffer
	if code := printMeasurement(config{VMMBinary: hv}, kernel, rootfs, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var got allowedImage
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	if got.Kernel != sum("kernel bytes") || got.Rootfs != sum("image bytes") || got.VMM != "cloud-hypervisor v43.0" || got.Name == "" {
		t.Fatalf("entry: %+v", got)
	}

	// Without a hypervisor the entry still prints, accepting any version.
	out.Reset()
	errOut.Reset()
	if code := printMeasurement(config{VMMBinary: filepath.Join(dir, "missing")}, kernel, rootfs, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	got = allowedImage{}
	_ = json.Unmarshal(out.Bytes(), &got)
	if got.VMM != "" || !strings.Contains(errOut.String(), "any version") {
		t.Fatalf("vmm=%q stderr=%q", got.VMM, errOut.String())
	}

	// A missing kernel or image is an error, not an entry with a hole.
	out.Reset()
	errOut.Reset()
	if code := printMeasurement(config{VMMBinary: hv}, filepath.Join(dir, "nope"), rootfs, &out, &errOut); code == 0 || out.Len() != 0 {
		t.Fatalf("a missing kernel printed %q (exit %d)", out.String(), code)
	}
	if code := printMeasurement(config{VMMBinary: hv}, kernel, filepath.Join(dir, "nope"), &out, &errOut); code == 0 || out.Len() != 0 {
		t.Fatalf("a missing image printed %q (exit %d)", out.String(), code)
	}
}
