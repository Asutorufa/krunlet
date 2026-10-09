package krunlet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestGuestPaths(t *testing.T) {
	root := t.TempDir()
	for _, s := range []string{"../etc/passwd", "/a/../b", "a.txt", "/\x00"} {
		if _, e := checkedHostPath(root, s); e == nil {
			t.Errorf("accepted unsafe path %q", s)
		}
	}
	if e := os.Mkdir(filepath.Join(root, "dir"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink("/tmp", filepath.Join(root, "dir", "escape")); e != nil {
		t.Fatal(e)
	}
	if _, e := checkedHostPath(root, "/dir/escape/secret"); e == nil {
		t.Fatal("accepted symlink escape")
	}
	if p, e := checkedHostPath(root, "/dir/new.txt"); e != nil || p != filepath.Join(root, "dir", "new.txt") {
		t.Fatalf("got %s, %v", p, e)
	}
}
func TestPortAndRLimit(t *testing.T) {
	for _, s := range []string{"0:80", "a:b", "123:", "65536:123"} {
		if validatePortMap(s) == nil {
			t.Fatal(s)
		}
	}
	if e := validatePortMap("8888:80"); e != nil {
		t.Fatal(e)
	}
	for _, s := range []string{"7=512:512", "6=100:100"} {
		if e := validateRLimit(s); e != nil {
			t.Fatal(e)
		}
	}
	for _, s := range []string{"NOFILE=1:1", "7=bad:1", "7=2"} {
		if validateRLimit(s) == nil {
			t.Fatal(s)
		}
	}
}
func TestOutputBudget(t *testing.T) {
	b := &outputBudget{limit: 8}
	w1 := &limitedWriter{budget: b}
	w2 := &limitedWriter{budget: b}
	_, _ = w1.Write([]byte("123456"))
	_, _ = w2.Write([]byte("abcdef"))
	if !b.exceeded() || len(w1.String())+len(w2.String()) != 8 {
		t.Fatalf("over=%t w1=%q w2=%q", b.exceeded(), w1.String(), w2.String())
	}
}
func TestCopyAndExchange(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	if e := os.Mkdir(filepath.Join(src, "tmp"), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(src, "tmp", "a"), []byte("hello"), 0644); e != nil {
		t.Fatal(e)
	}
	if e := copyRootFS(context.Background(), src, dst); e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(filepath.Join(dst, "tmp", "a"))
	if e != nil || string(data) != "hello" {
		t.Fatalf("file %q %v", data, e)
	}
	if e := os.WriteFile(filepath.Join(dst, "tmp", "a"), []byte("changed"), 0644); e != nil {
		t.Fatal(e)
	}
	data, _ = os.ReadFile(filepath.Join(src, "tmp", "a"))
	if string(data) != "hello" {
		t.Fatal("modified original rootfs")
	}
}
func TestRunFakeHelper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX script")
	}
	root := t.TempDir()
	script := filepath.Join(t.TempDir(), "helper")
	if e := os.WriteFile(script, []byte("#!/bin/sh\necho hello\necho alert >&2\nexit 7\n"), 0700); e != nil {
		t.Fatal(e)
	}
	r, e := New(Options{RootFS: root, HelperPath: script, Timeout: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	result, e := r.Run(context.Background(), Request{Command: []string{"/bin/true"}, Files: map[string][]byte{"/workspace/x": []byte("value")}, Collect: []string{"/workspace/x"}})
	if e != nil {
		t.Fatal(e)
	}
	if result.ExitCode != 7 || !strings.Contains(result.Stdout, "hello") || !strings.Contains(result.Stderr, "alert") || string(result.Files["/workspace/x"]) != "value" {
		t.Fatalf("result: %+v", result)
	}
}
func TestRunTimeout(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(t.TempDir(), "helper")
	if e := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 3\n"), 0700); e != nil {
		t.Fatal(e)
	}
	r, e := New(Options{RootFS: root, HelperPath: script, Timeout: 20 * time.Millisecond})
	if e != nil {
		t.Fatal(e)
	}
	result, e := r.Run(context.Background(), Request{Command: []string{"/bin/true"}})
	if !errors.Is(e, context.DeadlineExceeded) || !result.TimedOut {
		t.Fatalf("result %+v err %v", result, e)
	}
}
func TestRunOutputLimit(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(t.TempDir(), "helper")
	if e := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'abcdefghijklmno'\n"), 0700); e != nil {
		t.Fatal(e)
	}
	r, e := New(Options{RootFS: root, HelperPath: script, MaxOutputBytes: 4})
	if e != nil {
		t.Fatal(e)
	}
	result, e := r.Run(context.Background(), Request{Command: []string{"/bin/true"}})
	if e == nil || !result.OutputLimited || len(result.Stdout) != 4 {
		t.Fatalf("result %+v err %v", result, e)
	}
}

func TestLibraryDoesNotRequireCLI(t *testing.T) {
	r, err := New(Options{RootFS: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !r.autoHelper || r.cfg.HelperPath != exe {
		t.Fatalf("helper = %q (auto=%t), want %q", r.cfg.HelperPath, r.autoHelper, exe)
	}
}
