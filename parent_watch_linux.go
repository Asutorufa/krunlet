//go:build linux

package krunlet

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

func watchHelperParent() error {
	value:=os.Getenv("KRUNLET_SUPERVISOR_PARENT")
	if value=="" {return nil}
	parent,err:=strconv.Atoi(value)
	if err!=nil || parent<2 {return fmt.Errorf("invalid helper parent pid %q",value)}
	if os.Getppid()!=parent { killOrphanGroup();return fmt.Errorf("helper parent already gone") }
	// The kernel's PR_SET_PDEATHSIG notifies us even if the parent dies by
	// SIGKILL. A pidfd remains attached to that process, unlike numeric PIDs.
	signals:=make(chan os.Signal,1)
	signal.Notify(signals,syscall.SIGTERM)
	fd,err:=unix.PidfdOpen(parent,0)
	if err!=nil { signal.Stop(signals);return fmt.Errorf("pidfd_open parent: %w",err)}
	if os.Getppid()!=parent {
		_ = unix.Close(fd)
		killOrphanGroup()
		return fmt.Errorf("parent disappeared during helper bootstrap")
	}
	go func() {
		defer unix.Close(fd)
		for {
			var poll=[1]unix.PollFd{{Fd:int32(fd),Events:unix.POLLIN}}
			n,e:=unix.Poll(poll[:],-1)
			if e==unix.EINTR {continue}
			if e!=nil || n>0 {killOrphanGroup();return}
		}
	}()
	go func(){ <-signals; killOrphanGroup() }()
	return nil
}

func killOrphanGroup() {
	_ = syscall.Kill(-syscall.Getpgrp(),syscall.SIGKILL)
	_ = syscall.Kill(os.Getpid(),syscall.SIGKILL)
}
