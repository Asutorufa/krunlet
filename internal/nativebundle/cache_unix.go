//go:build linux || darwin

package nativebundle

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// The lock inode is stable across processes. O_NOFOLLOW prevents redirects.
func bundleLock(root string) (func(), error) {
	fd, err := syscall.Open(filepath.Join(root, ".native.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, fmt.Errorf("native cache lock: %w", err)
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("native cache flock: %w", err)
	}
	return func() { _ = syscall.Flock(fd, syscall.LOCK_UN); _ = syscall.Close(fd) }, nil
}

func openNativeFile(path string, create bool) (*os.File, error) {
	flags := syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	if create {
		flags = syscall.O_WRONLY | syscall.O_CREAT | syscall.O_EXCL | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	}
	fd, err := syscall.Open(path, flags, 0400)
	if err != nil {
		return nil, fmt.Errorf("open native asset (no-follow) %s: %w", path, err)
	}
	return os.NewFile(uintptr(fd), path), nil
}
