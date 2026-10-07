// Package imagepull installs the guest kernel and image of a release on a node, and
// checks what is installed.
//
// A release publishes the files with a SHA256SUMS and an image.json (scripts/build-guest-image.sh).
// Nothing is trusted but those checksums: a download that does not match is refused, and
// nothing of the installed set changes until every file has been checked.
package imagepull

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Names of the files a set is made of.
const (
	SumsFile     = "SHA256SUMS"
	ManifestFile = "image.json"
	RootFSFile   = "rootfs.img"
	RootFSGzip   = "rootfs.img.gz"
	KernelFile   = "vmlinux"
)

// ErrChecksum means a file is not what the checksums say it is.
var ErrChecksum = errors.New("checksum mismatch")

// File is a file in image.json.
type File struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Manifest is image.json.
type Manifest struct {
	Schema          int    `json:"schema"`
	Version         string `json:"version"`
	Commit          string `json:"commit"`
	SourceDateEpoch int64  `json:"source_date_epoch"`
	Base            struct {
		Image    string `json:"image"`
		Snapshot string `json:"snapshot"`
	} `json:"base"`
	Rootfs struct {
		File
		Gzip File `json:"gzip"`
	} `json:"rootfs"`
	Kernel   *File `json:"kernel,omitempty"`
	Packages []struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"packages"`
}

var (
	hexRe     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	versionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
)

// ParseSums reads a SHA256SUMS: "<hex>  <name>" per line (a "*" before the name, for
// binary mode, is accepted). A name with a directory in it is refused.
func ParseSums(text string) (map[string]string, error) {
	out := map[string]string{}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		sum, name, ok := strings.Cut(line, " ")
		name = strings.TrimPrefix(strings.TrimPrefix(name, " "), "*")
		if !ok || !hexRe.MatchString(sum) || name == "" || name != filepath.Base(name) || name == "." || name == ".." {
			return nil, fmt.Errorf("SHA256SUMS line %d is not \"<sha256>  <file>\": %q", i+1, line)
		}
		if prev, dup := out[name]; dup && prev != sum {
			return nil, fmt.Errorf("SHA256SUMS lists %s twice with different sums", name)
		}
		out[name] = sum
	}
	if len(out) == 0 {
		return nil, errors.New("SHA256SUMS is empty")
	}
	return out, nil
}

// Options says what to pull and where to put it.
type Options struct {
	// Version is the release (0.1.0). It names the directory the set is installed in.
	Version string
	// BaseURL is where the release's files are, without the trailing slash, e.g.
	// https://github.com/<owner>/<repo>/releases/download/v0.1.0.
	BaseURL string
	// Prefix starts the name of every guest file in the release (asp-guest_0.1.0_).
	Prefix string
	// Dir is where sets are installed: <Dir>/<Version>/..., and <Dir>/current points at the last.
	Dir string
	// LinkDir, when not empty, gets symlinks to the set through <Dir>/current (the paths the
	// node-agent reads by default are /opt/sandbox/vmlinux and /opt/sandbox/rootfs.img). A file
	// there that is not a symlink is left alone.
	LinkDir string
	HTTP    *http.Client
	// Progress, when set, is told what is happening.
	Progress func(format string, args ...any)
}

// Result is what a pull did.
type Result struct {
	Dir              string   `json:"dir"`
	Version          string   `json:"version"`
	Rootfs           string   `json:"rootfs"`
	Kernel           string   `json:"kernel,omitempty"`
	AlreadyInstalled bool     `json:"already_installed"`
	Linked           []string `json:"linked,omitempty"`
	Skipped          []string `json:"skipped,omitempty"`
}

func (o *Options) say(format string, args ...any) {
	if o.Progress != nil {
		o.Progress(format, args...)
	}
}

// Pull downloads the release's guest files, checks them, and installs them.
func Pull(ctx context.Context, o Options) (Result, error) {
	if !versionRe.MatchString(o.Version) {
		return Result{}, fmt.Errorf("version %q is not a version (letters, digits, . _ + -)", o.Version)
	}
	if o.BaseURL == "" || o.Dir == "" {
		return Result{}, errors.New("a base URL and a directory are required")
	}
	if o.HTTP == nil {
		o.HTTP = http.DefaultClient
	}
	base := strings.TrimRight(o.BaseURL, "/") + "/" + o.Prefix

	o.say("fetching the checksums of %s", o.Version)
	sumsText, err := o.getSmall(ctx, base+SumsFile, 1<<20)
	if err != nil {
		return Result{}, err
	}
	sums, err := ParseSums(string(sumsText))
	if err != nil {
		return Result{}, err
	}
	manifestBytes, err := o.getSmall(ctx, base+ManifestFile, 8<<20)
	if err != nil {
		return Result{}, err
	}
	if got := sumOf(manifestBytes); sums[ManifestFile] != got {
		return Result{}, fmt.Errorf("%w: %s is %s, SHA256SUMS says %s", ErrChecksum, ManifestFile, got, sums[ManifestFile])
	}
	var m Manifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return Result{}, fmt.Errorf("%s: %w", ManifestFile, err)
	}
	if err := m.consistentWith(sums); err != nil {
		return Result{}, err
	}

	final := filepath.Join(o.Dir, o.Version)
	res := Result{Dir: final, Version: o.Version, Rootfs: filepath.Join(final, RootFSFile)}
	if m.Kernel != nil {
		res.Kernel = filepath.Join(final, KernelFile)
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return Result{}, err
	}
	if installedMatches(final, sumsText) {
		o.say("%s is already installed and its checksums match", final)
		res.AlreadyInstalled = true
	} else {
		if err := o.install(ctx, base, m, sums, sumsText, manifestBytes, final); err != nil {
			return Result{}, err
		}
	}
	if err := switchCurrent(o.Dir, o.Version); err != nil {
		return Result{}, err
	}
	if o.LinkDir != "" {
		res.Linked, res.Skipped, err = linkInto(o.LinkDir, filepath.Join(o.Dir, "current"), m.Kernel != nil)
		if err != nil {
			return Result{}, err
		}
	}
	return res, nil
}

// installedMatches reports whether dir holds the set whose SHA256SUMS is sums, intact. The
// sums in the directory are compared with the ones just downloaded: a set that vouches
// for itself says nothing.
func installedMatches(dir string, sums []byte) bool {
	have, err := os.ReadFile(filepath.Join(dir, SumsFile))
	if err != nil || !bytes.Equal(have, sums) {
		return false
	}
	_, err = Verify(dir)
	return err == nil
}

// consistentWith checks that image.json and SHA256SUMS say the same, so a manifest cannot
// promise a file the checksums do not vouch for.
func (m *Manifest) consistentWith(sums map[string]string) error {
	if m.Schema != 1 {
		return fmt.Errorf("%s has schema %d; this asp reads schema 1: upgrade asp", ManifestFile, m.Schema)
	}
	type entry struct{ name, sum string }
	want := []entry{{RootFSFile, m.Rootfs.SHA256}, {RootFSGzip, m.Rootfs.Gzip.SHA256}}
	if m.Kernel != nil {
		want = append(want, entry{KernelFile, m.Kernel.SHA256})
	}
	for _, e := range want {
		if !hexRe.MatchString(e.sum) || sums[e.name] != e.sum {
			return fmt.Errorf("%s says %s is %q but SHA256SUMS says %q: the release is inconsistent", ManifestFile, e.name, e.sum, sums[e.name])
		}
	}
	if m.Rootfs.Size <= 0 || m.Rootfs.Gzip.Size <= 0 || (m.Kernel != nil && m.Kernel.Size <= 0) {
		return fmt.Errorf("%s has no sizes", ManifestFile)
	}
	return nil
}

func (o *Options) install(ctx context.Context, base string, m Manifest, sums map[string]string, sumsText, manifestBytes []byte, final string) error {
	stage, err := os.MkdirTemp(o.Dir, ".pull-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := os.Chmod(stage, 0o755); err != nil {
		return err
	}

	// The root filesystem: downloaded compressed, checked as it arrives (the compressed
	// bytes against the sum of the .gz, the decompressed ones against the sum of the image),
	// and written sparse: most of the image is zeros.
	o.say("downloading %s (%d MiB compressed)", RootFSFile, m.Rootfs.Gzip.Size>>20)
	if err := o.fetchRootfs(ctx, base+RootFSGzip, filepath.Join(stage, RootFSFile), m, sums); err != nil {
		return err
	}
	if m.Kernel != nil {
		o.say("downloading %s (%d MiB)", KernelFile, m.Kernel.Size>>20)
		if err := o.fetchPlain(ctx, base+KernelFile, filepath.Join(stage, KernelFile), m.Kernel.Size, sums[KernelFile]); err != nil {
			return err
		}
	}
	if err := writeFile(filepath.Join(stage, SumsFile), sumsText, 0o644); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(stage, ManifestFile), manifestBytes, 0o644); err != nil {
		return err
	}
	// All checked: put the set in place. A set that was there and did not verify is replaced.
	if _, err := os.Lstat(final); err == nil {
		old := final + ".old-" + randHex(4)
		if err := os.Rename(final, old); err != nil {
			return err
		}
		defer os.RemoveAll(old)
	}
	if err := os.Rename(stage, final); err != nil {
		return err
	}
	return syncDir(o.Dir)
}

func (o *Options) fetchRootfs(ctx context.Context, url, dst string, m Manifest, sums map[string]string) error {
	body, err := o.get(ctx, url)
	if err != nil {
		return err
	}
	defer body.Close()
	gzHash := sha256.New()
	// One reader, bounded, that hashes what passes: the gzip stream and anything after it.
	src := io.TeeReader(io.LimitReader(body, m.Rootfs.Gzip.Size+1), gzHash)
	zr, err := gzip.NewReader(src)
	if err != nil {
		return fmt.Errorf("%s: %w", RootFSGzip, err)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	imgHash := sha256.New()
	sw := &sparseWriter{f: f}
	// A bomb cannot fill the disk: no more than the manifest's size is read.
	n, err := io.Copy(io.MultiWriter(sw, imgHash), io.LimitReader(zr, m.Rootfs.Size+1))
	if err != nil {
		return fmt.Errorf("%s: %w", RootFSGzip, err)
	}
	if n > m.Rootfs.Size {
		return fmt.Errorf("%w: %s unpacks to more than the %d bytes image.json says", ErrChecksum, RootFSGzip, m.Rootfs.Size)
	}
	// Drain the gzip trailer and anything after it, so the compressed sum covers every byte.
	if _, err := io.Copy(io.Discard, src); err != nil {
		return err
	}
	if got := hex.EncodeToString(gzHash.Sum(nil)); got != sums[RootFSGzip] {
		return fmt.Errorf("%w: %s is %s, SHA256SUMS says %s", ErrChecksum, RootFSGzip, got, sums[RootFSGzip])
	}
	if got := hex.EncodeToString(imgHash.Sum(nil)); got != sums[RootFSFile] || n != m.Rootfs.Size {
		return fmt.Errorf("%w: the unpacked image is %s (%d bytes), SHA256SUMS says %s (%d)", ErrChecksum, got, n, sums[RootFSFile], m.Rootfs.Size)
	}
	if err := sw.Finish(); err != nil {
		return err
	}
	return f.Sync()
}

func (o *Options) fetchPlain(ctx context.Context, url, dst string, size int64, want string) error {
	body, err := o.get(ctx, url)
	if err != nil {
		return err
	}
	defer body.Close()
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(body, size+1))
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want || n != size {
		return fmt.Errorf("%w: %s is %s (%d bytes), SHA256SUMS says %s (%d)", ErrChecksum, filepath.Base(dst), got, n, want, size)
	}
	return f.Sync()
}

func (o *Options) get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

func (o *Options) getSmall(ctx context.Context, url string, max int64) ([]byte, error) {
	body, err := o.get(ctx, url)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	b, err := io.ReadAll(io.LimitReader(body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("GET %s: more than %d bytes", url, max)
	}
	return b, nil
}

// Check is the result of verifying one file.
type Check struct {
	File   string `json:"file"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Verify hashes the files of an installed set against the SHA256SUMS in its directory. Files
// the sums list but the directory does not hold (the compressed copy) are not checked. It
// fails if there is no SHA256SUMS, if nothing was checked, or if a file does not match.
func Verify(dir string) ([]Check, error) {
	b, err := os.ReadFile(filepath.Join(dir, SumsFile))
	if err != nil {
		return nil, err
	}
	sums, err := ParseSums(string(b))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(sums))
	for name := range sums {
		names = append(names, name)
	}
	sort.Strings(names)
	var checks []Check
	failed := false
	for _, name := range names {
		got, err := sumFile(filepath.Join(dir, name))
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue
		case err != nil:
			checks = append(checks, Check{File: name, Detail: err.Error()})
			failed = true
		case got != sums[name]:
			checks = append(checks, Check{File: name, Detail: fmt.Sprintf("is %s, SHA256SUMS says %s", got, sums[name])})
			failed = true
		default:
			checks = append(checks, Check{File: name, OK: true})
		}
	}
	if len(checks) == 0 {
		return nil, fmt.Errorf("none of the files in %s is listed in its SHA256SUMS", dir)
	}
	if failed {
		return checks, ErrChecksum
	}
	return checks, nil
}

func sumFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sumOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// switchCurrent points <dir>/current at the version, atomically.
func switchCurrent(dir, version string) error {
	tmp := filepath.Join(dir, ".current-"+randHex(4))
	if err := os.Symlink(version, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "current")); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(dir)
}

// linkInto makes linkDir/<name> a symlink to <current>/<name> for the files of the set. A file
// that is there and is not a symlink is not touched: it is somebody's.
func linkInto(linkDir, current string, withKernel bool) (linked, skipped []string, err error) {
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		return nil, nil, err
	}
	names := []string{RootFSFile, SumsFile, ManifestFile}
	if withKernel {
		names = append(names, KernelFile)
	}
	for _, name := range names {
		path := filepath.Join(linkDir, name)
		if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink == 0 {
			skipped = append(skipped, path)
			continue
		}
		tmp := filepath.Join(linkDir, ".link-"+randHex(4))
		if err := os.Symlink(filepath.Join(current, name), tmp); err != nil {
			return linked, skipped, err
		}
		if err := os.Rename(tmp, path); err != nil {
			os.Remove(tmp)
			return linked, skipped, err
		}
		linked = append(linked, path)
	}
	return linked, skipped, nil
}

func writeFile(path string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// sparseWriter writes to a file but seeks over the blocks that are all zeros, so a
// 512 MiB image that is mostly empty takes what its content takes.
type sparseWriter struct {
	f   *os.File
	off int64
}

const sparseBlock = 64 << 10

var zeroBlock = make([]byte, sparseBlock)

func (s *sparseWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		chunk := p
		if len(chunk) > sparseBlock {
			chunk = p[:sparseBlock]
		}
		var err error
		if bytes.Equal(chunk, zeroBlock[:len(chunk)]) {
			_, err = s.f.Seek(int64(len(chunk)), io.SeekCurrent)
		} else {
			_, err = s.f.Write(chunk)
		}
		if err != nil {
			return n - len(p), err
		}
		s.off += int64(len(chunk))
		p = p[len(chunk):]
	}
	return n, nil
}

// Finish sets the file's size, which a final hole leaves short.
func (s *sparseWriter) Finish() error { return s.f.Truncate(s.off) }
