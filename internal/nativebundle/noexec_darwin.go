//go:build darwin

package nativebundle

import (
	"fmt"
	"syscall"
)

func checkExecutableCache(path string) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return fmt.Errorf("inspect native cache mount %s: %w", path, err)
	}
	if stat.Flags&syscall.MNT_NOEXEC != 0 {
		return fmt.Errorf("native cache filesystem is noexec at %s; use an executable private cache directory", path)
	}
	return nil
}
