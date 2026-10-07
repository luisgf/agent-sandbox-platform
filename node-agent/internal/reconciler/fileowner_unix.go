//go:build unix

package reconciler

import (
	"io/fs"
	"syscall"
)

// fileUID is the user that owns the file fi describes.
func fileUID(fi fs.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint32(st.Uid), true
}
