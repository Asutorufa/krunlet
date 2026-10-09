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

// start pins the parent OS thread until the child has been reaped. Linux
// Pdeathsig belongs to the creating *thread*; without this Go's scheduler
// could retire that thread and spuriously terminate a live VM.
func (p *helperSupervisor) start() error {
	started:=make(chan error,1)
	go func(){
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err:=p.cmd.Start()
		started<-err
		if err==nil { <-p.ended }
	}()
	return <-started
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
