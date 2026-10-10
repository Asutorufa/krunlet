//go:build linux

package krunlet

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type vmCgroup struct {
	path      string
	ownerPath string
	owner     *os.File
	killOnce  sync.Once
	killErr   error
	closeOnce sync.Once
	closeErr  error
}

type cgroupUsage struct {
	MemoryPeakBytes *uint64
	OOMKilled       bool
	PidsLimited     bool
	OOMKills        uint64
	PidsMaxEvents   uint64
}

// ownerDirectory keeps process-locked ownership markers OUTSIDE cgroupfs.
// cgroupfs only permits kernel-defined files; creating ordinary files there
// is not possible. Ownership is per UID, delegated parent and run directory.
func ownerDirectory(parent string) (string, error) {
	digest := sha256.Sum256([]byte(parent))
	path := filepath.Join("/tmp", fmt.Sprintf("krunlet-cgroup-owners-%d-%x", os.Getuid(), digest[:8]))
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ok || stat.Uid != uint32(os.Getuid()) {
		return "", fmt.Errorf("unsafe cgroup owner lock directory: %s", path)
	}
	return path, nil
}

func lockOwner(path string) (*os.File, bool, error) {
	fd, err := unix.Open(path, unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_RDWR|unix.O_CREAT, 0600)
	if err != nil {
		return nil, false, err
	}
	file := os.NewFile(uintptr(fd), path)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return file, false, nil
}

// prepareVMCgroup requires an explicitly configured, delegated cgroup v2
// subtree. Absent CgroupParent means process-group-only containment.
// A configured but unusable cgroup NEVER silently downgrades.
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
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil {
		return nil, fmt.Errorf("statfs cgroup parent: %w", err)
	}
	const cgroup2Magic = 0x63677270
	if fs.Type != cgroup2Magic {
		return nil, fmt.Errorf("cgroup parent %q is not on a cgroup v2 filesystem", root)
	}
	if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
		return nil, fmt.Errorf("cgroup v2 delegation required at %s: %w", root, err)
	}
	owners, err := ownerDirectory(root)
	if err != nil {
		return nil, fmt.Errorf("cgroup ownership: %w", err)
	}
	if err := cleanupStaleVMGroups(root, 0); err != nil {
		return nil, fmt.Errorf("reclaim orphan cgroups: %w", err)
	}

	var cg *vmCgroup
	for attempt := 0; attempt < 4; attempt++ {
		name := "krunlet-vm-" + newRunID()
		ownerPath := filepath.Join(owners, name+".lock")
		// The owner lock is acquired BEFORE mkdir in cgroupfs. Concurrent
		// startup cleanup therefore cannot mistake a fresh run for an orphan.
		owner, busy, err := lockOwner(ownerPath)
		if err != nil {
			return nil, fmt.Errorf("lock cgroup owner: %w", err)
		}
		if busy {
			continue
		}
		path := filepath.Join(root, name)
		if err := os.Mkdir(path, 0700); err != nil {
			_ = owner.Close()
			_ = os.Remove(ownerPath)
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return nil, fmt.Errorf("create per-VM cgroup: %w (delegate cgroup write access)", err)
		}
		cg = &vmCgroup{path: path, owner: owner, ownerPath: ownerPath}
		break
	}
	if cg == nil {
		return nil, errors.New("unable to allocate unique per-VM cgroup")
	}
	fail := func(cause error) (*vmCgroup, error) {
		return nil, errors.Join(cause, cg.Close())
	}
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
		if err := os.WriteFile(filepath.Join(cg.path, item.name), []byte(strconv.FormatInt(item.value, 10)), 0600); err != nil {
			return fail(fmt.Errorf("set %s: %w (delegate memory/pids controllers in cgroup.subtree_control)", item.name, err))
		}
	}
	// Merely seeing cgroup.kill is insufficient: cleanup must be able to
	// open it for writing. Probe the permission without signalling anyone.
	killFile, err := os.OpenFile(filepath.Join(cg.path, "cgroup.kill"), os.O_WRONLY, 0)
	if err != nil {
		return fail(fmt.Errorf("writable cgroup.kill required: %w", err))
	}
	_ = killFile.Close()
	if _, err := os.Stat(filepath.Join(cg.path, "memory.peak")); err != nil {
		return fail(fmt.Errorf("memory.peak required: %w", err))
	}
	return cg, nil
}

// startHelperInCgroup gates the helper BEFORE it can touch libkrun or fork.
// A failed attach reaps only the specific not-yet-contained helper PID.
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
	s.mu.Lock()
	s.cgroup = cg
	s.mu.Unlock()
	err = s.start()
	_ = rd.Close()
	if err != nil {
		return err
	}
	err = os.WriteFile(filepath.Join(cg.path, "cgroup.procs"), []byte(strconv.Itoa(s.cmd.Process.Pid)), 0600)
	if err == nil {
		s.mu.Lock()
		s.cgroupAttached = true
		s.mu.Unlock()
		_, err = wr.Write([]byte{1})
	}
	if err != nil {
		killRun(s, true)
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
	if !filepath.IsAbs(path) || !strings.HasPrefix(filepath.Base(path), "krunlet-vm-") {
		return
	}
	_ = os.WriteFile(filepath.Join(path, "cgroup.kill"), []byte("1"), 0600)
}

func (cg *vmCgroup) kill() error {
	if cg == nil {
		return nil
	}
	cg.killOnce.Do(func() {
		// cgroup.kill is authoritative. Never follow it with kill(-pgid),
		// because process group IDs can be reused by unrelated processes.
		err := os.WriteFile(filepath.Join(cg.path, "cgroup.kill"), []byte("1"), 0600)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			cg.killErr = fmt.Errorf("kill cgroup %s: %w", cg.path, err)
		}
	})
	return cg.killErr
}

func removeCgroup(path string) error {
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := os.Remove(path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("reclaim cgroup %s: %w", path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (cg *vmCgroup) Close() error {
	if cg == nil {
		return nil
	}
	cg.closeOnce.Do(func() {
		cg.closeErr = cg.kill()
		cg.closeErr = errors.Join(cg.closeErr, removeCgroup(cg.path))
		if cg.owner != nil {
			cg.closeErr = errors.Join(cg.closeErr, cg.owner.Close())
		}
		if cg.closeErr == nil {
			_ = os.Remove(cg.ownerPath)
		}
	})
	return cg.closeErr
}

// cleanupStaleVMGroups immediately reclaims per-VM directories whose owner
// lock is no longer held, even when they are populated. An active owner lock
// (including another Runner/process) is NEVER killed. The age parameter is
// retained for existing call sites but intentionally not used: SIGKILL orphan
// recovery cannot wait 24 hours.
func cleanupStaleVMGroups(parent string, _ time.Duration) error {
	owners, err := ownerDirectory(parent)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "krunlet-vm-") {
			continue
		}
		// Do not kill cgroups created by another Linux UID. Their active
		// ownership locks live under that UID's private directory, and
		// probing our own directory would falsely classify them as orphans.
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect cgroup owner uid: %w", err)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uint32(os.Getuid()) {
			continue
		}
		path := filepath.Join(parent, entry.Name())
		ownerPath := filepath.Join(owners, entry.Name()+".lock")
		lock, active, err := lockOwner(ownerPath)
		if err != nil {
			return fmt.Errorf("inspect cgroup owner %s: %w", path, err)
		}
		if active {
			continue
		}
		// No living owner: this run was orphaned, possibly while its VM
		// continued running after the parent's uncatchable SIGKILL.
		killErr := os.WriteFile(filepath.Join(path, "cgroup.kill"), []byte("1"), 0600)
		if killErr != nil && !errors.Is(killErr, os.ErrNotExist) {
			_ = lock.Close()
			return fmt.Errorf("kill orphan cgroup %s: %w", path, killErr)
		}
		if err := removeCgroup(path); err != nil {
			_ = lock.Close()
			return err
		}
		_ = lock.Close()
		_ = os.Remove(ownerPath)
	}
	return nil
}

func cgroupEventCounters(path string) (map[string]uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	counters := make(map[string]uint64)
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		n, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid cgroup counter %q: %w", line, err)
		}
		counters[parts[0]] = n
	}
	return counters, nil
}

func (cg *vmCgroup) usage() (cgroupUsage, error) {
	var usage cgroupUsage
	if cg == nil {
		return usage, nil
	}
	peak, err := os.ReadFile(filepath.Join(cg.path, "memory.peak"))
	if err != nil {
		return usage, err
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(peak)), 10, 64)
	if err != nil {
		return usage, fmt.Errorf("read memory.peak: %w", err)
	}
	usage.MemoryPeakBytes = &n
	mem, err := cgroupEventCounters(filepath.Join(cg.path, "memory.events"))
	if err != nil {
		return usage, err
	}
	usage.OOMKills = mem["oom_kill"] + mem["oom_group_kill"]
	usage.OOMKilled = usage.OOMKills > 0
	pids, err := cgroupEventCounters(filepath.Join(cg.path, "pids.events"))
	if err != nil {
		return usage, err
	}
	usage.PidsMaxEvents = pids["max"]
	usage.PidsLimited = usage.PidsMaxEvents > 0
	return usage, nil
}
