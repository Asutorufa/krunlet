//go:build !linux

package krunlet

import "os"

// macOS/APFS and other platforms currently use the portable copy fallback.
// Filesystem clonefile optimization is independent of guest isolation.
func tryReflink(_ *os.File, _ *os.File) (bool, error) { return false, nil }
