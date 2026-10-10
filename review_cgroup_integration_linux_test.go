//go:build linux && vm_integration

package krunlet

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReviewUndelegatedCgroupFailsForUnprivilegedUser(t *testing.T) {
	if os.Getenv("KRUNLET_UNPRIVILEGED_CGROUP_CHILD") == "1" {
		cg, err := prepareVMCgroup(Options{CgroupParent: os.Getenv("KRUNLET_TEST_CGROUP_PARENT"), MemoryMiB: 256})
		if err == nil {
			_ = cg.Close()
			t.Fatal("unprivileged helper silently acquired cgroup containment")
		}
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("requires root to switch to an unprivileged Linux user")
	}
	_ = integrationCgroup(t)
	// Go's test binary is commonly stored under root-only /tmp/go-build.
	// Copy it into a traversable directory before dropping to nobody.
	work, err := os.MkdirTemp("/tmp", "krunlet-unprivileged-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(work)
	if err := os.Chmod(work, 0755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(work, "review.test")
	contents, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, contents, 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-test.run=^TestReviewUndelegatedCgroupFailsForUnprivilegedUser$")
	cmd.Env = append(os.Environ(), "KRUNLET_UNPRIVILEGED_CGROUP_CHILD=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unprivileged containment probe failed: %s: %v", out, err)
	}
}

// gatedHostPressure starts a host-side shell which blocks on fd 3 BEFORE
// it forks or allocates. The parent first moves that shell to cgroup.procs,
// then releases the gate; the pressure load can never escape containment.
func gatedHostPressure(ctx context.Context, script string) *exec.Cmd {
	gate := "IFS= read -r token <&3 || [ -n \"$token\" ] || exit 99\n"
	return exec.CommandContext(ctx, "/bin/sh", "-c", gate+script)
}

func TestReviewRealHostCgroupOOMIsDistinguishable(t *testing.T) {
	parent := integrationCgroup(t)
	cg, err := prepareVMCgroup(Options{CgroupParent: parent, MemoryMiB: 256, CgroupMemoryMaxBytes: 128 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer cg.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	cmd := gatedHostPressure(ctx, "exec python3 -c 'b=bytearray(512*1024*1024); b[::4096]=bytes([1]) * ((len(b)+4095)//4096)'")
	sup := configureHelper(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := startHelperInCgroup(sup, cg); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	sup.finish()
	usage, err := cg.usage()
	if err != nil {
		t.Fatal(err)
	}
	if !usage.OOMKilled || usage.MemoryPeakBytes == nil || *usage.MemoryPeakBytes == 0 {
		t.Fatalf("real memory.max OOM missing (wait=%v, peak=%v): usage=%+v stderr=%s", waitErr, usage.MemoryPeakBytes, usage, stderr.String())
	}
}

func TestReviewRealHostPidsMaxIsDistinguishable(t *testing.T) {
	cg, err := prepareVMCgroup(Options{CgroupParent: integrationCgroup(t), MemoryMiB: 256, CgroupPidsMax: 32})
	if err != nil {
		t.Fatal(err)
	}
	defer cg.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// This workload deliberately exceeds the host pids controller. The
	// bounded context and cgroup.kill reclaim every forked sleep process.
	cmd := gatedHostPressure(ctx, "i=0; while [ \"$i\" -lt 96 ]; do sleep 30 & i=$((i+1)); done; wait")
	sup := configureHelper(cmd)
	if err := startHelperInCgroup(sup, cg); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	sup.finish()
	usage, err := cg.usage()
	if err != nil {
		t.Fatal(err)
	}
	if !usage.PidsLimited {
		t.Fatalf("host fork load bypassed pids.max: %+v", usage)
	}
}

func TestReviewRealVMReportsHostCgroupPeak(t *testing.T) {
	root := integrationRootFS(t)
	var stats Stats
	runner, err := New(Options{RootFS: root, MemoryMiB: 256, CgroupParent: integrationCgroup(t), Timeout: 45 * time.Second, OnStats: func(s Stats) { stats = s }})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), Request{Command: []string{"/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Containment != "cgroup_v2" || stats.Containment != "cgroup_v2" || result.MemoryPeakBytes == nil || stats.MemoryPeakBytes == nil || *stats.MemoryPeakBytes == 0 {
		t.Fatalf("real VM cgroup memory.peak missing from Result/Stats: result=%+v stats=%+v", result, stats)
	}
}

// This validates Result.ErrOOM on a REAL microVM run. A host-side pressure
// process is attached to the VM cgroup while the guest sleeps: memory.peak
// and memory.events must account for every host task in that same cgroup,
// not just the helper's reported RSS.
func TestReviewVMHostOOMMappedToTypedError(t *testing.T) {
	parent := integrationCgroup(t)
	before := make(map[string]bool)
	existing, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range existing {
		before[entry.Name()] = true
	}
	runner, err := New(Options{RootFS: integrationRootFS(t), MemoryMiB: 256, CgroupParent: parent, CgroupMemoryMaxBytes: 768 << 20, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result Result
		err    error
	}
	results := make(chan outcome, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() {
		result, err := runner.Run(ctx, Request{Command: []string{"/bin/sh", "-c", "sleep 12"}})
		results <- outcome{result: result, err: err}
	}()
	var group string
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) && group == "" {
		entries, err := os.ReadDir(parent)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "krunlet-vm-") && !before[entry.Name()] {
				group = filepath.Join(parent, entry.Name())
				break
			}
		}
		if group == "" {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if group == "" {
		cancel()
		<-results
		t.Fatal("real VM never created a host cgroup")
	}
	// A gated helper waits until it has joined the VM cgroup, so allocation
	// can never escape to the CI host's unbounded parent cgroup.
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := gatedHostPressure(ctx, "exec python3 -c 'b=bytearray(1024*1024*1024); b[::4096]=bytes([1]) * ((len(b)+4095)//4096)'")
	cmd.Env = append(os.Environ(), "KRUNLET_CGROUP_GATE_FD=3")
	cmd.ExtraFiles = []*os.File{rd}
	if err := cmd.Start(); err != nil {
		_ = rd.Close()
		_ = wr.Close()
		t.Fatal(err)
	}
	_ = rd.Close()
	if err := os.WriteFile(filepath.Join(group, "cgroup.procs"), []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		_ = wr.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	_, _ = wr.Write([]byte{1})
	_ = wr.Close()
	_ = cmd.Wait()
	out := <-results
	if !errors.Is(out.err, ErrOOM) || out.result.TerminationReason != "oom" {
		t.Fatalf("VM cgroup OOM not mapped to ErrOOM: result=%+v error=%v", out.result, out.err)
	}
}
