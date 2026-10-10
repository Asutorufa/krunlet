package nativebundle

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func sample(t *testing.T) (string, string, []byte, []byte) {
	t.Helper()
	lib, fw := names()
	if lib == "" {
		t.Skip("unsupported platform")
	}
	return lib, fw, []byte("test-only-libkrun-payload"), []byte("test-only-firmware-payload")
}

func TestABIMismatchIsDistinguishable(t *testing.T) {
	if err := checkABI(5, 4); !errors.Is(err, ErrABIMismatch) {
		t.Fatalf("wrong ABI should be distinguishable: %v", err)
	}
	if err := checkABI(5, 5); err != nil {
		t.Fatalf("matching ABI rejected: %v", err)
	}
}

func TestCacheCorruptionSelfHeals(t *testing.T) {
	lib, fw, lb, fb := sample(t)
	root := filepath.Join(t.TempDir(), "private")
	a, err := stageBundle(root, lib, fw, lb, fb)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(a.Library, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.Library, []byte("corrupted"), 0400); err != nil {
		t.Fatal(err)
	}
	b, err := stageBundle(root, lib, fw, lb, fb)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(b.Library)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, lb) {
		t.Fatalf("corrupt cache not rebuilt")
	}
	if a.Library != b.Library || !b.Integrity {
		t.Fatalf("unexpected resolution: %#v", b)
	}
}

func TestUnsafeCacheSymlinkRejected(t *testing.T) {
	lib, fw, lb, fb := sample(t)
	root := filepath.Join(t.TempDir(), "private")
	a, err := stageBundle(root, lib, fw, lb, fb)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "other")
	if err := os.WriteFile(dest, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(a.Library); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dest, a.Library); err != nil {
		t.Fatal(err)
	}
	if _, err := stageBundle(root, lib, fw, lb, fb); err == nil {
		t.Fatal("accepted malicious symlink")
	}
	out, _ := os.ReadFile(dest)
	if string(out) != "untouched" {
		t.Fatal("symlink target modified")
	}
}

func TestCachePermissionsAndHash(t *testing.T) {
	lib, fw, lb, fb := sample(t)
	a, err := stageBundle(filepath.Join(t.TempDir(), "private"), lib, fw, lb, fb)
	if err != nil {
		t.Fatal(err)
	}
	if a.Source != "embedded" || !a.Integrity || len(a.LibrarySHA256) != 64 || len(a.FirmwareSHA256) != 64 {
		t.Fatalf("%#v", a)
	}
	st, err := os.Stat(filepath.Dir(a.Library))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0700 {
		t.Fatalf("directory permissions: %o", st.Mode().Perm())
	}
	for _, p := range []string{a.Library, a.Firmware} {
		f, e := os.Lstat(p)
		if e != nil {
			t.Fatal(e)
		}
		if !f.Mode().IsRegular() || f.Mode().Perm()&0222 != 0 {
			t.Fatalf("unsafe file %s: %o", p, f.Mode().Perm())
		}
	}
}

func TestConcurrentProcessesSingleCache(t *testing.T) {
	lib, fw, lb, fb := sample(t)
	root := os.Getenv("KRUNLET_CACHE_ROOT")
	if root == "" {
		root = filepath.Join(t.TempDir(), "private")
	}
	if os.Getenv("KRUNLET_CACHE_CHILD") == "1" {
		_, err := stageBundle(root, lib, fw, lb, fb)
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	const n = 5
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run", "^TestConcurrentProcessesSingleCache$")
			cmd.Env = append(os.Environ(), "KRUNLET_CACHE_CHILD=1", "KRUNLET_CACHE_ROOT="+root)
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs <- errors.New(string(out) + ": " + err.Error())
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	ds, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	// One lock file is allowed; exactly one immutable content directory.
	count := 0
	for _, d := range ds {
		if d.IsDir() && !strings.HasPrefix(d.Name(), ".") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("found %d bundle directories", count)
	}
}

func TestNoExecLocationReportsReadableError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux mount test")
	}
	if _, err := os.Stat("/dev/shm"); err != nil {
		t.Skip(err)
	}
	err := checkExecutableCache("/dev/shm")
	if err == nil {
		t.Skip("/dev/shm is executable on this host")
	}
	if !strings.Contains(err.Error(), "noexec") {
		t.Fatalf("unexpected noexec error: %v", err)
	}
}

func TestGlibcFloorHumanReadable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("glibc only")
	}
	err := checkGlibcBaseline("999.0")
	if err == nil || !strings.Contains(err.Error(), "glibc") || !strings.Contains(err.Error(), "999.0") {
		t.Fatalf("missing readable old-glibc error: %v", err)
	}
}
