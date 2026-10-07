// Package rundir creates the directory that holds a node's sockets, locks and
// pid files (--ch-socket-dir, /run/asp by default).
//
// Everything in it grants authority over a sandbox: the vsock muxer socket
// takes an exec as root in the guest, the identity socket mints that
// sandbox's tokens, the virtiofsd socket speaks vhost-user to a root process.
// A user who can search the directory can reach all of them, so it is 0700.
package rundir

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Mode is the mode of a socket directory.
const Mode = 0o700

// Ensure creates dir, and its parents, with mode 0700, and tightens an
// existing directory that others can enter. It leaves alone a directory that
// is not ours to restrict: one owned by another user, a sticky or
// world-writable one such as /tmp, and the system directories an operator may
// have named by mistake (--ch-socket-dir=/run would otherwise stop systemd
// from working).
func Ensure(dir string) error {
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, Mode); err != nil {
		return err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if fi.Mode().Perm()&0o077 == 0 {
		return nil
	}
	if fi.Mode().Perm()&0o002 != 0 || fi.Mode()&os.ModeSticky != 0 {
		return nil
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return nil
	}
	if shared(dir) {
		return nil
	}
	return os.Chmod(dir, Mode)
}

// SearchableMode is the mode of a directory that every user may pass through but
// not list or write: the parent of the directories VMMs running as users of their
// own work in.
const SearchableMode = 0o711

// EnsureSearchable creates dir, and its parents, and makes it 0711: a user that
// knows a name inside it can reach that, nobody can list it or add to it. A VMM
// running as an unprivileged user needs this on every directory above the files
// the agent hands it (its own directory, its disk), while the entries themselves
// stay readable only by their owner. A directory that is not ours to change
// (owned by another user, a system directory) is left alone and must already let
// everyone through.
func EnsureSearchable(dir string) error {
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, SearchableMode); err != nil {
		return err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	perm := fi.Mode().Perm()
	ours := true
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		ours = false
	}
	if !ours || shared(dir) || perm&0o002 != 0 || fi.Mode()&os.ModeSticky != 0 {
		if perm&0o001 == 0 {
			return fmt.Errorf("%s cannot be searched by the VMM users (mode %o) and is not ours to change", dir, perm)
		}
		return nil
	}
	if perm == SearchableMode {
		return nil
	}
	return os.Chmod(dir, SearchableMode)
}

// shared reports whether dir is a directory other programs rely on.
func shared(dir string) bool {
	for _, d := range []string{
		"/", "/run", "/var", "/var/run", "/var/lib", "/var/tmp", "/tmp", "/usr", "/etc",
		"/home", "/root", "/opt", "/mnt", "/srv", "/dev", "/dev/shm", os.TempDir(),
	} {
		if dir == filepath.Clean(d) {
			return true
		}
	}
	if home, err := os.UserHomeDir(); err == nil && dir == filepath.Clean(home) {
		return true
	}
	return false
}
