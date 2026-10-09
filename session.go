package krunlet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Session holds a disposable rootfs whose file changes survive across calls.
// Each Run still starts a NEW VM (libkrun 1.x does not expose an interactive
// multi-exec session). All operations are serialized to avoid races.
type Session struct {
	mu     sync.Mutex
	closed bool
	root   string
	marker *os.File
	runner *Runner
}

// NewSession clones the trusted base rootfs once. Call Close when done.
func NewSession(ctx context.Context, opts Options) (*Session, error) {
	validated, err := New(opts)
	if err != nil {
		return nil, err
	}
	opts = validated.cfg
	_ = cleanupStaleRoots(os.TempDir(), 24*time.Hour)
	// Preserve the self-reexec helper mode across the second New call.
	// Otherwise, an auto-resolved HelperPath looks like an explicit CLI path.
	if validated.autoHelper {
		opts.HelperPath = ""
	}
	root := opts.RootFS
	if err := checkRootFSSize(ctx, root, opts.MaxRootFSBytes); err != nil {
		return nil, fmt.Errorf("rootfs preflight: %w", err)
	}
	temp, err := os.MkdirTemp("", "krunlet-session-*")
	if err != nil {
		return nil, err
	}
	marker, err := markTempRoot(temp)
	if err != nil {
		_ = os.RemoveAll(temp)
		return nil, err
	}
	if err = copyRootFS(ctx, root, temp); err != nil {
		_ = marker.Close()
		_ = os.RemoveAll(temp)
		return nil, err
	}
	opts.RootFS = temp
	opts.Persistent = true
	r, err := New(opts)
	if err != nil {
		_ = marker.Close()
		_ = os.RemoveAll(temp)
		return nil, err
	}
	return &Session{runner: r, root: temp, marker: marker}, nil
}

func (s *Session) Run(ctx context.Context, req Request) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Result{}, errors.New("session closed")
	}
	return s.runner.Run(ctx, req)
}

// RunIO streams a one-shot VM execution with a serialized reusable rootfs.
func (s *Session) RunIO(ctx context.Context, req Request, stdin io.Reader, stdout, stderr io.Writer) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Result{}, errors.New("session closed")
	}
	return s.runner.RunIO(ctx, req, stdin, stdout, stderr)
}

// RunStream forwards output as it arrives from a fresh VM in this session.
func (s *Session) RunStream(ctx context.Context, req Request, stdout, stderr io.Writer) (Result, error) {
	return s.RunIO(ctx, req, nil, stdout, stderr)
}

func (s *Session) Shell(ctx context.Context, script string) (Result, error) {
	return s.Run(ctx, Request{Command: []string{"/bin/sh", "-lc", script}})
}

func (s *Session) WriteFile(guest string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("session closed")
	}
	if int64(len(data)) > s.runner.cfg.MaxFileBytes {
		return errors.New("file exceeds limit")
	}
	p, err := checkedHostPath(s.root, guest)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	p, err = checkedHostPath(s.root, guest)
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0600)
}
func (s *Session) ReadFile(guest string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("session closed")
	}
	p, err := checkedHostPath(s.root, guest)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > s.runner.cfg.MaxFileBytes {
		return nil, fmt.Errorf("unsafe or oversized file: %s", guest)
	}
	return os.ReadFile(p)
}
func (s *Session) RemoveFile(guest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("session closed")
	}
	p, err := checkedHostPath(s.root, guest)
	if err != nil {
		return err
	}
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("only regular files may be removed")
	}
	return os.Remove(p)
}
func (s *Session) ListDir(guest string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("session closed")
	}
	// Allow listing / without allowing ReadFile or WriteFile to /.
	var p string
	if guest == "/" {
		p = s.root
	} else {
		var err error
		p, err = checkedHostPath(s.root, guest)
		if err != nil {
			return nil, err
		}
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out, nil
}
func (s *Session) RunBatch(ctx context.Context, requests []Request) ([]Result, error) {
	results := make([]Result, 0, len(requests))
	for i, req := range requests {
		res, err := s.Run(ctx, req)
		results = append(results, res)
		if err != nil {
			return results, fmt.Errorf("command %d: %w", i, err)
		}
		if res.ExitCode != 0 {
			return results, nil
		}
	}
	return results, nil
}
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.marker != nil {
		_ = s.marker.Close()
		s.marker = nil
	}
	return os.RemoveAll(s.root)
}
