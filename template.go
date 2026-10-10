package krunlet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// Template owns a private, trusted rootfs snapshot. The first preparation
// copies the input directory once; subsequent runs can clone files using
// Linux FICLONE reflinks with portable copy fallback. Call Close after all
// runners and sessions created from it have completed.
type Template struct {
	mu     sync.Mutex
	dir    string
	root   string
	marker *os.File
	closed bool
}

// PrepareTemplate stages a private immutable-by-convention rootfs source.
// The caller must not modify Template.RootFS(); treat the source as trusted.
func PrepareTemplate(dir string) (*Template, error) {
	validated, err := New(Options{RootFS: dir})
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	if err := checkRootFSSize(ctx, validated.cfg.RootFS, validated.cfg.MaxRootFSBytes); err != nil {
		return nil, err
	}
	temp, err := os.MkdirTemp("", "krunlet-template-*")
	if err != nil {
		return nil, err
	}
	marker, err := markTempRoot(temp)
	if err != nil {
		_ = os.RemoveAll(temp)
		return nil, err
	}
	root := filepath.Join(temp, "rootfs")
	if err = os.Mkdir(root, 0700); err == nil {
		err = copyRootFS(ctx, validated.cfg.RootFS, root)
	}
	if err != nil {
		_ = marker.Close()
		_ = os.RemoveAll(temp)
		return nil, err
	}
	return &Template{dir: temp, root: root, marker: marker}, nil
}

// NewRunner binds a runner to this template snapshot. Each non-persistent
// invocation clones independently; caller-provided RootFS is ignored.
func (t *Template) NewRunner(opts Options) (*Runner, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, errors.New("template is closed")
	}
	opts.RootFS = t.root
	opts.Persistent = false // always isolated, even when caller requests otherwise
	return New(opts)
}

func (t *Template) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	if t.marker != nil {
		_ = t.marker.Close()
		t.marker = nil
	}
	return os.RemoveAll(t.dir)
}
