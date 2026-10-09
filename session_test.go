package krunlet

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSessionFiles(t *testing.T) {
	src := t.TempDir()
	helper := filepath.Join(t.TempDir(), "helper")
	if e := os.WriteFile(helper, []byte("#!/bin/sh\nexit 0\n"), 0700); e != nil {
		t.Fatal(e)
	}
	s, e := NewSession(context.Background(), Options{RootFS: src, HelperPath: helper})
	if e != nil {
		t.Fatal(e)
	}
	if e := s.WriteFile("/work/test.txt", []byte("ok")); e != nil {
		t.Fatal(e)
	}
	data, e := s.ReadFile("/work/test.txt")
	if e != nil || string(data) != "ok" {
		t.Fatalf("read: %q %v", data, e)
	}
	if names, e := s.ListDir("/work"); e != nil || len(names) != 1 || names[0] != "test.txt" {
		t.Fatalf("names %v err %v", names, e)
	}
	if e := s.RemoveFile("/work/test.txt"); e != nil {
		t.Fatal(e)
	}
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	if e := s.WriteFile("/bad", []byte("x")); e == nil {
		t.Fatal("write to closed session")
	}
}

func TestSessionLibraryAutoHelper(t *testing.T) {
	session, err := NewSession(context.Background(), Options{RootFS: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if !session.runner.autoHelper {
		t.Fatal("session lost self-reexec helper mode")
	}
}
