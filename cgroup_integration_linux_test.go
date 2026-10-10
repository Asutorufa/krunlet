//go:build vm_integration && linux

package krunlet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Verifies kernel cgroup.kill, not a simulation: the child executes setsid,
// escaping its helper process group. cgroup.kill must still terminate it.
func TestRealCgroupKillsSetsidEscapes(t *testing.T) {
	if os.Getenv("KRUNLET_CGROUP_ESCAPE_CHILD") == "1" {
		if err := waitCgroupStartupGate(); err != nil {
			t.Fatal(err)
		}
		sleeper := exec.Command("setsid", "sleep", "40")
		if err := sleeper.Start(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv("KRUNLET_CGROUP_ESCAPE_PIDS"), []byte(strconv.Itoa(sleeper.Process.Pid)), 0600); err != nil {
			t.Fatal(err)
		}
		_ = sleeper.Wait()
		return
	}
	parent := integrationCgroup(t)
	cg, err := prepareVMCgroup(Options{CgroupParent: parent, MemoryMiB: 256})
	if err != nil {
		t.Fatal(err)
	}
	defer cg.Close()
	pidFile := filepath.Join(t.TempDir(), "detached.pid")
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestRealCgroupKillsSetsidEscapes$")
	sup := configureHelper(cmd)
	cmd.Env = append(cmd.Env, "KRUNLET_CGROUP_ESCAPE_CHILD=1", "KRUNLET_CGROUP_ESCAPE_PIDS="+pidFile)
	if err := startHelperInCgroup(sup, cg); err != nil {
		t.Fatal(err)
	}
	defer func() { signalHelperGroup(cmd, true); _ = cmd.Wait(); sup.finish() }()
	var detached int
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			detached, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if detached > 0 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if detached <= 0 {
		t.Fatal("setsid escape did not start")
	}
	if err := cg.Close(); err != nil {
		t.Fatalf("cgroup.kill could not reclaim detached host process: %v", err)
	}
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(detached, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		stat, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", detached))
		if e == nil {
			if i := strings.LastIndex(string(stat), ") "); i >= 0 && len(stat) > i+2 && stat[i+2] == 'Z' {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("escaped process survived cgroup.kill: PID %d", detached)
}

// On a self-hosted runner there must be no surviving per-VM cgroup after
// all real VM tests complete, even when runs are queued across Runners.
func TestRealVMCgroupDirectoryReclaimed(t *testing.T) {
	root := integrationRootFS(t)
	parent := integrationCgroup(t)
	r, err := New(Options{RootFS: root, Timeout: 45 * time.Second, CgroupParent: parent})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), Request{Command: []string{"/bin/true"}}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "krunlet-vm-") {
			t.Errorf("leftover per-VM cgroup: %s", e.Name())
		}
	}
}
