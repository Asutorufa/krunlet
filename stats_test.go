package krunlet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStatsCallbackOneShot(t *testing.T) {
	root := t.TempDir()
	helper := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf 'stats-tested\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var received []Stats
	r, err := New(Options{RootFS: root, HelperPath: helper, OnStats: func(s Stats) { received = append(received, s) }})
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Run(context.Background(), Request{Command: []string{"/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.RunID == "" || len(received) != 1 || received[0].RunID != result.RunID {
		t.Fatalf("run ID not linked between result and stats: %+v, %+v", result, received)
	}
	if received[0].ExitCode != 0 || received[0].StartupReadyMillis != -1 || received[0].RunElapsedMillis < 0 {
		t.Fatalf("unexpected stats: %+v", received[0])
	}
}

func TestDoctorMissingNativeLibraryReadableError(t *testing.T) {
	err := Doctor(filepath.Join(t.TempDir(), "definitely-missing-library"))
	if err == nil || strings.TrimSpace(err.Error()) == "" {
		t.Fatalf("missing lib not reported: %v", err)
	}
}

func TestPersistentVMStatsReady(t *testing.T) {
	var received []Stats
	vm, err := NewVM(context.Background(), Options{
		RootFS: t.TempDir(), HelperPath: fakeLiveHelper(t, false),
		Timeout: 2 * time.Second,
		OnStats: func(s Stats) { received = append(received, s) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close()
	res, err := vm.Run(context.Background(), Request{Command: []string{"/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 || received[0].RunID != res.RunID || received[0].StartupReadyMillis < 0 {
		t.Fatalf("missing VM readiness stats: %+v", received)
	}
}
