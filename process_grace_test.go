//go:build linux || darwin

package krunlet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHelperProcessGroupGracePeriod(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "grace.done")
	t.Setenv("KRUNLET_GRACE_MARKER", marker)
	helper := filepath.Join(t.TempDir(), "helper")
	script := "#!/bin/sh\ntrap 'sleep 0.2; printf graceful > \"$KRUNLET_GRACE_MARKER\"; exit 0' TERM\nwhile :; do sleep 0.1; done\n"
	if err := os.WriteFile(helper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	r, err := New(Options{RootFS: root, HelperPath: helper, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	begin := time.Now()
	result, err := r.Run(context.Background(), Request{Command: []string{"/bin/true"}})
	if !errors.Is(err, context.DeadlineExceeded) || !result.TimedOut {
		t.Fatalf("expected timeout, got result=%+v error=%v", result, err)
	}
	if elapsed := time.Since(begin); elapsed < 2*time.Second {
		t.Fatalf("process-group grace prematurely shortened: %v", elapsed)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "graceful" {
		t.Fatalf("helper TERM handler did not complete: %q, %v", data, err)
	}
}
