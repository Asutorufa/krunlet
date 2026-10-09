//go:build linux

package krunlet

import "syscall"

// SIGTERM is delivered by the kernel even if the parent is SIGKILLed.
// The built-in helper also monitors a parent pidfd to kill the *group*.
func helperSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid:true,Pdeathsig:syscall.SIGTERM}
}
