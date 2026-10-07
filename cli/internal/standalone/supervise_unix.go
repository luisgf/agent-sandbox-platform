package standalone

import "syscall"

// withOwnGroup puts the child in a process group of its own: a ^C at a terminal reaches asp-server
// only, which stops its children in order, and not each of them at once.
func withOwnGroup(a *syscall.SysProcAttr) *syscall.SysProcAttr {
	if a == nil {
		a = &syscall.SysProcAttr{}
	}
	a.Setpgid = true
	return a
}
