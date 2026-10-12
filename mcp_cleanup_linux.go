//go:build linux

package krunlet

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func reclaimMCPVMGroups(parent string) error {
	if parent == "" {
		return nil
	}
	if !filepath.IsAbs(parent) {
		return errors.New("cgroup parent must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return fmt.Errorf("resolve cgroup parent: %w", err)
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(resolved, &stat); err != nil {
		return err
	}
	if stat.Type != 0x63677270 {
		return errors.New("cgroup parent must be on a cgroup v2 filesystem")
	}
	if _, err := os.Stat(filepath.Join(resolved, "cgroup.controllers")); err != nil {
		return fmt.Errorf("cgroup v2 delegation required: %w", err)
	}
	return cleanupStaleVMGroups(resolved, 0)
}
