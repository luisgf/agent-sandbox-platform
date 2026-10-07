package reconciler

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/attest"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

func digestOf(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

// fakeMeasure is a Measure whose answers a test can change, and that records
// what it was asked for.
type fakeMeasure struct {
	mu     sync.Mutex
	sums   map[string]string
	err    error
	called []string
}

func (f *fakeMeasure) measure(path string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called = append(f.called, path)
	if f.err != nil {
		return "", f.err
	}
	if s, ok := f.sums[path]; ok {
		return s, nil
	}
	return "", os.ErrNotExist
}

func (f *fakeMeasure) set(path, sum string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sums[path] = sum
}

// measuredRec is retainRec with a signer, a kernel, a measurer and a hypervisor
// version.
func measuredRec(t *testing.T, cp *fakeCP, eng vmm.MicroVM) (*Reconciler, string, *fakeMeasure) {
	t.Helper()
	rec, disks := retainRec(t, cp, eng)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rec.Attest = []*attest.Signer{attest.NewSigner(key)}
	rec.KernelPath = "/opt/test/vmlinux"
	fm := &fakeMeasure{sums: map[string]string{rec.KernelPath: digestOf('e'), rec.RootFSPath: digestOf('a')}}
	rec.Measure = fm.measure
	rec.VMMVersion = "cloud-hypervisor v43.0"
	return rec, disks, fm
}

// A first boot says which kernel it loaded and which base image its disk was
// copied from, not the name the sandbox was created with.
func TestFirstBootAttestsTheMeasuredKernelAndBaseImage(t *testing.T) {
	cp := newFakeCP(t, idA)
	rec, disks, fm := measuredRec(t, cp, vmm.NewFakeVMM(nil))
	rec.tick(context.Background())

	st, ok := cp.lastAttest(idA)
	if !ok {
		t.Fatal("no attestation posted")
	}
	if st.ImageDigest != digestOf('a') || st.KernelDigest != digestOf('e') ||
		st.VMMVersion != "cloud-hypervisor v43.0" || st.Boot != "new" {
		t.Fatalf("statement: %+v", st)
	}
	// The image was hashed before the copy was made, and the record sits next to
	// the disk for a resume to read.
	if b, err := os.ReadFile(baseDigestPath(diskOf(disks, idA))); err != nil || strings.TrimSpace(string(b)) != digestOf('a') {
		t.Fatalf("digest record: %q %v", b, err)
	}
	if len(fm.called) < 2 {
		t.Fatalf("measured %v", fm.called)
	}
}

// A resume boots a disk the guest has written to. The statement names the base
// image that disk came from, even when the node's image has been replaced since.
func TestResumeAttestsTheImageTheDiskCameFrom(t *testing.T) {
	cp := newFakeCP(t, idA)
	rec, _, fm := measuredRec(t, cp, vmm.NewFakeVMM(nil))
	rec.tick(context.Background())

	cp.setState(idA, "stopping")
	rec.tick(context.Background())

	fm.set(rec.RootFSPath, digestOf('b')) // the base image was rebuilt
	fm.set(rec.KernelPath, digestOf('c')) // and so was the kernel
	cp.mu.Lock()
	cp.boxes[idA].State, cp.boxes[idA].BootCount = "requested", 2
	cp.mu.Unlock()
	rec.tick(context.Background())

	st, _ := cp.lastAttest(idA)
	if st.Boot != "resume" {
		t.Fatalf("boot=%q, want resume", st.Boot)
	}
	if st.ImageDigest != digestOf('a') {
		t.Fatalf("image digest %q: the resume named the node's current image, not the one the disk came from", st.ImageDigest)
	}
	if st.KernelDigest != digestOf('c') {
		t.Fatalf("kernel digest %q: the kernel is loaded at every boot, so the current one is the one that booted", st.KernelDigest)
	}
}

// A retained disk with no record (made before records existed) is resumed, and
// the statement does not claim an image for it.
func TestResumeWithoutARecordAttestsNoImage(t *testing.T) {
	cp := newFakeCP(t, idA)
	rec, disks, _ := measuredRec(t, cp, vmm.NewFakeVMM(nil))
	rec.tick(context.Background())
	cp.setState(idA, "stopping")
	rec.tick(context.Background())
	if err := os.Remove(baseDigestPath(diskOf(disks, idA))); err != nil {
		t.Fatal(err)
	}
	cp.mu.Lock()
	cp.boxes[idA].State, cp.boxes[idA].BootCount = "requested", 2
	cp.mu.Unlock()
	rec.tick(context.Background())

	if got, _ := cp.state(idA); got != "running" {
		t.Fatalf("state=%s: a missing record must not stop a resume", got)
	}
	st, _ := cp.lastAttest(idA)
	if st.ImageDigest != "" || st.Boot != "resume" || st.KernelDigest == "" {
		t.Fatalf("statement: %+v", st)
	}
}

// A file that cannot be read does not keep the sandbox from booting: the
// statement says less.
func TestUnmeasurableFilesLeaveTheStatementUnmeasured(t *testing.T) {
	cp := newFakeCP(t, idA)
	rec, disks, fm := measuredRec(t, cp, vmm.NewFakeVMM(nil))
	fm.err = errors.New("permission denied")
	rec.tick(context.Background())

	if got, _ := cp.state(idA); got != "running" {
		t.Fatalf("state=%s: a measurement error must not fail a start", got)
	}
	st, ok := cp.lastAttest(idA)
	if !ok {
		t.Fatal("no attestation posted")
	}
	if st.ImageDigest != "" || st.KernelDigest != "" || st.Boot != "new" {
		t.Fatalf("statement: %+v", st)
	}
	if exists(baseDigestPath(diskOf(disks, idA))) {
		t.Fatal("a digest record was written for an image that was not measured")
	}
}

// Without a measurer (dry-run) the statement carries no digests at all.
func TestNoMeasurerNoDigests(t *testing.T) {
	cp := newFakeCP(t, idA)
	rec, _, _ := measuredRec(t, cp, vmm.NewFakeVMM(nil))
	rec.Measure, rec.VMMVersion = nil, ""
	rec.tick(context.Background())
	st, ok := cp.lastAttest(idA)
	if !ok || st.ImageDigest != "" || st.KernelDigest != "" || st.VMMVersion != "" {
		t.Fatalf("statement: %+v (posted %v)", st, ok)
	}
}

// The digest record is removed with the disk, and one left behind is swept.
func TestDigestRecordsDieWithTheirDisks(t *testing.T) {
	cp := newFakeCP(t, idA, idB)
	rec, disks, _ := measuredRec(t, cp, vmm.NewFakeVMM(nil))
	rec.tick(context.Background())
	for _, id := range []string{idA, idB} {
		if !exists(baseDigestPath(diskOf(disks, id))) {
			t.Fatalf("%s: no digest record after the first boot", id)
		}
	}
	cp.setState(idA, "deleting")
	rec.tick(context.Background())
	if exists(diskOf(disks, idA)) || exists(baseDigestPath(diskOf(disks, idA))) {
		t.Fatal("deleting a sandbox left its disk or digest record")
	}
	if !exists(baseDigestPath(diskOf(disks, idB))) {
		t.Fatal("deleting one sandbox removed another's digest record")
	}

	// A record whose disk is gone (a crash between the two removals) is swept
	// by the disk GC, and a record whose disk is there is left alone.
	orphan := baseDigestPath(diskOf(disks, idC))
	if err := os.WriteFile(orphan, []byte(digestOf('a')+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	removeOrphanBaseDigests(disks)
	if exists(orphan) {
		t.Fatal("an orphan digest record survived the sweep")
	}
	if !exists(baseDigestPath(diskOf(disks, idB))) {
		t.Fatal("the sweep removed the record of a disk that exists")
	}
}

func TestReadBaseDigestIgnoresJunk(t *testing.T) {
	dir := t.TempDir()
	disk := dir + "/rootfs-x.img"
	if got := readBaseDigest(disk); got != "" {
		t.Fatalf("no record: %q", got)
	}
	for _, junk := range []string{"", "not a digest\n", "sha256:abc\n", digestOf('a') + "extra\n"} {
		if err := os.WriteFile(baseDigestPath(disk), []byte(junk), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := readBaseDigest(disk); got != "" {
			t.Errorf("record %q read as %q", junk, got)
		}
	}
	if err := writeBaseDigest(disk, digestOf('d')); err != nil {
		t.Fatal(err)
	}
	if got := readBaseDigest(disk); got != digestOf('d') {
		t.Fatalf("round trip: %q", got)
	}
}
