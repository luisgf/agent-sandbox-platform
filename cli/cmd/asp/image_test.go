package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// guestRelease serves the files of a guest release under /rel/asp-guest_0.1.0_*.
func guestRelease(t *testing.T, tamperKernel bool) (*httptest.Server, []byte, []byte) {
	t.Helper()
	rootfs := append(bytes.Repeat([]byte{0}, 200<<10), []byte("the end of the image")...)
	var gzbuf bytes.Buffer
	w := gzip.NewWriter(&gzbuf)
	_, _ = w.Write(rootfs)
	_ = w.Close()
	kernel := []byte("a kernel, as far as the test is concerned")
	manifest := []byte(fmt.Sprintf(`{"schema":1,"version":"0.1.0","rootfs":{"file":"rootfs.img","sha256":%q,"size":%d,"gzip":{"file":"rootfs.img.gz","sha256":%q,"size":%d}},"kernel":{"file":"vmlinux","sha256":%q,"size":%d}}`,
		sum(rootfs), len(rootfs), sum(gzbuf.Bytes()), gzbuf.Len(), sum(kernel), len(kernel)))
	sums := []byte(fmt.Sprintf("%s  rootfs.img\n%s  rootfs.img.gz\n%s  image.json\n%s  vmlinux\n", sum(rootfs), sum(gzbuf.Bytes()), sum(manifest), sum(kernel)))
	served := map[string][]byte{"SHA256SUMS": sums, "image.json": manifest, "rootfs.img.gz": gzbuf.Bytes(), "vmlinux": kernel}
	if tamperKernel {
		served["vmlinux"] = []byte("a kernel that someone changed on the way")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := served[strings.TrimPrefix(r.URL.Path, "/rel/asp-guest_0.1.0_")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, rootfs, kernel
}

func TestImagePullAndVerify(t *testing.T) {
	srv, rootfs, kernel := guestRelease(t, false)
	dir, link := t.TempDir(), filepath.Join(t.TempDir(), "sandbox")
	var stdout, stderr strings.Builder
	code := run([]string{"image", "pull", "--version", "v0.1.0", "--base-url", srv.URL + "/rel", "--dir", dir, "--link", link}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("pull exit %d: %s%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"installed 0.1.0", "ASP_GUEST_ROOTFS=" + filepath.Join(dir, "current", "rootfs.img"), "ASP_GUEST_KERNEL="} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, stdout.String())
		}
	}
	if b, _ := os.ReadFile(filepath.Join(link, "rootfs.img")); !bytes.Equal(b, rootfs) {
		t.Error("the rootfs through the link is not the release's")
	}
	if b, _ := os.ReadFile(filepath.Join(link, "vmlinux")); !bytes.Equal(b, kernel) {
		t.Error("the kernel through the link is not the release's")
	}

	// verify: through the link the node-agent reads, and then with a changed file.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"image", "verify", link}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "ok    vmlinux") {
		t.Fatalf("verify exit %d: %s%s", code, stdout.String(), stderr.String())
	}
	if err := os.WriteFile(filepath.Join(dir, "0.1.0", "vmlinux"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"image", "verify", "--json", link}, &stdout, &stderr); code != 1 {
		t.Fatalf("verify of a changed kernel: exit %d, want 1", code)
	}
	var out struct {
		OK     bool `json:"ok"`
		Checks []struct {
			File string `json:"file"`
			OK   bool   `json:"ok"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(stdout.String()), &out); err != nil || out.OK {
		t.Fatalf("verify --json: %q %v", stdout.String(), err)
	}
}

func TestImagePullRefusesATamperedKernel(t *testing.T) {
	srv, _, _ := guestRelease(t, true)
	dir := t.TempDir()
	var stdout, stderr strings.Builder
	code := run([]string{"image", "pull", "--version", "0.1.0", "--base-url", srv.URL + "/rel", "--dir", dir, "--no-link"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "nothing was installed") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a refused pull left %d entries in %s", len(entries), dir)
	}
}

func TestImagePullNeedsAVersionFromADevBuild(t *testing.T) {
	var stdout, stderr strings.Builder
	if code := run([]string{"image", "pull", "--dir", t.TempDir()}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--version") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if code := run([]string{"image"}, &stdout, &stderr); code != 2 {
		t.Fatalf("image alone: exit %d", code)
	}
	if code := run([]string{"image", "frobnicate"}, &stdout, &stderr); code != 2 {
		t.Fatalf("unknown subcommand: exit %d", code)
	}
}
