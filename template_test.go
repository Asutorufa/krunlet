package krunlet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestCloneFallbackAndTemplateIsolation(t *testing.T) {
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "work"), 0700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(source, "work", "data.txt")
	if err := os.WriteFile(input, []byte("template"), 0600); err != nil {
		t.Fatal(err)
	}
	tpl, err := PrepareTemplate(source)
	if err != nil {
		t.Fatal(err)
	}
	defer tpl.Close()

	type clone struct{ dir string }
	clones := make([]clone, 2)
	var wg sync.WaitGroup
	for i := range clones {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dest, e := os.MkdirTemp("", "krunlet-test-clone-")
			if e != nil {
				t.Error(e)
				return
			}
			clones[i].dir = dest
			if e = copyRootFS(context.Background(), tpl.root, dest); e != nil {
				t.Error(e)
				return
			}
			payload := []byte{byte('a' + i)}
			if e = os.WriteFile(filepath.Join(dest, "work", "data.txt"), payload, 0600); e != nil {
				t.Error(e)
			}
		}(i)
	}
	wg.Wait()
	for i, c := range clones {
		if c.dir == "" {
			t.Fatal("clone missing")
		}
		defer os.RemoveAll(c.dir)
		data, err := os.ReadFile(filepath.Join(c.dir, "work", "data.txt"))
		if err != nil || len(data) != 1 || data[0] != byte('a'+i) {
			t.Fatalf("clone %d not isolated: %s %v", i, data, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(tpl.root, "work", "data.txt"))
	if err != nil || string(data) != "template" {
		t.Fatalf("template mutated: %q %v", data, err)
	}
	if _, err = tpl.NewRunner(Options{}); err != nil {
		t.Fatal(err)
	}
	if err = tpl.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = tpl.NewRunner(Options{}); err == nil {
		t.Fatal("closed template accepted")
	}
}

func TestCloneFileFallbackWithoutReflinks(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	dst := filepath.Join(t.TempDir(), "dst")
	if err := os.WriteFile(src, []byte("copy-fallback"), 0600); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if err = cloneFileData(context.Background(), in, out, func(*os.File, *os.File) (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	_ = out.Close()
	data, err := os.ReadFile(dst)
	if err != nil || string(data) != "copy-fallback" {
		t.Fatalf("fallback copied %q: %v", data, err)
	}
	_, err = PrepareTemplate("/this/file/does/not/exist")
	if err == nil {
		t.Fatal("invalid template path accepted")
	}
	if err = cloneFileData(context.Background(), in, out, func(*os.File, *os.File) (bool, error) { return false, errors.New("clone failure") }); err == nil {
		t.Fatal("unexpectedly ignored non-filesystem error")
	}
}
