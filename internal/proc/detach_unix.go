//go:build unix

package proc

import "syscall"

// detached starts the program in a session of its own: it has no terminal,
// so neither an interrupt nor a hangup of the terminal reaches it.
func detached() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
