//go:build vm_integration

package krunlet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func integrationRootFS(t *testing.T) string {
	t.Helper()
	root := os.Getenv("KRUNLET_TEST_ROOTFS")
	if root == "" {
		t.Skip("set KRUNLET_TEST_ROOTFS to a trusted Linux rootfs")
	}
	return root
}

func TestNativeMinimalVMBoot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	report, err := DoctorDetailed(ctx, "", integrationRootFS(t))
	if err != nil {
		t.Fatalf("real VM smoke failed: %v (%+v)", err, report)
	}
	if !report.SmokeAttempted || report.SmokeExitCode != 0 {
		t.Fatalf("unexpected smoke %+v", report)
	}
}

func TestNativeVMTimeoutReapsHelper(t *testing.T) {
	root := integrationRootFS(t)
	r, err := New(Options{RootFS: root, CPUs: 1, MemoryMiB: 256, Timeout: 1500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	out, err := r.Run(context.Background(), Request{Command: []string{"/bin/sh", "-c", "sleep 30"}})
	if !errors.Is(err, context.DeadlineExceeded) || !out.TimedOut {
		t.Fatalf("expected timeout and shutdown: %+v err=%v", out, err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("helper did not stop promptly")
	}
}

func TestNative32RunsBounded(t *testing.T) {
	if os.Getenv("KRUNLET_STRESS_32") != "1" {
		t.Skip("set KRUNLET_STRESS_32=1 to enable full VM stress")
	}
	root := integrationRootFS(t)
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	r, err := New(Options{RootFS: root, CPUs: 1, MemoryMiB: 256, MaxConcurrentVMs: 4})
	if err != nil {
		t.Fatal(err)
	}
	const count = 32
	errorsChan := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, e := r.Run(context.Background(), Request{Command: []string{"/bin/true"}})
			if e != nil {
				errorsChan <- e
				return
			}
			if res.ExitCode != 0 {
				errorsChan <- fmt.Errorf("unexpected exit code %d", res.ExitCode)
			}
		}()
	}
	wg.Wait()
	close(errorsChan)
	for e := range errorsChan {
		t.Error(e)
	}
	entries, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "krunlet-") {
			t.Errorf("VM stress left a temporary resource: %s", entry.Name())
		}
	}
	// The helper's argv includes --config under the scratch directory.
	// Confirm ps sees no surviving child from this particular test run.
	if output, err := exec.Command("ps", "-eo", "args").Output(); err == nil && bytes.Contains(output, []byte(scratch)) {
		t.Fatalf("VM stress left a helper process referencing %s", scratch)
	}
}
