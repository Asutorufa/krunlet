//go:build linux

package krunlet

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func TestReviewKillRunNeverSignalsProcessGroupInCgroupMode(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sleep", "30")
	sup := configureHelper(cmd)
	cg := &vmCgroup{path: t.TempDir()}
	sup.cgroup = cg
	sup.cgroupAttached = true
	if err := sup.start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		sup.finish()
	}()
	killRun(sup, true)
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatalf("cgroup strategy also signalled an unrelated process group: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(cg.path, "cgroup.kill"))
	if err != nil || string(data) != "1" {
		t.Fatalf("cgroup.kill was not selected: data=%q err=%v", data, err)
	}
}

func TestReviewKillRunFallsBackToProcessGroupWithoutCgroup(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sleep", "30")
	sup := configureHelper(cmd)
	if err := sup.start(); err != nil {
		t.Fatal(err)
	}
	killRun(sup, true)
	_ = cmd.Wait()
	sup.finish()
	if err := syscall.Kill(cmd.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("fallback failed to terminate helper pid %s: %v", strconv.Itoa(cmd.Process.Pid), err)
	}
}

func TestReviewCgroupUsageParsesOOMAndPidsCounters(t *testing.T) {
	path := t.TempDir()
	for name, value := range map[string]string{
		"memory.peak":   "424242\n",
		"memory.events": "low 0\nhigh 0\nmax 10\noom 1\noom_kill 1\noom_group_kill 0\n",
		"pids.events":   "max 3\n",
	} {
		if err := os.WriteFile(filepath.Join(path, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	usage, err := (&vmCgroup{path: path}).usage()
	if err != nil {
		t.Fatal(err)
	}
	if usage.MemoryPeakBytes == nil || *usage.MemoryPeakBytes != 424242 || !usage.OOMKilled || !usage.PidsLimited || usage.OOMKills != 1 || usage.PidsMaxEvents != 3 {
		t.Fatalf("cgroup usage ignored a limit event: %+v", usage)
	}
}
