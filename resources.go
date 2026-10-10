package krunlet

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"runtime"
	"time"
)

var (
	ErrTooManyVMs     = errors.New("maximum concurrent VMs reached")
	ErrRootFSTooLarge = errors.New("rootfs exceeds MaxRootFSBytes")
)

// globalVMGate is shared by every Runner in this Go process, including
// sessions and live VMs. The default is max(1, min(4, NumCPU/2)).
var globalVMGate = struct {
	mu sync.Mutex
	used int
	max int
	changed chan struct{}
}{max:defaultVMConcurrency(),changed:make(chan struct{})}

func defaultVMConcurrency() int {
	n:=runtime.NumCPU()/2
	if n<1{return 1}
	if n>4{return 4}
	return n
}

// SetGlobalVMLimit changes the process-wide VM concurrency limit.
// The limit is shared by all Runner instances. A change may be made only
// when no VM is active; independent OS processes need host quotas/cgroups.
func SetGlobalVMLimit(n int) error {
	if n<1{return errors.New("global VM limit must be positive")}
	globalVMGate.mu.Lock()
	defer globalVMGate.mu.Unlock()
	if globalVMGate.used!=0{return errors.New("cannot adjust global VM limit while VMs are running")}
	globalVMGate.max=n
	close(globalVMGate.changed)
	globalVMGate.changed=make(chan struct{})
	return nil
}

func GlobalVMLimit() int {
	globalVMGate.mu.Lock()
	defer globalVMGate.mu.Unlock()
	return globalVMGate.max
}

func acquireGlobalVM(ctx context.Context, failFast bool) error {
	for {
		if err:=ctx.Err();err!=nil{return err}
		globalVMGate.mu.Lock()
		if globalVMGate.used<globalVMGate.max {
			globalVMGate.used++
			globalVMGate.mu.Unlock()
			return nil
		}
		changed:=globalVMGate.changed
		globalVMGate.mu.Unlock()
		if failFast{return ErrTooManyVMs}
		select {
		case <-changed:
		case <-ctx.Done():return ctx.Err()
		}
	}
}

func releaseGlobalVM() {
	globalVMGate.mu.Lock()
	defer globalVMGate.mu.Unlock()
	globalVMGate.used--
	close(globalVMGate.changed)
	globalVMGate.changed=make(chan struct{})
}

func (r *Runner) acquire(ctx context.Context) error {
	if err := acquireGlobalVM(ctx,r.cfg.FailFast);err!=nil{return err}
	if err := ctx.Err(); err != nil {
		releaseGlobalVM()
		return err
	}
	if r.cfg.FailFast {
		select {
		case r.permits <- struct{}{}:
			return nil
		default:
			releaseGlobalVM()
			return ErrTooManyVMs
		}
	}
	select {
	case r.permits <- struct{}{}:
		return nil
	case <-ctx.Done():
		releaseGlobalVM()
		return ctx.Err()
	}
}

func (r *Runner) release() {
	<-r.permits
	releaseGlobalVM()
}

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
