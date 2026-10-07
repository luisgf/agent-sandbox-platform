package reconciler

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

func expectedFrom(m map[string]string) func(string) (string, bool) {
	return func(path string) (string, bool) { d, ok := m[path]; return d, ok }
}

// The kernel or the base image is not what its SHA256SUMS lists: the sandbox does not boot.
func TestAKernelThatIsNotTheReleasedOneDoesNotBoot(t *testing.T) {
	cp := newFakeCP(t, idA)
	rec, _, _ := measuredRec(t, cp, vmm.NewFakeVMM(nil))
	rec.GuestVerify = GuestVerifyAuto
	rec.Expected = expectedFrom(map[string]string{rec.KernelPath: digestOf('f')}) // measured as 'e'
	rec.tick(context.Background())
	state, detail := cp.state(idA)
	if state != "failed" || !strings.Contains(detail, "SHA256SUMS says "+digestOf('f')) || !strings.Contains(detail, rec.KernelPath) {
		t.Fatalf("state=%s detail=%q", state, detail)
	}
}

func TestABaseImageThatIsNotTheReleasedOneIsNotCopied(t *testing.T) {
	cp := newFakeCP(t, idA)
	rec, disks, _ := measuredRec(t, cp, vmm.NewFakeVMM(nil))
	rec.GuestVerify = GuestVerifyAuto
	rec.Expected = expectedFrom(map[string]string{rec.RootFSPath: digestOf('f')}) // measured as 'a'
	rec.tick(context.Background())
	state, detail := cp.state(idA)
	if state != "failed" || !strings.Contains(detail, "base image") {
		t.Fatalf("state=%s detail=%q", state, detail)
	}
	if _, err := os.Stat(diskOf(disks, idA)); err == nil {
		t.Errorf("a disk was made from an image that does not match")
	}
}

// What matches boots; so does what no SHA256SUMS lists, unless --guest-verify=on asks for one;
// and off checks nothing.
func TestGuestVerifyModes(t *testing.T) {
	for _, c := range []struct {
		name     string
		mode     string
		expected func(rec *Reconciler) map[string]string
		want     string
	}{
		{"matching sums", GuestVerifyAuto, func(r *Reconciler) map[string]string {
			return map[string]string{r.KernelPath: digestOf('e'), r.RootFSPath: digestOf('a')}
		}, "running"},
		{"no sums, auto", GuestVerifyAuto, func(*Reconciler) map[string]string { return nil }, "running"},
		{"no sums, on", GuestVerifyOn, func(*Reconciler) map[string]string { return nil }, "failed"},
		{"matching sums, on", GuestVerifyOn, func(r *Reconciler) map[string]string {
			return map[string]string{r.KernelPath: digestOf('e'), r.RootFSPath: digestOf('a')}
		}, "running"},
		{"wrong sums, off", GuestVerifyOff, func(r *Reconciler) map[string]string {
			return map[string]string{r.KernelPath: digestOf('f')}
		}, "running"},
		{"wrong sums, unset", "", func(r *Reconciler) map[string]string {
			return map[string]string{r.KernelPath: digestOf('f')}
		}, "running"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cp := newFakeCP(t, idA)
			rec, _, _ := measuredRec(t, cp, vmm.NewFakeVMM(nil))
			rec.GuestVerify = c.mode
			rec.Expected = expectedFrom(c.expected(rec))
			rec.tick(context.Background())
			if got, detail := cp.state(idA); got != c.want {
				t.Fatalf("state=%s (%s), want %s", got, detail, c.want)
			}
		})
	}
}

// A file that cannot be read to check it is not booted from either.
func TestAnUnreadableGuestFileDoesNotBootWhenItHasSums(t *testing.T) {
	cp := newFakeCP(t, idA)
	rec, _, fm := measuredRec(t, cp, vmm.NewFakeVMM(nil))
	rec.GuestVerify = GuestVerifyAuto
	rec.Expected = expectedFrom(map[string]string{rec.KernelPath: digestOf('e')})
	fm.err = errors.New("read error")
	rec.tick(context.Background())
	if state, _ := cp.state(idA); state != "failed" {
		t.Fatalf("state=%s", state)
	}
}
