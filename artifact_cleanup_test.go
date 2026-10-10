//go:build linux || darwin

package krunlet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayUnixSocketCleanupAndMarker(t *testing.T) {
	lease, err := prepareNetwork(context.Background(), &NetworkPolicy{Mode: NetworkBlocklist})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(lease.socket)
	if _, err := os.Stat(filepath.Join(dir, tempRootMarker)); err != nil {
		lease.Close()
		t.Fatalf("gateway cleanup marker missing: %v", err)
	}
	if _, err := os.Stat(lease.socket); err != nil {
		lease.Close()
		t.Fatalf("gateway socket missing: %v", err)
	}
	lease.Close()
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("gateway directory survived Close: %v", err)
	}
}

func TestPersistentRunnerScratchIsReclaimed(t *testing.T) {
	root := t.TempDir()
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	helper := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	runner, err := New(Options{RootFS: root, HelperPath: helper, Persistent: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), Request{Command: []string{"/bin/true"}}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "krunlet-state-") {
			t.Errorf("orphaned helper scratch directory: %s", entry.Name())
		}
	}
}
