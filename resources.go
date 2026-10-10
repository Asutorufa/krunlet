package krunlet

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"runtime"
	"sync"
	"time"
)

var (
	ErrTooManyVMs     = errors.New("maximum concurrent VMs reached")
	ErrRootFSTooLarge = errors.New("rootfs exceeds MaxRootFSBytes")
)

// Process-wide VM admission applies across *all* Runner instances in the
// current Go process. ConfigureProcessVMLimit must be called before starting
// any Runner execution. Separate processes require an external supervisor.
var processVMAdmission = struct {
	sync.Mutex
	permits chan struct{}
	started bool
}{
	permits: make(chan struct{}, defaultVMLimit()),
}

func defaultVMLimit() int {
	n := runtime.NumCPU() / 2
	if n < 1 { return 1 }
	if n > 4 { return 4 }
	return n
}

// ConfigureProcessVMLimit sets an upper bound shared by all Runners in this
// process. Reconfiguration after the first admission attempt is prohibited
// to avoid orphaned permits or changing limits under contention.
func ConfigureProcessVMLimit(max int) error {
	if max < 1 { return errors.New("process VM limit must be positive") }
	processVMAdmission.Lock()
	defer processVMAdmission.Unlock()
	if processVMAdmission.started { return errors.New("process VM limit cannot change after first VM admission") }
	processVMAdmission.permits = make(chan struct{}, max)
	return nil
}

func (r *Runner) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil { return err }
	processVMAdmission.Lock()
	processVMAdmission.started = true
	global := processVMAdmission.permits
	processVMAdmission.Unlock()

	// Reserve the per-Runner slot first. In both paths, if admission to the
	// process-wide quota fails, release the local reservation immediately.
	if r.cfg.FailFast {
		select {
		case r.permits <- struct{}{}:
		default:
			return ErrTooManyVMs
		}
		select {
		case global <- struct{}{}:
			return nil
		default:
			<-r.permits
			return ErrTooManyVMs
		}
	}
	select {
	case r.permits <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case global <- struct{}{}:
		return nil
	case <-ctx.Done():
		<-r.permits
		return ctx.Err()
	}
}

func (r *Runner) release() {
	<-r.permits
	processVMAdmission.Lock()
	global := processVMAdmission.permits
	processVMAdmission.Unlock()
	<-global
}

) { <-r.permits }

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
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "krunlet-root-") &&
			!strings.HasPrefix(name, "krunlet-session-") &&
			!strings.HasPrefix(name, "krunlet-template-") &&
			!strings.HasPrefix(name, "krunlet-net-") &&
			!strings.HasPrefix(name, "krunlet-state-") {
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
