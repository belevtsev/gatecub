//go:build !unix && !windows

package proc

import "syscall"

// detached does nothing here: the program ends with its terminal.
func detached() *syscall.SysProcAttr { return nil }
