//go:build linux && vm_integration

package krunlet

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReviewOrphanCgroupReapedWithoutAgeDelay(t *testing.T) {
	parent := integrationCgroup(t)
	orphan, err := os.MkdirTemp(parent, "krunlet-vm-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(orphan)
	// Deliberately fresh: a crashed owner must not wait 24 hours for recovery.
	if err := cleanupStaleVMGroups(parent, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan cgroup still exists after startup recovery: %v", err)
	}
}
func TestReviewCgroupFilesNeverBecomeNormalFiles(t *testing.T) {
	parent := integrationCgroup(t)
	cg, err := prepareVMCgroup(Options{CgroupParent: parent, MemoryMiB: 256})
	if err != nil {
		t.Fatal(err)
	}
	defer cg.Close()
	if _, err := os.Stat(filepath.Join(cg.path, "cgroup.kill")); err != nil {
		t.Fatal(err)
	}
}
