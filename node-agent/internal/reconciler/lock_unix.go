//go:build unix

package reconciler

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/rundir"
)

// LockSocketDir takes the agent lock of a socket dir without waiting. A real
// node-agent holds it for its whole life, so that a second agent, or
// --reap-only, on the same --ch-socket-dir cannot take its VMs for leftovers.
// The kernel drops the lock when the holder exits, however it exits, and Go
// opens files close-on-exec, so cloud-hypervisor children never inherit it.
func LockSocketDir(dir string) (*SocketDirLock, error) {
	if err := rundir.Ensure(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, lockFileName)
	// 0600: anyone who could open the file could hold a shared lock on it
	// and keep the agent from starting.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder, _ := io.ReadAll(io.LimitReader(f, 32))
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			pid := strings.TrimSpace(string(holder))
			if pid == "" {
				pid = "unknown"
			}
			return nil, fmt.Errorf("%w: %s is held by pid %s", ErrSocketDirLocked, path, pid)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	// The pid is for the message above; the lock itself is the flock.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &SocketDirLock{f: f}, nil
}
