//go:build darwin

package krunlet

import (
	"fmt"
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// kqueue EVFILT_PROC NOTE_EXIT observes the parent without periodically
// polling a reusable numeric PID. The helper kills its own process group.
func watchHelperParent() error {
	value := os.Getenv("KRUNLET_SUPERVISOR_PARENT")
	if value == "" {
		return nil
	}
	parent, err := strconv.Atoi(value)
	if err != nil || parent < 2 {
		return fmt.Errorf("invalid helper parent pid %q", value)
	}
	if os.Getppid() != parent {
		killOrphanGroup()
		return fmt.Errorf("helper parent already gone")
	}
	kq, err := unix.Kqueue()
	if err != nil {
		return fmt.Errorf("kqueue helper parent: %w", err)
	}
	event := unix.Kevent_t{Ident: uint64(parent), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ENABLE, Fflags: unix.NOTE_EXIT}
	if _, err = unix.Kevent(kq, []unix.Kevent_t{event}, nil, nil); err != nil {
		_ = unix.Close(kq)
		return fmt.Errorf("register parent exit event: %w", err)
	}
	if os.Getppid() != parent {
		_ = unix.Close(kq)
		killOrphanGroup()
		return fmt.Errorf("parent disappeared during helper bootstrap")
	}
	go func() {
		defer unix.Close(kq)
		var events [1]unix.Kevent_t
		for {
			n, e := unix.Kevent(kq, nil, events[:], nil)
			if e == unix.EINTR {
				continue
			}
			if e != nil || n > 0 {
				killOrphanGroup()
				return
			}
		}
	}()
	return nil
}

func killOrphanGroup() {
	_ = syscall.Kill(-syscall.Getpgrp(), syscall.SIGKILL)
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
}
