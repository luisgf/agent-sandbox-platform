package virtiofs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDaemonArgsRustCLI(t *testing.T) {
	args := DaemonArgs("/run/asp/virtiofs-sb.sock", "/data/proj")
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
	argv := append([]string{"/usr/libexec/virtiofsd"}, DaemonArgs("/run/asp/virtiofs-sb.sock", "/data/proj")...)
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
