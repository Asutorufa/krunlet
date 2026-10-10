//go:build linux

package nativebundle

import (
	"fmt"
	"syscall"
)

func checkExecutableCache(path string) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil { return fmt.Errorf("inspect native cache mount %s: %w", path, err) }
	if stat.Flags&syscall.MS_NOEXEC != 0 {
		return fmt.Errorf("native cache filesystem is noexec at %s; set XDG_CACHE_HOME to a private executable filesystem", path)
	}
	return nil
}
