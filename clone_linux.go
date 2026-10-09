//go:build linux

package krunlet

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Filesystem reflinks are copy-on-write and preserve write isolation. Never
// use hardlinks: a guest modifying a cloned rootfs must not alter its template.
func tryReflink(src, dst *os.File) (bool, error) {
	err := unix.IoctlFileClone(int(dst.Fd()), int(src.Fd()))
	if err == nil { return true, nil }
	if errors.Is(err,unix.EOPNOTSUPP) || errors.Is(err,unix.ENOTSUP) ||
		errors.Is(err,unix.EXDEV) || errors.Is(err,unix.ENOTTY) ||
		errors.Is(err,unix.EINVAL) || errors.Is(err,unix.ENOSYS) ||
		errors.Is(err,unix.EPERM) {
		return false,nil
	}
	return false,err
}
