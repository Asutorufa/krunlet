package krunlet

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Fake helper verifies the same kernel settings are forwarded when the
// library boots a single long-running VM instead of a one-shot instance.
func TestCustomKernelPassedToPersistentVM(t *testing.T) {
	kernel := filepath.Join(t.TempDir(), "vmlinux")
	if err := os.WriteFile(kernel, []byte("test-image"), 0600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "vm-config.json")
	original := fakeLiveHelper(t, false)
	body, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(t.TempDir(), "helper")
	capture := []byte("#!/bin/sh\ncp \"$3\" \"" + cfgPath + "\"\n")
	body = bytes.Replace(body, []byte("#!/bin/sh\n"), capture, 1)
	if err := os.WriteFile(helper, body, 0700); err != nil {
		t.Fatal(err)
	}
	vm, err := NewVM(context.Background(), Options{
		RootFS: t.TempDir(), HelperPath: helper, Timeout: time.Second,
		Kernel: &KernelConfig{Path: kernel, Format: KernelFormatELF, Cmdline: "console=hvc0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close()
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Kernel *struct {
			Path    string
			Format  uint32
			Cmdline string
		}
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Kernel == nil || payload.Kernel.Path != kernel || payload.Kernel.Format != 1 || payload.Kernel.Cmdline != "console=hvc0" {
		t.Fatalf("missing VM kernel config: %s", data)
	}
}
