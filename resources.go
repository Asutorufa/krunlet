package krunlet

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrTooManyVMs     = errors.New("maximum concurrent VMs reached")
	ErrRootFSTooLarge = errors.New("rootfs exceeds MaxRootFSBytes")
)

func (r *Runner) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.cfg.FailFast {
		select {
		case r.permits <- struct{}{}:
			return nil
		default:
			return ErrTooManyVMs
		}
	}
	select {
	case r.permits <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runner) release() { <-r.permits }

func checkRootFSSize(ctx context.Context, root string, maxBytes int64) error {
	var size int64
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported special rootfs file %q", path)
		}
		if info.Size() > maxBytes-size {
			return ErrRootFSTooLarge
		}
		size += info.Size()
		return nil
	})
}

const tempRootMarker = ".krunlet-owner"
const tempRootSignature = "krunlet-temporary-root-v1\n"

// The marker file stays open and exclusively locked for the entire rootfs
// lifetime. A cleanup run must never remove a live instance's rootfs.
func markTempRoot(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, tempRootMarker), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	if _, err = f.WriteString(tempRootSignature); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err = lockTempMarker(f, false); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func cleanupStaleRoots(base string, olderThan time.Duration) error {
	entries, err := os.ReadDir(base)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-olderThan)
	for _, entry := range entries {
		if !entry.IsDir() ||
			!(strings.HasPrefix(entry.Name(), "krunlet-root-") || strings.HasPrefix(entry.Name(), "krunlet-session-") || strings.HasPrefix(entry.Name(), "krunlet-template-")) {
			continue
		}
		dir := filepath.Join(base, entry.Name())
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			continue
		}
		marker := filepath.Join(dir, tempRootMarker)
		mi, err := os.Lstat(marker)
		if err != nil || !mi.Mode().IsRegular() || mi.ModTime().After(cutoff) {
			continue
		}
		f, err := openTempMarker(marker)
		if err != nil {
			continue
		}
		if err := lockTempMarker(f, true); err != nil {
			_ = f.Close()
			continue
		}
		var buf [64]byte
		n, err := f.Read(buf[:])
		if err == nil || errors.Is(err, os.ErrClosed) {
			// Files with extra bytes are not recognized as our marker.
			if string(buf[:n]) == tempRootSignature {
				_ = os.RemoveAll(dir)
			}
		}
		_ = f.Close()
	}
	return nil
}
