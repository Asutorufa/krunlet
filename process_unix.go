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
	cmd            *exec.Cmd
	mu             sync.Mutex
	terminating    bool
	finished       bool
	ended          chan struct{}
	graceDone      chan struct{}
	cgroup         *vmCgroup
	cgroupAttached bool
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
	hasCgroup := p.cgroup != nil
	p.mu.Unlock()

	if hasCgroup {
		// No delayed process-group signal when cgroup.kill owns reclamation.
		killRun(p, true)
		close(p.graceDone)
		return
	}
	killRun(p, false)
	go func() {
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		<-timer.C
		killRun(p, true)
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
		killRun(p, true)
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

// killRun selects exactly one authoritative reclamation strategy. A contained
// run uses cgroup.kill exclusively; SIGKILL on the historical process group
// after cgroup.kill is dangerous because a PGID can be reused by an unrelated
// process. Before cgroup assignment only the exact gated helper PID is killed.
func killRun(p *helperSupervisor, force bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	cg, attached := p.cgroup, p.cgroupAttached
	p.mu.Unlock()
	if cg != nil {
		if attached {
			_ = cg.kill()
		} else if p.cmd != nil && p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		return
	}
	signalHelperGroup(p.cmd, force)
}
