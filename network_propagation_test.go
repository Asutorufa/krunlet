//go:build linux || darwin

package krunlet

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func checkRestrictedHelperConfig(t *testing.T, file string) {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Network           bool
		RestrictedNetwork bool
		NetSocket         string
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Network || !got.RestrictedNetwork || got.NetSocket == "" {
		t.Fatalf("restricted gVisor backend not passed to helper: %+v", got)
	}
	if _, err := os.Stat(got.NetSocket); !os.IsNotExist(err) {
		t.Fatalf("gateway socket was not removed after helper exit: %v", err)
	}
}

func TestRestrictedNetworkOneShotAndSession(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "helper-config.json")
	helper := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\ncp \"$3\" \""+capture+"\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	opts := Options{RootFS: t.TempDir(), HelperPath: helper, Network: true,
		NetworkPolicy: &NetworkPolicy{Mode: NetworkAllowlist},
	}
	r, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), Request{Command: []string{"/bin/true"}}); err != nil {
		t.Fatal(err)
	}
	checkRestrictedHelperConfig(t, capture)
	s, err := NewSession(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Run(context.Background(), Request{Command: []string{"/bin/true"}}); err != nil {
		t.Fatal(err)
	}
	checkRestrictedHelperConfig(t, capture)
}

func TestRestrictedNetworkPersistentVM(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "vm-config.json")
	original := fakeLiveHelper(t, false)
	body, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(t.TempDir(), "helper")
	body = append([]byte("#!/bin/sh\ncp \"$3\" \""+capture+"\"\n"), body[len("#!/bin/sh\n"):]...)
	if err := os.WriteFile(helper, body, 0700); err != nil {
		t.Fatal(err)
	}
	vm, err := NewVM(context.Background(), Options{RootFS: t.TempDir(), HelperPath: helper, Network: true, Timeout: 4 * time.Second,
		NetworkPolicy: &NetworkPolicy{Mode: NetworkAllowlist},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.Close(); err != nil {
		t.Fatal(err)
	}
	checkRestrictedHelperConfig(t, capture)
}
