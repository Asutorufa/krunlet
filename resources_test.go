package krunlet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestVMQuotaFastFailAndWait(t *testing.T) {
	root := t.TempDir()
	r, err := New(Options{RootFS: root, MaxConcurrentVMs: 1, FailFast: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = r.acquire(context.Background()); !errors.Is(err, ErrTooManyVMs) {
		t.Fatalf("expected ErrTooManyVMs, got %v", err)
	}
	r.release()
	if err = r.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.release()

	q, err := New(Options{RootFS: root, MaxConcurrentVMs: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = q.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err = q.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected blocked admission timeout, got %v", err)
	}
	q.release()
	if err = q.acquire(context.Background()); err != nil {
		t.Fatalf("slot not returned: %v", err)
	}
	q.release()
}

func TestRootFSQuotaBeforeHelper(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "big"), make([]byte, 4096), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := New(Options{RootFS: root, MaxRootFSBytes: 1024, HelperPath: filepath.Join(root, "missing-helper")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Run(context.Background(), Request{Command: []string{"/bin/true"}})
	if !errors.Is(err, ErrRootFSTooLarge) {
		t.Fatalf("want rootfs limit error, got %v", err)
	}
}

func TestRootFSHostPathSafety(t *testing.T) {
	if _, err := New(Options{RootFS: "."}); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative root accepted: %v", err)
	}
	if _, err := New(Options{RootFS: "/"}); err == nil {
		t.Fatal("host / accepted")
	}
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := New(Options{RootFS: home}); err == nil {
			t.Fatal("host home accepted")
		}
	}
}

func TestStaleRootOnlyDeletesUnlockedSignedDirectories(t *testing.T) {
	base := t.TempDir()
	makeRoot := func(name string) (string, *os.File) {
		t.Helper()
		dir := filepath.Join(base, name)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		f, err := markTempRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, tempRootMarker)
		then := time.Now().Add(-26 * time.Hour)
		if err := os.Chtimes(p, then, then); err != nil {
			t.Fatal(err)
		}
		return dir, f
	}
	live, f := makeRoot("krunlet-root-live")
	old, oldMarker := makeRoot("krunlet-root-old")
	_ = oldMarker.Close()
	// Prefix alone is insufficient; the owner signature is mandatory.
	foreign := filepath.Join(base, "krunlet-root-foreign")
	if err := os.Mkdir(foreign, 0700); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaleRoots(base, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("removed locked root: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("stale root remains: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("removed foreign root: %v", err)
	}
	_ = f.Close()
	if err := cleanupStaleRoots(base, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(live); !os.IsNotExist(err) {
		t.Fatalf("unlocked root remains: %v", err)
	}
}

func TestConcurrent32NoTemporaryRootsLeft(t *testing.T) {
	root := t.TempDir()
	helper := filepath.Join(t.TempDir(), "helper")
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf 'ready\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	r, err := New(Options{RootFS: root, HelperPath: helper, MaxConcurrentVMs: 4})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := r.Run(context.Background(), Request{Command: []string{"/bin/true"}})
			if e != nil {
				errs <- e
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "krunlet-") {
			t.Errorf("leftover temporary resource after 32 runs: %s", entry.Name())
		}
	}
}

func TestTimedOutWhileWaitingForVMQuota(t *testing.T) {
	var observed Stats
	r, err := New(Options{
		RootFS:           t.TempDir(),
		MaxConcurrentVMs: 1,
		Timeout:          35 * time.Millisecond,
		OnStats:          func(stats Stats) { observed = stats },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.release()
	res, err := r.Run(context.Background(), Request{Command: []string{"/bin/true"}})
	if !errors.Is(err, context.DeadlineExceeded) || !res.TimedOut {
		t.Fatalf("queue deadline was not reflected in Result: %+v, %v", res, err)
	}
	if res.RunID == "" || observed.RunID != res.RunID || !observed.TimedOut {
		t.Fatalf("queue deadline was not reflected in Stats: %+v, %+v", res, observed)
	}
	if res.Duration <= 0 {
		t.Fatalf("queue deadline produced empty duration: %+v", res)
	}
}

func TestProcessWideVMAdmissionAcrossTenRunners(t *testing.T) {
	// Global quota is shared even when callers instantiate ten Runners.
	globalLimit := cap(processVMAdmission.permits)
	instances := make([]*Runner,10)
	for i := range instances {
		r,err:=New(Options{RootFS:t.TempDir(),MaxConcurrentVMs:10,FailFast:true})
		if err!=nil {t.Fatal(err)}
		instances[i]=r
	}
	for i:=0;i<globalLimit;i++ {
		if err:=instances[i%len(instances)].acquire(context.Background());err!=nil {t.Fatal(err)}
	}
	if err:=instances[9].acquire(context.Background());!errors.Is(err,ErrTooManyVMs){
		t.Fatalf("global quota not enforced across Runner instances: %v",err)
	}
	for i:=0;i<globalLimit;i++ {instances[i%len(instances)].release()}
	if err:=instances[9].acquire(context.Background());err!=nil{t.Fatalf("global permit leak: %v",err)}
	instances[9].release()
	if err:=ConfigureProcessVMLimit(0);err==nil {t.Fatal("accepted zero global capacity")}
	if err:=ConfigureProcessVMLimit(globalLimit+1);err==nil {t.Fatal("reconfigured live global semaphore")}
}
