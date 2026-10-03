package vmm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceVirtiofsGuestUnit(t *testing.T) {
	root := findRepoRoot(t)
	files := []string{
		"images/guest/systemd/workspace-virtiofs.service",
		"images/guest/openrc/workspace-virtiofs",
		"images/guest/helpers/mount-virtiofs-workspace.sh",
		"scripts/build-guest-rootfs.sh",
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

	build, err := os.ReadFile(filepath.Join(root, "scripts/build-guest-rootfs.sh"))
	if err != nil {
		t.Fatal(err)
	}
	bb := string(build)
	for _, want := range []string{
		"workspace-virtiofs.service",
		"mount-virtiofs-workspace.sh",
		"multi-user.target.wants/workspace-virtiofs.service",
	} {
		if !strings.Contains(bb, want) {
			t.Fatalf("build-guest-rootfs.sh missing %q", want)
		}
	}

	df, err := os.ReadFile(filepath.Join(root, "images/guest/Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(df), "workspace-virtiofs.service") ||
		!strings.Contains(string(df), "mount-virtiofs-workspace.sh") {
		t.Fatal("Dockerfile does not install the workspace mount unit")
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
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("mount was not invoked: %v\n%s", err, out)
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
