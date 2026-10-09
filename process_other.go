//go:build !linux && !darwin

package krunlet

import (
	"os/exec"
	"sync"
)

type helperSupervisor struct{
	cmd *exec.Cmd
	once sync.Once
}
func configureHelper(cmd *exec.Cmd) *helperSupervisor {
	return &helperSupervisor{cmd:cmd}
}
func (p *helperSupervisor) terminate() { if p != nil && p.cmd.Process != nil { _ = p.cmd.Process.Kill() } }
func (p *helperSupervisor) finish() {}
func signalHelperGroup(cmd *exec.Cmd, _ bool) {
	if cmd != nil && cmd.Process != nil { _ = cmd.Process.Kill() }
}
