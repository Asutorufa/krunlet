//go:build (linux || darwin) && (amd64 || arm64)

package krunffi

import (
	"testing"
	"unsafe"
)

func TestCStringArray(t *testing.T) {
	err := withCStringArray([]string{"a", "bc"}, func(p unsafe.Pointer) error {
		arr := (*[3]*byte)(p)
		if arr[0] == nil || arr[1] == nil || arr[2] != nil {
			t.Fatal("bad pointers")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if withCStringArray([]string{"a\x00b"}, func(unsafe.Pointer) error { return nil }) == nil {
		t.Fatal("NUL was accepted")
	}
}
