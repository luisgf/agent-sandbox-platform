package vmm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceVirtiofsGuestUnit(t *testing.T) {
	root := findRepoRoot(t)
	files := []string{
		"images/guest/systemd/workspace-virtiofs.service",
		"images/guest/openrc/workspace-virtiofs",
		"images/guest/helpers/mount-virtiofs-workspace.sh",
		"scripts/build-guest-image.sh",
		"images/guest/Dockerfile",
	}
	for _, rel := range files {
		p := filepath.Join(root, rel)
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			t.Fatalf("missing %s: %v", rel, err)
		}
	}

	unit, err := os.ReadFile(filepath.Join(root, "images/guest/systemd/workspace-virtiofs.service"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(unit)
	for _, want := range []string{
		"Type=oneshot",
		"ExecStart=/usr/local/share/asp/mount-virtiofs-workspace.sh",
		"WantedBy=multi-user.target",
		"RemainAfterExit=yes",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("unit missing %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "User=sandboxd") {
		t.Fatal("mount unit must run as root, not sandboxd")
	}
	if strings.Contains(body, "RequiredBy=") {
		t.Fatal("unit must not be RequiredBy: a missing tag must not fail boot")
	}

	script, err := os.ReadFile(filepath.Join(root, "images/guest/helpers/mount-virtiofs-workspace.sh"))
	if err != nil {
		t.Fatal(err)
	}
	sbody := string(script)
	for _, want := range []string{"mkdir -p", "mount -t virtiofs", "exit 0"} {
		if !strings.Contains(sbody, want) {
			t.Fatalf("helper missing %q", want)
		}
	}

	df, err := os.ReadFile(filepath.Join(root, "images/guest/Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"workspace-virtiofs.service",
		"mount-virtiofs-workspace.sh",
		"multi-user.target.wants/workspace-virtiofs.service",
	} {
		if !strings.Contains(string(df), want) {
			t.Fatalf("Dockerfile does not install the workspace mount unit: missing %q", want)
		}
	}
}

func TestWorkspaceVirtiofsHelperExitZeroWhenTagAbsent(t *testing.T) {
	root := findRepoRoot(t)
	bin := t.TempDir()
	mountStub := filepath.Join(bin, "mount")
	if err := os.WriteFile(mountStub, []byte("#!/bin/sh\nexit 32\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "workspace")
	cmd := exec.Command(filepath.Join(root, "images/guest/helpers/mount-virtiofs-workspace.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":/usr/bin:/bin",
		"ASP_WORKSPACE_MOUNT="+dest,
		"ASP_VIRTIOFS_TAG=workspace",
		"ASP_VIRTIOFS_MOUNT_TRIES=2",
		"ASP_VIRTIOFS_MOUNT_PAUSE=0",
		// The device is there, its driver or its tag is not ready: the helper retries and then gives up.
		"ASP_VIRTIO_SYSFS="+fakeVirtioDevices(t, virtioIDBlock, virtioIDFS),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper must exit 0 when mount fails (tag absent): %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "no montado") {
		t.Fatalf("expected absent-tag notice, got %q", out)
	}
	st, err := os.Stat(dest)
	if err != nil || !st.IsDir() {
		t.Fatalf("helper should mkdir the mountpoint: %v", err)
	}
}

func TestWorkspaceVirtiofsHelperMountsWhenDevicePresent(t *testing.T) {
	root := findRepoRoot(t)
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "mounted")
	stub := "#!/bin/sh\n" +
		"if [ \"$1\" != \"-t\" ] || [ \"$2\" != \"virtiofs\" ] || [ \"$3\" != \"workspace\" ]; then\n" +
		"  echo \"bad args: $*\" >&2\n  exit 1\nfi\n" +
		"touch \"" + marker + "\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "mount"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "workspace")
	cmd := exec.Command(filepath.Join(root, "images/guest/helpers/mount-virtiofs-workspace.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":/usr/bin:/bin",
		"ASP_WORKSPACE_MOUNT="+dest,
		"ASP_VIRTIOFS_MOUNT_TRIES=1",
		"ASP_VIRTIOFS_MOUNT_PAUSE=0",
		"ASP_VIRTIO_SYSFS="+fakeVirtioDevices(t, virtioIDBlock, virtioIDFS),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("mount was not invoked: %v\n%s", err, out)
	}
}

// The virtio device ids the helper reads from /sys/bus/virtio/devices/*/device.
const (
	virtioIDBlock = "0x0002"
	virtioIDFS    = "0x001a"
)

// fakeVirtioDevices makes a directory shaped like /sys/bus/virtio/devices, one device per id.
func fakeVirtioDevices(t *testing.T, ids ...string) string {
	t.Helper()
	dir := t.TempDir()
	for i, id := range ids {
		dev := filepath.Join(dir, "virtio"+string(rune('0'+i)))
		if err := os.MkdirAll(dev, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dev, "device"), []byte(id+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A sandbox without a workspace has no virtio-fs device. The helper used to try to mount the tag
// eight times, a quarter of a second apart, before giving up: about 1.8 s on every boot of a sandbox
// that was never going to have one (ADR-0017). Without the device it leaves at once, never calls
// mount, and still makes the mount point.
func TestWorkspaceVirtiofsHelperDoesNotWaitWithoutAVirtiofsDevice(t *testing.T) {
	root := findRepoRoot(t)
	for name, devices := range map[string]string{
		"no virtio devices at all": fakeVirtioDevices(t),
		"only a block device":      fakeVirtioDevices(t, virtioIDBlock),
		"no such sysfs":            filepath.Join(t.TempDir(), "missing"),
	} {
		bin := t.TempDir()
		marker := filepath.Join(t.TempDir(), "mount-called")
		stub := "#!/bin/sh\ntouch \"" + marker + "\"\nexit 32\n"
		if err := os.WriteFile(filepath.Join(bin, "mount"), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(t.TempDir(), "workspace")
		cmd := exec.Command(filepath.Join(root, "images/guest/helpers/mount-virtiofs-workspace.sh"))
		cmd.Env = append(os.Environ(),
			"PATH="+bin+":/usr/bin:/bin",
			"ASP_WORKSPACE_MOUNT="+dest,
			// The defaults, which would take two seconds if the helper waited.
			"ASP_VIRTIOFS_MOUNT_TRIES=8",
			"ASP_VIRTIOFS_MOUNT_PAUSE=0.5",
			"ASP_VIRTIO_SYSFS="+devices,
		)
		start := time.Now()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: helper must exit 0: %v\n%s", name, err, out)
		}
		if took := time.Since(start); took > time.Second {
			t.Errorf("%s: the helper took %s: it waited for a device that is not there", name, took)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("%s: mount was called without a virtio-fs device", name)
		}
		if st, err := os.Stat(dest); err != nil || !st.IsDir() {
			t.Errorf("%s: the helper should still make the mount point: %v", name, err)
		}
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := wd
	for i := 0; i < 8; i++ {
		p := filepath.Join(dir, "images", "guest", "systemd", "workspace-virtiofs.service")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("repo root not found from", wd)
	return ""
}
