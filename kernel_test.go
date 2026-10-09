package krunlet

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestKernelFormats(t *testing.T) {
	for i, name := range []string{"raw", "elf", "pe-gz", "image-bz2", "image-gz", "image-zstd"} {
		f, err := ParseKernelFormat(name)
		if err != nil || f != KernelFormat(i) || f.String() != name {
			t.Fatalf("format %q = %d, %v", name, f, err)
		}
	}
	if _, err := ParseKernelFormat("unknown"); err == nil {
		t.Fatal("accepted unknown kernel format")
	}
}

func TestKernelValidation(t *testing.T) {
	root := t.TempDir()
	kernel := filepath.Join(t.TempDir(), "Image")
	initramfs := filepath.Join(t.TempDir(), "initrd.img")
	for _, p := range []string{kernel, initramfs} {
		if err := os.WriteFile(p, []byte("test-image"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	orig := &KernelConfig{Path: kernel, Initrd: initramfs, Format: KernelFormatELF, Cmdline: "console=hvc0"}
	runner, err := New(Options{RootFS: root, Kernel: orig})
	if err != nil {
		t.Fatal(err)
	}
	orig.Cmdline = "changed"
	if runner.cfg.Kernel.Cmdline != "console=hvc0" || runner.cfg.Kernel == orig {
		t.Fatal("kernel options were not defensively copied")
	}
	for _, cfg := range []*KernelConfig{
		{Path: ""},
		{Path: root},
		{Path: filepath.Join(root, "not-found")},
		{Path: kernel, Initrd: root},
		{Path: kernel, Initrd: filepath.Join(root, "not-found")},
		{Path: kernel, Cmdline: "bad\x00literal"},
		{Path: kernel, Format: KernelFormat(99)},
	} {
		if _, err := New(Options{RootFS: root, Kernel: cfg}); err == nil {
			t.Fatalf("accepted invalid kernel config %+v", cfg)
		}
	}
	if runtime.GOARCH == "amd64" {
		if _, err := New(Options{RootFS: root, Kernel: &KernelConfig{Path: kernel, Format: KernelFormatRaw, Cmdline: "console=hvc0"}}); err == nil {
			t.Fatal("accepted silently ignored x86_64 raw kernel command line")
		}
	}
}

func TestCustomKernelPassedToOneShotAndSession(t *testing.T) {
	kernel := filepath.Join(t.TempDir(), "Image")
	if err := os.WriteFile(kernel, []byte("test-image"), 0600); err != nil {
		t.Fatal(err)
	}
	realKernel, err := filepath.EvalSymlinks(kernel)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "helper.json")
	helper := filepath.Join(t.TempDir(), "helper")
	code := "#!/bin/sh\ncp \"$3\" \"" + cfgPath + "\"\n"
	if err := os.WriteFile(helper, []byte(code), 0700); err != nil {
		t.Fatal(err)
	}
	opts := Options{RootFS: root, HelperPath: helper, Timeout: time.Second, Kernel: &KernelConfig{Path: kernel, Format: KernelFormatELF, Cmdline: "console=hvc0"}}
	check := func() {
		t.Helper()
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Kernel *struct {
				Path    string
				Format  uint32
				Initrd  string
				Cmdline string
			}
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Kernel == nil || payload.Kernel.Path != realKernel || payload.Kernel.Format != 1 || payload.Kernel.Cmdline != "console=hvc0" {
			t.Fatalf("kernel missing in helper payload: %s", data)
		}
	}
	r, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), Request{Command: []string{"/bin/true"}}); err != nil {
		t.Fatal(err)
	}
	check()
	s, err := NewSession(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Run(context.Background(), Request{Command: []string{"/bin/true"}}); err != nil {
		t.Fatal(err)
	}
	check()
}
