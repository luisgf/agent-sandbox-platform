package measure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func sumOf(b []byte) string {
	s := sha256.Sum256(b)
	return Prefix + hex.EncodeToString(s[:])
}

func TestSHA256MatchesAndCaches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rootfs.img")
	if err := os.WriteFile(path, []byte("image v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewCache()
	got, err := c.SHA256(path)
	if err != nil || got != sumOf([]byte("image v1")) {
		t.Fatalf("SHA256 = %q, %v", got, err)
	}
	if !Valid(got) {
		t.Fatalf("%q is not a valid digest", got)
	}

	// Same size, same mtime, other content: the cache answers, as it must for a
	// file it has no reason to re-read.
	fi, _ := os.Stat(path)
	if err := os.WriteFile(path, []byte("image v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if again, _ := c.SHA256(path); again != got {
		t.Fatalf("a file with unchanged size and mtime was read again: %q", again)
	}

	// A new mtime or size is a new version.
	later := fi.ModTime().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if v2, _ := c.SHA256(path); v2 != sumOf([]byte("image v2")) {
		t.Fatalf("a changed file kept its old digest: %q", v2)
	}
	if err := os.WriteFile(path, []byte("a longer image v3"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v3, _ := c.SHA256(path); v3 != sumOf([]byte("a longer image v3")) {
		t.Fatalf("a resized file kept its old digest: %q", v3)
	}
}

func TestSHA256Errors(t *testing.T) {
	c := NewCache()
	dir := t.TempDir()
	if _, err := c.SHA256(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing file was hashed")
	}
	if _, err := c.SHA256(dir); err == nil || !strings.Contains(err.Error(), "regular") {
		t.Errorf("a directory was hashed: %v", err)
	}
	var zero Cache
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := zero.SHA256(p); err != nil || got != sumOf([]byte("x")) {
		t.Errorf("the zero Cache: %q, %v", got, err)
	}
}

func TestSHA256FollowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.img")
	if err := os.WriteFile(real, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.img")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if got, err := NewCache().SHA256(link); err != nil || got != sumOf([]byte("payload")) {
		t.Fatalf("through a symlink: %q, %v", got, err)
	}
}

func TestSHA256Concurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(path, []byte("kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewCache()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := c.SHA256(path); err != nil || got != sumOf([]byte("kernel")) {
				t.Errorf("concurrent SHA256 = %q, %v", got, err)
			}
		}()
	}
	wg.Wait()
}

func TestValid(t *testing.T) {
	good := sumOf([]byte("x"))
	for s, want := range map[string]bool{
		good:                                true,
		"":                                  false,
		"sha256:":                           false,
		"sha256:abc":                        false,
		strings.ToUpper(good):               false, // the prefix and the hex are lowercase
		"sha512:" + good[len(Prefix):]:      false,
		good + "0":                          false,
		"sha256:" + strings.Repeat("g", 64): false,
		"debian-asp":                        false, // an image reference is not a digest
	} {
		if Valid(s) != want {
			t.Errorf("Valid(%q) = %v, want %v", s, !want, want)
		}
	}
}

func TestVMMVersion(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "cloud-hypervisor")
	script := "#!/bin/sh\necho 'cloud-hypervisor v43.0'\necho 'second line'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := VMMVersion(context.Background(), bin)
	if err != nil || got != "cloud-hypervisor v43.0" {
		t.Fatalf("VMMVersion = %q, %v", got, err)
	}
	if _, err := VMMVersion(context.Background(), filepath.Join(dir, "nope")); err == nil {
		t.Error("a missing binary reported a version")
	}
	if _, err := VMMVersion(context.Background(), ""); err == nil {
		t.Error("no binary reported a version")
	}
	empty := filepath.Join(dir, "silent")
	if err := os.WriteFile(empty, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := VMMVersion(context.Background(), empty); err == nil {
		t.Error("a binary that prints nothing reported a version")
	}
}
