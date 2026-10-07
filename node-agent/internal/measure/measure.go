// Package measure hashes the files a node boots VMs from, so a boot
// attestation can say which kernel and which base image were booted instead of
// repeating a name the control plane already knew.
package measure

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Prefix starts every digest this package returns.
const Prefix = "sha256:"

// Cache hashes files and remembers the sum until the file's size, modification
// time or inode changes, so a multi-gigabyte base image is read once per
// version and not once per sandbox.
type Cache struct {
	mu    sync.Mutex
	files map[string]entry
}

type entry struct {
	size  int64
	mtime time.Time
	ino   uint64
	sum   string
}

// NewCache returns an empty Cache.
func NewCache() *Cache { return &Cache{files: map[string]entry{}} }

func identity(fi os.FileInfo) entry {
	e := entry{size: fi.Size(), mtime: fi.ModTime()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		e.ino = uint64(st.Ino)
	}
	return e
}

func (e entry) same(o entry) bool {
	return e.size == o.size && e.mtime.Equal(o.mtime) && e.ino == o.ino
}

// Peek returns the digest of path if the cache holds it and the file has not changed since,
// and "" otherwise. It never reads the file.
func (c *Cache) Peek(path string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	if cached, ok := c.files[path]; ok && cached.same(identity(fi)) {
		return cached.sum
	}
	return ""
}

// SHA256 returns "sha256:<hex>" for the file at path, following symlinks. Callers
// asking for the same file at once wait for the first read.
func (c *Cache) SHA256(path string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.files == nil {
		c.files = map[string]entry{}
	}
	for attempt := 0; attempt < 2; attempt++ {
		fi, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if !fi.Mode().IsRegular() {
			return "", fmt.Errorf("%s is not a regular file", path)
		}
		before := identity(fi)
		if cached, ok := c.files[path]; ok && cached.same(before) {
			return cached.sum, nil
		}
		sum, after, err := hashFile(path)
		if err != nil {
			return "", err
		}
		if !before.same(after) {
			continue // replaced or written while it was read: read it again
		}
		before.sum = sum
		c.files[path] = before
		return sum, nil
	}
	return "", fmt.Errorf("%s kept changing while it was read", path)
}

func hashFile(path string) (string, entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", entry{}, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", entry{}, err
	}
	fi, err := f.Stat()
	if err != nil {
		return "", entry{}, err
	}
	return Prefix + hex.EncodeToString(h.Sum(nil)), identity(fi), nil
}

// Valid reports whether s is a digest this package produces: "sha256:" and 64
// lowercase hex digits.
func Valid(s string) bool {
	hexPart, ok := strings.CutPrefix(s, Prefix)
	if !ok || len(hexPart) != 64 {
		return false
	}
	for _, c := range hexPart {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// VMMVersion is the first line `binary --version` prints (for Cloud Hypervisor,
// "cloud-hypervisor v43.0"), cut to a sane length.
func VMMVersion(ctx context.Context, binary string) (string, error) {
	if strings.TrimSpace(binary) == "" {
		return "", errors.New("no VMM binary")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", binary, err)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	line = strings.TrimSpace(line)
	if len(line) > 128 {
		line = line[:128]
	}
	if line == "" {
		return "", fmt.Errorf("%s --version printed nothing", binary)
	}
	return line, nil
}

// SumsFile is the name of the checksums file a release puts next to the guest files.
const SumsFile = "SHA256SUMS"

// FromSums finds name in the text of a SHA256SUMS ("<hex>  <name>" per line, a "*" before the
// name allowed) and returns its digest as "sha256:<hex>".
func FromSums(sums []byte, name string) (digest string, ok bool) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			return Prefix + strings.ToLower(f[0]), true
		}
	}
	return "", false
}

// Expected is the digest a SHA256SUMS next to the file gives it: next to the path as named,
// and then next to the file a symlink leads to (a release installed by asp image pull is
// linked into /opt/sandbox, and the sums sit with the files in the version's directory).
func Expected(path string) (digest string, listed bool) {
	candidates := []string{path}
	if real, err := filepath.EvalSymlinks(path); err == nil && real != path {
		candidates = append(candidates, real)
	}
	for _, c := range candidates {
		b, err := os.ReadFile(filepath.Join(filepath.Dir(c), SumsFile))
		if err != nil {
			continue
		}
		if d, ok := FromSums(b, filepath.Base(c)); ok {
			return d, true
		}
	}
	return "", false
}
