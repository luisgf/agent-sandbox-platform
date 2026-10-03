//go:build !linux

package term

import "fmt"

func IsTerminal(int) bool { return false }

func MakeRaw(int) (func(), error) {
	return func() {}, fmt.Errorf("raw mode is only implemented on linux")
}

func Size(int) (int, int, error) {
	return 0, 0, fmt.Errorf("terminal size is only implemented on linux")
}
