//go:build linux || darwin

package krunlet

import (
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

type helperSupervisor struct {
	cmd   *exec.Cmd
	once  sync.Once
	ended chan struct{}
}

func configureHelper(cmd *exec.Cmd) *helperSupervisor {
	cmd.SysProcAttr = helperSysProcAttr()
	cmd.Env = append(os.Environ(), "KRUNLET_SUPERVISOR_PARENT="+strconv.Itoa(os.Getpid()))
	p := &helperSupervisor{cmd: cmd, ended: make(chan struct{})}
	cmd.Cancel = func() error { p.terminate(); return nil }
	cmd.WaitDelay = 3 * time.Second
	return p
}

func (p *helperSupervisor) terminate() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		signalHelperGroup(p.cmd, false)
		go func() {
			select {
			case <-time.After(2 * time.Second):
				signalHelperGroup(p.cmd, true)
			case <-p.ended:
			}
		}()
	})
}

func (p *helperSupervisor) finish() {
	if p == nil {
		return
	}
	close(p.ended)
	// The helper may have spawned descendants and exited before they did.
	// While its PID/PGID are still owned by this invocation, kill the group.
	signalHelperGroup(p.cmd, true)
}

func signalHelperGroup(cmd *exec.Cmd, force bool) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
	if force {
		_ = cmd.Process.Kill()
	}
}
