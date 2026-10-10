package krunlet

import "errors"

// ErrOOM is backed by Linux cgroup memory.events oom_kill, not an exit code.
var ErrOOM = errors.New("VM host cgroup out of memory")

// ErrPidsLimit means host cgroup pids.max rejected a process/thread.
// It does NOT restrict the number of tasks inside the guest kernel.
var ErrPidsLimit = errors.New("VM host cgroup pids.max reached")
