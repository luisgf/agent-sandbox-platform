package virtiofs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/unit"
)

func TestDaemonArgsRustCLI(t *testing.T) {
	args := DaemonArgs("/run/asp/virtiofs-sb.sock", "/data/proj", "")
	got := strings.Join(args, " ")
	for _, want := range []string{
		"--socket-path /run/asp/virtiofs-sb.sock",
		"--shared-dir /data/proj",
		"--cache never",
		"--sandbox none",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("args %q missing %q", got, want)
		}
	}
}

func TestSocketPathOfDaemonArgs(t *testing.T) {
	argv := append([]string{"/usr/libexec/virtiofsd"}, DaemonArgs("/run/asp/virtiofs-sb.sock", "/data/proj", "chroot")...)
	if got, ok := SocketPathOf(argv); !ok || got != "/run/asp/virtiofs-sb.sock" {
		t.Fatalf("SocketPathOf(%q) = %q %v", argv, got, ok)
	}
	if got, ok := SocketPathOf([]string{"cloud-hypervisor", "--api-socket", "/run/asp/ch-sb.sock"}); ok {
		t.Fatalf("a CH argv names socket %q", got)
	}
}

func TestStartMissingBinary(t *testing.T) {
	_, err := Start(context.Background(), Config{
		Binary:     "asp-virtiofsd-does-not-exist",
		SocketPath: t.TempDir() + "/v.sock",
		SharedDir:  t.TempDir(),
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
}

// fakeVirtiofsd writes a virtiofsd stand-in: like the real one, it writes
// {socket}.pid next to its socket. body runs after that, with $sock set.
func fakeVirtiofsd(t *testing.T, body string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "virtiofsd")
	script := "#!/bin/sh\n" +
		"sock=$2\n" + // DaemonArgs: --socket-path {sock} ...
		"echo $$ > \"$sock.pid\"\n" +
		body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestStopRemovesSocketAndPIDFile(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "virtiofs-sb.sock")
	stop, err := Start(context.Background(), Config{
		Binary:     fakeVirtiofsd(t, `: > "$sock"; exec sleep 30`),
		SocketPath: sock,
		SharedDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sock + PIDFileSuffix); err != nil {
		t.Fatalf("pid file while virtiofsd runs: %v", err)
	}
	stop()
	for _, p := range []string{sock, sock + PIDFileSuffix} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s is still there after stop (%v)", p, err)
		}
	}
	stop() // a second call finds nothing to do
}

func TestStartRemovesPIDFileOfADaemonThatExits(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "virtiofs-sb.sock")
	_, err := Start(context.Background(), Config{
		Binary:     fakeVirtiofsd(t, "exit 1"),
		SocketPath: sock,
		SharedDir:  t.TempDir(),
	})
	if err == nil {
		t.Fatal("Start succeeded without a socket")
	}
	if _, err := os.Stat(sock + PIDFileSuffix); !os.IsNotExist(err) {
		t.Fatalf("pid file of an exited virtiofsd is still there (%v)", err)
	}
}

func TestDaemonArgsSandboxModes(t *testing.T) {
	for mode, want := range map[string]string{"": "--sandbox none", "none": "--sandbox none", "chroot": "--sandbox chroot", "namespace": "--sandbox namespace"} {
		got := strings.Join(DaemonArgs("/run/asp/v.sock", "/data/proj", mode), " ")
		if !strings.Contains(got, want) || strings.Count(got, "--sandbox") != 1 {
			t.Errorf("mode %q: %q, want it to contain %q once", mode, got, want)
		}
	}
	for _, ok := range []string{"", "none", "chroot", "namespace"} {
		if !ValidSandbox(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	if ValidSandbox("pivot") {
		t.Error("an unknown mode was accepted")
	}
	_, err := Start(context.Background(), Config{Binary: "true", SocketPath: t.TempDir() + "/v.sock", SharedDir: t.TempDir(), Sandbox: "pivot"})
	if err == nil || !strings.Contains(err.Error(), "unknown sandbox mode") {
		t.Fatalf("Start with an unknown mode: %v", err)
	}
}

// With a Launcher the daemon runs through systemd-run, in the unit the Config
// names, and stop kills that unit rather than a child.
func TestStartInATransientUnit(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "virtiofs-sb.sock")
	run := filepath.Join(dir, "systemd-run")
	ctl := filepath.Join(dir, "systemctl")
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// systemd-run records its arguments and runs the command after "--", as the
	// unit would; systemctl records that it was asked to kill and does it.
	write(run, "#!/bin/sh\necho \"$@\" > \""+dir+"/run.log\"\nwhile [ \"$1\" != \"--\" ]; do shift; done\nshift\nexec \"$@\"\n")
	write(ctl, "#!/bin/sh\necho \"$@\" >> \""+dir+"/ctl.log\"\n[ \"$1\" = kill ] && kill -9 \"$(cat \""+sock+".pid\")\"\nexit 0\n")

	stop, err := Start(context.Background(), Config{
		Binary:     fakeVirtiofsd(t, `: > "$sock"; exec sleep 30`),
		SocketPath: sock,
		SharedDir:  t.TempDir(),
		Sandbox:    SandboxChroot,
		Launcher:   &unit.Launcher{SystemdRun: run, Systemctl: ctl},
		Unit:       unit.Spec{Name: "asp-vm-sb-fs", Slice: "asp-vms.slice", MemoryMax: 512 << 20, TasksMax: 256},
	})
	if err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "run.log"))
	for _, want := range []string{"--unit=asp-vm-sb-fs", "--slice=asp-vms.slice", "--property=MemoryMax=536870912", "--property=TasksMax=256", "--sandbox chroot", "--socket-path " + sock} {
		if !strings.Contains(string(log), want) {
			t.Errorf("systemd-run saw %q, lacks %q", log, want)
		}
	}
	stop()
	got, _ := os.ReadFile(filepath.Join(dir, "ctl.log"))
	if !strings.Contains(string(got), "kill --signal=SIGKILL --kill-whom=all asp-vm-sb-fs.service") {
		t.Fatalf("stop did not kill the unit: %q", got)
	}
	for _, p := range []string{sock, sock + PIDFileSuffix} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s is still there after stop (%v)", p, err)
		}
	}
}

// `apt install virtiofsd` leaves the Rust daemon in /usr/libexec, off PATH: the default name finds it
// there, and a name the operator chose is never second-guessed.
func TestResolveFindsTheDistributionsVirtiofsd(t *testing.T) {
	dir := t.TempDir()
	onPath := filepath.Join(dir, "bin")
	if err := os.Mkdir(onPath, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", onPath)
	libexec := filepath.Join(dir, "virtiofsd")
	old := DistroPath
	DistroPath = libexec
	t.Cleanup(func() { DistroPath = old })

	if got := Resolve(""); got != DefaultBinary {
		t.Errorf("nothing installed: got %q, want the plain name so that the error names it", got)
	}
	if err := os.WriteFile(libexec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Resolve(""); got != DefaultBinary {
		t.Errorf("a file that cannot run: got %q", got)
	}
	if err := os.Chmod(libexec, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Resolve(DefaultBinary); got != libexec {
		t.Errorf("in libexec: got %q, want %q", got, libexec)
	}
	if got := Resolve("/opt/mine/virtiofsd"); got != "/opt/mine/virtiofsd" {
		t.Errorf("a chosen path is kept: got %q", got)
	}
	onPathBin := filepath.Join(onPath, "virtiofsd")
	if err := os.WriteFile(onPathBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Resolve(""); got != onPathBin {
		t.Errorf("on PATH wins: got %q, want %q", got, onPathBin)
	}
}
