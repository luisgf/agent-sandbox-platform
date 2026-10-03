package term

import (
	"syscall"
	"unsafe"
)

// Linux ioctl numbers. Not in x/sys so the CLI stays dependency-free.
const (
	ioctlGetTermios = 0x5401 // TCGETS
	ioctlSetTermios = 0x5402 // TCSETS
	ioctlGetWinSize = 0x5413 // TIOCGWINSZ
)

// IsTerminal reports whether fd is a terminal.
func IsTerminal(fd int) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), ioctlGetTermios, uintptr(unsafe.Pointer(&t)), 0, 0, 0)
	return errno == 0
}

// MakeRaw puts fd in raw mode and returns a restore func.
// Raw mode is what makes local keystrokes match a guest PTY (no local echo,
// no line buffering). The caller must invoke restore, including on panic.
func MakeRaw(fd int) (func(), error) {
	var old syscall.Termios
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), ioctlGetTermios, uintptr(unsafe.Pointer(&old)), 0, 0, 0); errno != 0 {
		return nil, errno
	}
	next := old
	next.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	next.Oflag &^= syscall.OPOST
	next.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	next.Cflag &^= syscall.CSIZE | syscall.PARENB
	next.Cflag |= syscall.CS8
	next.Cc[syscall.VMIN] = 1
	next.Cc[syscall.VTIME] = 0
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), ioctlSetTermios, uintptr(unsafe.Pointer(&next)), 0, 0, 0); errno != 0 {
		return nil, errno
	}
	return func() {
		_, _, _ = syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), ioctlSetTermios, uintptr(unsafe.Pointer(&old)), 0, 0, 0)
	}, nil
}

type winSize struct {
	Row, Col, XPixel, YPixel uint16
}

// Size returns the terminal rows and cols.
func Size(fd int) (rows, cols int, err error) {
	var ws winSize
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), ioctlGetWinSize, uintptr(unsafe.Pointer(&ws)), 0, 0, 0)
	if errno != 0 {
		return 0, 0, errno
	}
	return int(ws.Row), int(ws.Col), nil
}
