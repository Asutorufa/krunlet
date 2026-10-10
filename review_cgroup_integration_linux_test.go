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

// gatedHostPressure runs a directly gated Python workload so a shell cannot
// accidentally consume a closed pipe and report a successful no-op. Each
// child verifies its real /proc/self/cgroup membership before allocating
// memory or attempting forks.
func gatedHostPressure(ctx context.Context, work string) *exec.Cmd {
	program := `import os, sys, subprocess
token = os.read(3, 1)
if token != bytes([1]):
    sys.exit("cgroup startup gate was not released")
group = os.environ["KRUNLET_EXPECT_CGROUP"]
membership = open("/proc/self/cgroup", encoding="utf8").read()
if group not in membership:
    sys.exit("process was NOT attached to cgroup: " + membership)
` + work
	return exec.CommandContext(ctx, "python3", "-c", program)
}

// cgroupPressureDiagnostics captures the actual configured kernel
// limits and counters when a supposedly controlled load survives.
func cgroupPressureDiagnostics(path string) string {
	var diagnostics []string
	for _, name := range []string{"memory.max", "memory.swap.max", "memory.current", "memory.peak", "memory.events", "pids.max", "pids.events"} {
		b, err := os.ReadFile(filepath.Join(path, name))
		if err != nil {
			diagnostics = append(diagnostics, name+"="+err.Error())
		} else {
			diagnostics = append(diagnostics, name+"="+strings.TrimSpace(string(b)))
		}
	}
	return strings.Join(diagnostics, "; ")
}

func TestReviewRealHostCgroupOOMIsDistinguishable(t *testing.T) {
	cg, err := prepareVMCgroup(Options{CgroupParent: integrationCgroup(t), MemoryMiB: 256, CgroupMemoryMaxBytes: 128 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer cg.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := gatedHostPressure(ctx, `import time
mem = bytearray(512 * 1024 * 1024)
for n in range(0, len(mem), 4096): mem[n] = 1
print("unexpected: 512 MiB private allocation survived 128 MiB memory.max", flush=True)
time.sleep(1)
`)
	sup := configureHelper(cmd)
	// configureHelper owns Env; add the expected membership AFTER it.
	cmd.Env = append(cmd.Env, "KRUNLET_EXPECT_CGROUP="+filepath.Base(cg.path))
	var stderr, stdout bytes.Buffer
	cmd.Stderr, cmd.Stdout = &stderr, &stdout
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
		t.Fatalf("missing real cgroup OOM: wait=%v peak=%v events=%+v stderr=%q stdout=%q kernel=%s", waitErr, usage.MemoryPeakBytes, usage, stderr.String(), stdout.String(), cgroupPressureDiagnostics(cg.path))
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
	cmd := gatedHostPressure(ctx, `children = []
for _ in range(80):
    try:
        children.append(subprocess.Popen(["/bin/sleep", "30"]))
    except OSError:
        break
for child in children: child.kill()
for child in children: child.wait()
`)
	sup := configureHelper(cmd)
	cmd.Env = append(cmd.Env, "KRUNLET_EXPECT_CGROUP="+filepath.Base(cg.path))
	var stderr, stdout bytes.Buffer
	cmd.Stderr, cmd.Stdout = &stderr, &stdout
	if err := startHelperInCgroup(sup, cg); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	sup.finish()
	usage, err := cg.usage()
	if err != nil {
		t.Fatal(err)
	}
	if !usage.PidsLimited {
		t.Fatalf("missing real pids.max event: wait=%v events=%+v stderr=%q stdout=%q", waitErr, usage, stderr.String(), stdout.String())
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
	cmd := gatedHostPressure(ctx, `mem = bytearray(1024 * 1024 * 1024)
for n in range(0, len(mem), 4096): mem[n] = 1
`)
	cmd.Env = append(os.Environ(), "KRUNLET_EXPECT_CGROUP="+filepath.Base(group))
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
