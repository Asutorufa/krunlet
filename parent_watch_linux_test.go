//go:build linux

package krunlet

import (
	"context"
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

func linuxProcessExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == syscall.ESRCH {
		return false
	}
	// A zombie is dead but may await the system reaper in a container.
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		s := string(b)
		if k := strings.LastIndex(s, ") "); k >= 0 && len(s) > k+2 && s[k+2] == 'Z' {
			return false
		}
	}
	return err == nil || err == syscall.EPERM
}

func TestParentSIGKILLReapsHelperGroup(t *testing.T) {
	switch os.Getenv("KRUNLET_TEST_ORPHAN_MODE") {
	case "parent":
		r, err := New(Options{
			RootFS:     os.Getenv("KRUNLET_TEST_ORPHAN_ROOT"),
			HelperPath: os.Getenv("KRUNLET_TEST_ORPHAN_SCRIPT"),
			Timeout:    30 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = r.Run(context.Background(), Request{Command: []string{"/bin/true"}})
		return
	case "helper":
		if err := watchHelperParent(); err != nil {
			t.Fatal(err)
		}
		child := exec.Command("sleep", "30")
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		info := fmt.Sprintf("%d %d", os.Getpid(), child.Process.Pid)
		if err := os.WriteFile(os.Getenv("KRUNLET_TEST_ORPHAN_PIDS"), []byte(info), 0600); err != nil {
			t.Fatal(err)
		}
		_ = child.Wait()
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	folder := t.TempDir()
	script := filepath.Join(folder, "helper")
	body := fmt.Sprintf("#!/bin/sh\nKRUNLET_TEST_ORPHAN_MODE=helper exec %q -test.run=TestParentSIGKILLReapsHelperGroup\n", exe)
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	pidsFile := filepath.Join(folder, "worker.pids")
	root := filepath.Join(folder, "rootfs")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	parent := exec.Command(exe, "-test.run=TestParentSIGKILLReapsHelperGroup")
	parent.Env = append(os.Environ(),
		"KRUNLET_TEST_ORPHAN_MODE=parent",
		"KRUNLET_TEST_ORPHAN_ROOT="+root,
		"KRUNLET_TEST_ORPHAN_SCRIPT="+script,
		"KRUNLET_TEST_ORPHAN_PIDS="+pidsFile,
		"TMPDIR="+folder)
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parent.Process.Kill(); _ = parent.Wait() }()
	var helperPid, childPid int
	until := time.Now().Add(8 * time.Second)
	for time.Now().Before(until) {
		b, e := os.ReadFile(pidsFile)
		if e == nil {
			fields := strings.Fields(string(b))
			if len(fields) == 2 {
				helperPid, _ = strconv.Atoi(fields[0])
				childPid, _ = strconv.Atoi(fields[1])
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if helperPid <= 0 || childPid <= 0 {
		t.Fatalf("helper did not report process group: helper=%d child=%d", helperPid, childPid)
	}
	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	until = time.Now().Add(4 * time.Second)
	for time.Now().Before(until) {
		if !linuxProcessExists(helperPid) && !linuxProcessExists(childPid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("helper group survived parent SIGKILL: helper %d alive=%t, child %d alive=%t", helperPid, linuxProcessExists(helperPid), childPid, linuxProcessExists(childPid))
}
