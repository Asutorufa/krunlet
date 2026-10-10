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
	// MNT_NOEXEC = 0x00000004 in Darwin sys/mount.h; Go syscall does not export it.
	const mntNoExec = 0x00000004
	if stat.Flags&mntNoExec != 0 {
		return fmt.Errorf("native cache filesystem is noexec at %s; use an executable private cache directory", path)
	}
	return nil
}
