//go:build linux

package krunlet

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type vmCgroup struct{ path string }

// prepareVMCgroup requires a predelegated cgroup v2 directory with memory,
// pids and cgroup.kill support. It NEVER mounts cgroupfs or silently skips
// limits, and deliberately forbids custom helpers that cannot honor the gate.
func prepareVMCgroup(opts Options) (*vmCgroup, error) {
	if opts.CgroupParent == "" {
		return nil, nil
	}
	if !filepath.IsAbs(opts.CgroupParent) {
		return nil, errors.New("cgroup parent must be absolute")
	}
	root, err := filepath.EvalSymlinks(opts.CgroupParent)
	if err != nil {
		return nil, fmt.Errorf("resolve cgroup parent: %w", err)
	}
	if _, err = os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
		return nil, fmt.Errorf("cgroup v2 delegation required at %s: %w", root, err)
	}
	dir, err := os.MkdirTemp(root, "krunlet-vm-")
	if err != nil {
		return nil, fmt.Errorf("create per-VM cgroup: %w", err)
	}
	cg := &vmCgroup{path: dir}
	fail := func(err error) (*vmCgroup, error) { _ = os.Remove(dir); return nil, err }
	memory := opts.CgroupMemoryMaxBytes
	if memory == 0 {
		memory = (int64(opts.MemoryMiB) + 256) * 1024 * 1024
	}
	pids := opts.CgroupPidsMax
	if pids == 0 {
		pids = 256
	}
	for _, item := range []struct {
		name  string
		value int64
	}{{"memory.max", memory}, {"pids.max", pids}} {
		if err := os.WriteFile(filepath.Join(dir, item.name), []byte(strconv.FormatInt(item.value, 10)), 0600); err != nil {
			return fail(fmt.Errorf("set %s: %w (delegate memory/pids controllers in cgroup.subtree_control)", item.name, err))
		}
	}
	if _, err = os.Stat(filepath.Join(dir, "cgroup.kill")); err != nil {
		return fail(fmt.Errorf("cgroup.kill required: %w", err))
	}
	return cg, nil
}

// startHelperInCgroup creates an inherited pipe gate BEFORE fork. The
// built-in helper cannot touch libkrun or start children until moved into the
// per-VM cgroup. A failed attach kills and reaps it instead of failing open.
func startHelperInCgroup(s *helperSupervisor, cg *vmCgroup) error {
	if cg == nil {
		return s.start()
	}
	rd, wr, err := os.Pipe()
	if err != nil {
		return err
	}
	defer wr.Close()
	fd := 3 + len(s.cmd.ExtraFiles)
	s.cmd.ExtraFiles = append(s.cmd.ExtraFiles, rd)
	s.cmd.Env = append(s.cmd.Env, "KRUNLET_CGROUP_GATE_FD="+strconv.Itoa(fd), "KRUNLET_CGROUP_PATH="+cg.path)
	err = s.start()
	_ = rd.Close()
	if err != nil {
		return err
	}
	err = os.WriteFile(filepath.Join(cg.path, "cgroup.procs"), []byte(strconv.Itoa(s.cmd.Process.Pid)), 0600)
	if err == nil {
		_, err = wr.Write([]byte{1})
	}
	if err != nil {
		signalHelperGroup(s.cmd, true)
		_ = s.cmd.Wait()
		s.finish()
		return fmt.Errorf("attach helper to cgroup (fail closed): %w", err)
	}
	return nil
}

func waitCgroupStartupGate() error {
	value := os.Getenv("KRUNLET_CGROUP_GATE_FD")
	if value == "" {
		return nil
	}
	fd, err := strconv.Atoi(value)
	if err != nil || fd < 3 {
		return errors.New("invalid cgroup gate descriptor")
	}
	f := os.NewFile(uintptr(fd), "cgroup-startup-gate")
	if f == nil {
		return errors.New("missing cgroup gate")
	}
	defer f.Close()
	var signal [1]byte
	if _, err := io.ReadFull(f, signal[:]); err != nil {
		return fmt.Errorf("cgroup assignment gate closed: %w", err)
	}
	if signal[0] != 1 {
		return errors.New("invalid cgroup assignment token")
	}
	return nil
}

func killOwnCgroup() {
	path := os.Getenv("KRUNLET_CGROUP_PATH")
	if path == "" {
		return
	}
	if !filepath.IsAbs(path) || !strings.HasPrefix(filepath.Base(path), "krunlet-vm-") {
		return
	}
	// cgroup.kill covers escaped host process groups / setsid descendants.
	_ = os.WriteFile(filepath.Join(path, "cgroup.kill"), []byte("1"), 0600)
}

func (cg *vmCgroup) Close() error {
	if cg == nil {
		return nil
	}
	var last error
	if err := os.WriteFile(filepath.Join(cg.path, "cgroup.kill"), []byte("1"), 0600); err != nil && !errors.Is(err, syscall.ENOENT) {
		last = err
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := os.Remove(cg.path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return last
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("reclaim cgroup %s: %w", cg.path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
