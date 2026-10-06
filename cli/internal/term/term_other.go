//go:build !linux

package term

import "errors"

// Raw mode and the window size are only implemented on linux. The errors are
// variables so the callers' error checks stay meaningful to linters on every
// platform.
var (
	errRawUnsupported  = errors.New("raw mode is only implemented on linux")
	errSizeUnsupported = errors.New("terminal size is only implemented on linux")
)

func IsTerminal(int) bool { return false }

func MakeRaw(int) (func(), error) {
	return func() {}, errRawUnsupported
}

func Size(int) (int, int, error) {
	return 0, 0, errSizeUnsupported
}
