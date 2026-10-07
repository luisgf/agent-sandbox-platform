package imagepull

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// release is a guest release the tests serve: a rootfs mostly zeros, a kernel, and the
// SHA256SUMS and image.json that describe them.
type release struct {
	rootfs, rootfsGz, kernel []byte
	sums, manifest           []byte
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func gz(t testing.TB, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newRelease(t testing.TB) release {
	t.Helper()
	rng := rand.New(rand.NewSource(1))
	rootfs := make([]byte, 3<<20)       // zeros...
	rng.Read(rootfs[64<<10 : 200<<10])  // ...with some content at the start
	rng.Read(rootfs[2<<20 : 2<<20+777]) // ...and a little in the middle
	kernel := make([]byte, 100<<10)
	rng.Read(kernel)
	r := release{rootfs: rootfs, rootfsGz: gz(t, rootfs), kernel: kernel}
	r.build()
	return r
}

// build writes SHA256SUMS and image.json from the files.
func (r *release) build() {
	r.manifest = []byte(fmt.Sprintf(`{"schema":1,"version":"1.0","commit":"abc","source_date_epoch":1,
 "base":{"image":"debian@sha256:x","snapshot":"20261005T000000Z"},
 "rootfs":{"file":"rootfs.img","sha256":%q,"size":%d,"gzip":{"file":"rootfs.img.gz","sha256":%q,"size":%d}},
 "kernel":{"file":"vmlinux","sha256":%q,"size":%d},"packages":[{"name":"systemd","version":"252"}]}`,
		hexSum(r.rootfs), len(r.rootfs), hexSum(r.rootfsGz), len(r.rootfsGz), hexSum(r.kernel), len(r.kernel)))
	r.sums = []byte(fmt.Sprintf("%s  rootfs.img\n%s  rootfs.img.gz\n%s  image.json\n%s  vmlinux\n",
		hexSum(r.rootfs), hexSum(r.rootfsGz), hexSum(r.manifest), hexSum(r.kernel)))
}

// serve serves the release under /rel/asp-guest_1.0_<file>, counting requests per file.
func serve(t *testing.T, r release, tamper func(name string, body []byte) []byte) (*httptest.Server, func(string) int) {
	t.Helper()
	files := map[string][]byte{
		"SHA256SUMS": r.sums, "image.json": r.manifest, "rootfs.img.gz": r.rootfsGz, "vmlinux": r.kernel,
	}
	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		name := strings.TrimPrefix(req.URL.Path, "/rel/asp-guest_1.0_")
		body, ok := files[name]
		if !ok || !strings.HasPrefix(req.URL.Path, "/rel/asp-guest_1.0_") {
			http.NotFound(w, req)
			return
		}
		mu.Lock()
		hits[name]++
		mu.Unlock()
		if tamper != nil {
			body = tamper(name, append([]byte(nil), body...))
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, func(n string) int { mu.Lock(); defer mu.Unlock(); return hits[n] }
}

func opts(srv *httptest.Server, dir string) Options {
	return Options{Version: "1.0", BaseURL: srv.URL + "/rel", Prefix: "asp-guest_1.0_", Dir: dir, HTTP: srv.Client()}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPullInstallsAVerifiedSet(t *testing.T) {
	r := newRelease(t)
	srv, _ := serve(t, r, nil)
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "opt-sandbox")
	o := opts(srv, dir)
	o.LinkDir = link
	res, err := Pull(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.AlreadyInstalled || res.Rootfs != filepath.Join(dir, "1.0", "rootfs.img") || res.Kernel != filepath.Join(dir, "1.0", "vmlinux") {
		t.Fatalf("result: %+v", res)
	}
	if !bytes.Equal(mustRead(t, res.Rootfs), r.rootfs) {
		t.Fatal("the installed rootfs is not the one served")
	}
	if !bytes.Equal(mustRead(t, res.Kernel), r.kernel) {
		t.Fatal("the installed kernel is not the one served")
	}
	if !bytes.Equal(mustRead(t, filepath.Join(dir, "1.0", "SHA256SUMS")), r.sums) || !bytes.Equal(mustRead(t, filepath.Join(dir, "1.0", "image.json")), r.manifest) {
		t.Fatal("SHA256SUMS and image.json are kept with the files")
	}
	if got, err := os.Readlink(filepath.Join(dir, "current")); err != nil || got != "1.0" {
		t.Fatalf("current -> %q, %v", got, err)
	}
	// What the node-agent reads by default, through the link.
	for _, name := range []string{"rootfs.img", "vmlinux", "SHA256SUMS", "image.json"} {
		if _, err := os.Stat(filepath.Join(link, name)); err != nil {
			t.Errorf("%s is not linked: %v", name, err)
		}
	}
	if !bytes.Equal(mustRead(t, filepath.Join(link, "rootfs.img")), r.rootfs) {
		t.Fatal("the link does not lead to the rootfs")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*")); len(left) != 0 {
		t.Errorf("leftovers: %v", left)
	}
	if checks, err := Verify(filepath.Join(dir, "current")); err != nil || len(checks) != 3 {
		t.Fatalf("verify: %+v %v", checks, err)
	}
}

// A release whose files do not match the checksums installs nothing: not a file, not the
// current link, not a leftover.
func TestPullRefusesWhatDoesNotMatch(t *testing.T) {
	r := newRelease(t)
	for _, c := range []struct {
		name   string
		tamper func(name string, body []byte) []byte
		want   string
	}{
		{"a flipped byte in the compressed image", func(n string, b []byte) []byte {
			if n == "rootfs.img.gz" {
				b[len(b)/2] ^= 0xff
			}
			return b
		}, ""},
		{"another stream of the same length", func(n string, b []byte) []byte {
			if n == "rootfs.img.gz" {
				rand.New(rand.NewSource(99)).Read(b)
			}
			return b
		}, ""},
		{"a kernel that is not the listed one", func(n string, b []byte) []byte {
			if n == "vmlinux" {
				b[10] ^= 1
			}
			return b
		}, "vmlinux"},
		{"an image.json that is not the listed one", func(n string, b []byte) []byte {
			if n == "image.json" {
				return bytes.Replace(b, []byte(`"version":"1.0"`), []byte(`"version":"6.6"`), 1)
			}
			return b
		}, "image.json"},
		{"a truncated kernel", func(n string, b []byte) []byte {
			if n == "vmlinux" {
				return b[:len(b)-1]
			}
			return b
		}, "vmlinux"},
		{"more bytes after the compressed image", func(n string, b []byte) []byte {
			if n == "rootfs.img.gz" {
				return append(b, 0)
			}
			return b
		}, "rootfs.img.gz"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := serve(t, r, c.tamper)
			dir := t.TempDir()
			_, err := Pull(context.Background(), opts(srv, dir))
			if err == nil {
				t.Fatal("pull accepted it")
			}
			if c.want != "" && !errors.Is(err, ErrChecksum) && !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not name %s", err, c.want)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				var names []string
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Errorf("a refused pull left %v in the directory", names)
			}
		})
	}
}

func TestPullRefusesAnInconsistentRelease(t *testing.T) {
	r := newRelease(t)
	// image.json promises a rootfs the checksums do not list.
	bad := r
	bad.manifest = bytes.Replace(r.manifest, []byte(hexSum(r.rootfs)), []byte(strings.Repeat("a", 64)), 1)
	bad.sums = []byte(fmt.Sprintf("%s  rootfs.img\n%s  rootfs.img.gz\n%s  image.json\n%s  vmlinux\n",
		hexSum(r.rootfs), hexSum(r.rootfsGz), hexSum(bad.manifest), hexSum(r.kernel)))
	srv, _ := serve(t, bad, nil)
	if _, err := Pull(context.Background(), opts(srv, t.TempDir())); err == nil || !strings.Contains(err.Error(), "inconsistent") {
		t.Fatalf("want an inconsistent release, got %v", err)
	}
	// A schema this asp does not know.
	newer := r
	newer.manifest = bytes.Replace(r.manifest, []byte(`"schema":1`), []byte(`"schema":2`), 1)
	newer.sums = []byte(fmt.Sprintf("%s  rootfs.img\n%s  rootfs.img.gz\n%s  image.json\n%s  vmlinux\n",
		hexSum(r.rootfs), hexSum(r.rootfsGz), hexSum(newer.manifest), hexSum(r.kernel)))
	srv2, _ := serve(t, newer, nil)
	if _, err := Pull(context.Background(), opts(srv2, t.TempDir())); err == nil || !strings.Contains(err.Error(), "schema 2") {
		t.Fatalf("want a schema error, got %v", err)
	}
}

// A compressed image that unpacks to more than image.json says is stopped at that size.
func TestPullStopsADecompressionBomb(t *testing.T) {
	r := newRelease(t)
	big := bytes.Repeat([]byte{0}, 40<<20)
	bomb := r
	bomb.rootfs, bomb.rootfsGz = big, gz(t, big)
	bomb.build() // sums that are right for the bomb...
	bomb.manifest = bytes.Replace(bomb.manifest, []byte(fmt.Sprintf(`"size":%d,"gzip"`, len(big))), []byte(fmt.Sprintf(`"size":%d,"gzip"`, 1<<20)), 1)
	bomb.sums = []byte(fmt.Sprintf("%s  rootfs.img\n%s  rootfs.img.gz\n%s  image.json\n%s  vmlinux\n",
		hexSum(big), hexSum(bomb.rootfsGz), hexSum(bomb.manifest), hexSum(bomb.kernel))) // ...with a manifest that promises 1 MiB
	srv, _ := serve(t, bomb, nil)
	dir := t.TempDir()
	_, err := Pull(context.Background(), opts(srv, dir))
	if err == nil || !errors.Is(err, ErrChecksum) {
		t.Fatalf("want a checksum error for more than the promised size, got %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left %d entries", len(entries))
	}
}

func TestPullSkipsWhatIsInstalledAndRepairsWhatIsNot(t *testing.T) {
	r := newRelease(t)
	srv, hits := serve(t, r, nil)
	dir := t.TempDir()
	if _, err := Pull(context.Background(), opts(srv, dir)); err != nil {
		t.Fatal(err)
	}
	res, err := Pull(context.Background(), opts(srv, dir))
	if err != nil || !res.AlreadyInstalled {
		t.Fatalf("second pull: %+v %v", res, err)
	}
	if hits("rootfs.img.gz") != 1 || hits("vmlinux") != 1 {
		t.Fatalf("the files were downloaded again: gz %d, vmlinux %d", hits("rootfs.img.gz"), hits("vmlinux"))
	}
	// A file changed on disk: the set is not what it says, and is replaced.
	if err := os.WriteFile(filepath.Join(dir, "1.0", "vmlinux"), []byte("not a kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = Pull(context.Background(), opts(srv, dir))
	if err != nil || res.AlreadyInstalled {
		t.Fatalf("pull over a broken set: %+v %v", res, err)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(dir, "1.0", "vmlinux")), r.kernel) {
		t.Fatal("the kernel was not restored")
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, "1.0.old-*")); len(entries) != 0 {
		t.Errorf("the replaced set was not removed: %v", entries)
	}
	// SHA256SUMS in the directory is not taken on its word: a set that lists a different
	// image is not "already installed" for a release that lists this one.
	if err := os.WriteFile(filepath.Join(dir, "1.0", "SHA256SUMS"), []byte(strings.Repeat("0", 64)+"  vmlinux\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, err := Pull(context.Background(), opts(srv, dir)); err != nil || res.AlreadyInstalled {
		t.Fatalf("a set with other sums: %+v %v", res, err)
	}
}

func TestPullLeavesAFileThatIsNotALinkAlone(t *testing.T) {
	r := newRelease(t)
	srv, _ := serve(t, r, nil)
	link := t.TempDir()
	mine := filepath.Join(link, "rootfs.img")
	if err := os.WriteFile(mine, []byte("my own image"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An older link is replaced.
	if err := os.Symlink("/nonexistent/old", filepath.Join(link, "vmlinux")); err != nil {
		t.Fatal(err)
	}
	o := opts(srv, t.TempDir())
	o.LinkDir = link
	res, err := Pull(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if string(mustRead(t, mine)) != "my own image" {
		t.Fatal("a regular file was overwritten")
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != mine {
		t.Errorf("skipped: %v", res.Skipped)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(link, "vmlinux")), r.kernel) {
		t.Error("the old link was not replaced")
	}
}

func TestPullChecksTheVersionName(t *testing.T) {
	for _, v := range []string{"", "../x", "a/b", ".hidden", "v 1", strings.Repeat("a", 65)} {
		if _, err := Pull(context.Background(), Options{Version: v, BaseURL: "http://x", Dir: t.TempDir()}); err == nil {
			t.Errorf("version %q accepted", v)
		}
	}
}

func TestPullReportsAServerError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if _, err := Pull(context.Background(), opts(srv, t.TempDir())); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("want a 404, got %v", err)
	}
}

func TestParseSums(t *testing.T) {
	h := strings.Repeat("ab", 32)
	got, err := ParseSums(h + "  a.img\n" + h + " *b.bin\n\n")
	if err != nil || got["a.img"] != h || got["b.bin"] != h {
		t.Fatalf("got %v %v", got, err)
	}
	for name, text := range map[string]string{
		"empty":         "",
		"a short sum":   "abc  a.img\n",
		"upper case":    strings.ToUpper(h) + "  a.img\n",
		"a path":        h + "  ../a.img\n",
		"a directory":   h + "  d/a.img\n",
		"no name":       h + "  \n",
		"two sums":      h + "  a\n" + strings.Repeat("cd", 32) + "  a\n",
		"not a sum":     "hello world\n",
		"a dot":         h + "  .\n",
		"dot dot":       h + "  ..\n",
		"only comments": "# nothing\n",
	} {
		if _, err := ParseSums(text); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestVerifyFindsWhatChanged(t *testing.T) {
	dir := t.TempDir()
	good := []byte("kernel bytes")
	sums := hexSum(good) + "  vmlinux\n" + hexSum([]byte("x")) + "  rootfs.img.gz\n" // the .gz is not installed
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(dir); err == nil {
		t.Fatal("no listed file is there: want an error")
	}
	if err := os.WriteFile(filepath.Join(dir, "vmlinux"), good, 0o644); err != nil {
		t.Fatal(err)
	}
	if checks, err := Verify(dir); err != nil || len(checks) != 1 || !checks[0].OK {
		t.Fatalf("intact: %+v %v", checks, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vmlinux"), []byte("kernel bytez"), 0o644); err != nil {
		t.Fatal(err)
	}
	checks, err := Verify(dir)
	if !errors.Is(err, ErrChecksum) || len(checks) != 1 || checks[0].OK {
		t.Fatalf("changed: %+v %v", checks, err)
	}
	if _, err := Verify(t.TempDir()); err == nil {
		t.Fatal("a directory with no SHA256SUMS verifies")
	}
}

func TestSparseWriterKeepsContentAndSize(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "img"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rng := rand.New(rand.NewSource(7))
	var want []byte
	sw := &sparseWriter{f: f}
	// content, a hole of several blocks, content, a hole at the end; written in odd pieces
	for _, seg := range []struct {
		n    int
		zero bool
	}{{100, false}, {300 << 10, true}, {70 << 10, false}, {1 << 20, true}} {
		b := make([]byte, seg.n)
		if !seg.zero {
			rng.Read(b)
		}
		want = append(want, b...)
		for off := 0; off < len(b); off += 9999 {
			end := off + 9999
			if end > len(b) {
				end = len(b)
			}
			if _, err := sw.Write(b[off:end]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := sw.Finish(); err != nil {
		t.Fatal(err)
	}
	got := mustRead(t, f.Name())
	if !bytes.Equal(got, want) {
		t.Fatalf("content differs (len %d vs %d)", len(got), len(want))
	}
}
