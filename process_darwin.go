//go:build darwin

package krunlet

import "syscall"

func helperSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
