package hostvsock

import (
	"os"
	"path/filepath"
	"testing"
)

// The sockets of a sandbox grant authority over it (the identity socket mints
// its tokens), so only the node-agent's user may connect to them.
func TestListenersArePrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	ln, err := UnixFactory{Dir: dir}.Listen(26501)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	hy, err := listenUnix(filepath.Join(dir, "vsock-x.sock_26502"))
	if err != nil {
		t.Fatal(err)
	}
	defer hy.Close()

	for _, p := range []string{
		dir,
		filepath.Join(dir, "host-vsock-26501.sock"),
		filepath.Join(dir, "vsock-x.sock_26502"),
	} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got&0o077 != 0 {
			t.Errorf("%s has mode %o: group or others can reach it", p, got)
		}
	}
}
