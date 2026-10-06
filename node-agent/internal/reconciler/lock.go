package reconciler

import (
	"errors"
	"os"
)

// lockFileName is the agent lock in the socket dir. No leftover name matches it.
const lockFileName = "node-agent.lock"

// ErrSocketDirLocked means another live node-agent holds the socket dir.
var ErrSocketDirLocked = errors.New("another node-agent is running with this --ch-socket-dir")

// SocketDirLock is a held LockSocketDir. Release, or the holder's exit,
// drops it.
type SocketDirLock struct {
	f *os.File
}

// Release drops the lock. It is safe on a nil lock.
func (l *SocketDirLock) Release() {
	if l != nil && l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
}
