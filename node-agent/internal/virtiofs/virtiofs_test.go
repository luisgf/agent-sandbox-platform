package virtiofs

import (
	"context"
	"errors"
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
