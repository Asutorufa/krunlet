package krunlet

import (
	"context"
	"errors"
	"testing"
	"time"
)

// This check uses two independent Runner objects to prove that creating more
// Runners cannot multiply the process-wide global VM cap.
func TestProcessGlobalQuotaAcrossRunners(t *testing.T) {
	previous := GlobalVMLimit()
	if err := SetGlobalVMLimit(1); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := SetGlobalVMLimit(previous); err != nil {
			t.Error(err)
		}
	}()
	r1, err := New(Options{RootFS: t.TempDir(), MaxConcurrentVMs: 4})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := New(Options{RootFS: t.TempDir(), MaxConcurrentVMs: 4, FailFast: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := r1.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r2.acquire(context.Background()); !errors.Is(err, ErrTooManyVMs) {
		t.Fatalf("a second Runner bypassed the global quota: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	r2.cfg.FailFast = false
	if err := r2.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("global admission wait returned %v", err)
	}
	r1.release()
	if err := r2.acquire(context.Background()); err != nil {
		t.Fatalf("global permit not released: %v", err)
	}
	r2.release()
}

func TestCgroupConfigurationRejectsInvalidRoots(t *testing.T) {
	if _, err := New(Options{RootFS: t.TempDir(), CgroupParent: "relative/path"}); err == nil {
		t.Fatal("relative cgroup parent allowed")
	}
	if _, err := New(Options{RootFS: t.TempDir(), CgroupParent: "/sys/fs/cgroup/test", HelperPath: "/bin/sh"}); err == nil {
		t.Fatal("custom helper bypassed cgroup startup gate")
	}
	if _, err := New(Options{RootFS: t.TempDir(), CgroupMemoryMaxBytes: -1}); err == nil {
		t.Fatal("invalid cgroup memory maximum accepted")
	}
}
