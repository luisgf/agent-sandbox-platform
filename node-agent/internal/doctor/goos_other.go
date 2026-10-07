//go:build !linux

package doctor

import "runtime"

func goos() string { return runtime.GOOS }
