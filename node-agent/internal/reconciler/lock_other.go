//go:build !unix

package reconciler

import "errors"

// LockSocketDir needs flock(2); the node-agent runs on Linux.
func LockSocketDir(string) (*SocketDirLock, error) {
	return nil, errors.New("socket dir lock: not supported on this platform")
}
