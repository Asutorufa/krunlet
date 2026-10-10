package krunlet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSessionMarkerIsOutsideGuestRoot(t *testing.T) {
	session, err := NewSession(context.Background(), Options{RootFS: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	root := session.root
	container := filepath.Dir(root)
	defer func() { _ = session.Close() }()
	if _, err := os.Lstat(filepath.Join(root, tempRootMarker)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("guest can access cleanup marker: %v", err)
	}
	if _, err := os.Stat(filepath.Join(container, tempRootMarker)); err != nil {
		t.Fatalf("cleanup marker is absent outside guest root: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(container); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session container not cleaned up: %v", err)
	}
}

func TestPersistentVMEarlyHelperExitReleasesResources(t *testing.T) {
	script := filepath.Join(t.TempDir(), "helper")
	body := "#!/bin/sh\nprintf 'KRUNLET_READY\\n'\nsleep 0.2\nexit 42\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	vm, err := NewVM(context.Background(), Options{
		RootFS: t.TempDir(), HelperPath: script, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	container := filepath.Dir(vm.session.root)
	pid := vm.cmd.Process.Pid
	defer func() { _ = vm.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		vm.mu.Lock()
		closed := vm.closed && vm.session == nil
		vm.mu.Unlock()
		if closed {
			if _, err := os.Stat(container); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rootfs retained after helper exited: %v", err)
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("helper process still present, pid=%d: %v", pid, err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("persistent VM helper exit did not trigger Close and cleanup")
}
