//go:build !unix

package reconciler

import "errors"

func freeBytes(string) (uint64, error) {
	return 0, errors.New("free disk space is not available on this platform")
}
