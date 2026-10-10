//go:build linux || darwin

package krunlet

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"
)

type helperSupervisor struct {
	cmd         *exec.Cmd
	mu          sync.Mutex
	terminating bool
	finished    bool
	ended       chan struct{}
	graceDone   chan struct{}
}

func configureHelper(cmd *exec.Cmd) *helperSupervisor {
	cmd.SysProcAttr = helperSysProcAttr()
	cmd.Env = append(os.Environ(), "KRUNLET_SUPERVISOR_PARENT="+strconv.Itoa(os.Getpid()))
	p := &helperSupervisor{cmd: cmd, ended: make(chan struct{}), graceDone: make(chan struct{})}
	cmd.Cancel = func() error { p.terminate(); return nil }
	cmd.WaitDelay = 3 * time.Second
	return p
}

// start pins the parent OS thread until the child has been reaped. Linux
// Pdeathsig belongs to the creating *thread*; without this Go's scheduler
// could retire that thread and spuriously terminate a live VM.
func (p *helperSupervisor) start() error {
	started := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err := p.cmd.Start()
		started <- err
		if err == nil {
			<-p.ended
		}
	}()
	return <-started
}

func (p *helperSupervisor) terminate() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.finished || p.terminating {
		p.mu.Unlock()
		return
	}
	p.terminating = true
	p.mu.Unlock()

	signalHelperGroup(p.cmd, false)
	go func() {
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		<-timer.C
		// The grace period is per process group, not just its leader.
		// Killing immediately after Wait would cut it short.
		signalHelperGroup(p.cmd, true)
		close(p.graceDone)
	}()
}

func (p *helperSupervisor) finish() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.finished {
		p.mu.Unlock()
		return
	}
	p.finished = true
	terminating := p.terminating
	p.mu.Unlock()

	if terminating {
		// Always let the TERM grace timer complete before releasing the
		// pinned parent OS thread.
		<-p.graceDone
	} else {
		// Natural helper exit (including panic): force-kill descendants.
		signalHelperGroup(p.cmd, true)
	}
	close(p.ended)
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
