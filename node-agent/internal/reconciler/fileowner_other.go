//go:build !unix

package reconciler

import "io/fs"

func fileUID(fs.FileInfo) (uint32, bool) { return 0, false }
