package proc

import "syscall"

// createNoWindow gives the program a console of its own that shows no
// window. The programs it starts inherit that console.
const createNoWindow = 0x08000000

// detached starts the program outside this command's console and process
// group, so that Ctrl+C and closing the console do not reach it.
func detached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | createNoWindow}
}
