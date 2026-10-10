package krunlet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/Asutorufa/krunlet/internal/krunffi"
	"github.com/Asutorufa/krunlet/internal/nativebundle"
)

// DoctorReport describes native capabilities and, when RootFS is provided,
// one actual VM smoke boot. Kernel/HVF permission cannot be proven without
// starting a guest. PkgConfigVersion may not be the version of Library.
type DoctorReport struct {
	Native         krunffi.NativeInspection `json:"native"`
	Bundle         nativebundle.Info        `json:"native_bundle"`
	Platform       string                   `json:"platform"`
	KVMAccessible  bool                     `json:"kvm_accessible,omitempty"`
	KVMError       string                   `json:"kvm_error,omitempty"`
	SmokeAttempted bool                     `json:"smoke_attempted"`
	SmokeMillis    int64                    `json:"smoke_millis,omitempty"`
	SmokeExitCode  int                      `json:"smoke_exit_code"`
}

// DoctorDetailed probes ABI feature support and runs a minimal guest /bin/true
// if a trusted rootfs is provided. Without RootFS, it is only a static probe.
func DoctorDetailed(ctx context.Context, lib, rootfs string) (DoctorReport, error) {
	result := DoctorReport{Platform: runtime.GOOS + "/" + runtime.GOARCH}
	info, err := nativebundle.Resolve(lib)
	result.Bundle = info
	if err != nil {
		return result, fmt.Errorf("prepare libkrun: %w", err)
	}
	native, err := krunffi.InspectNativePair(info.Library, info.Firmware)
	result.Native = native
	if err != nil {
		return result, fmt.Errorf("libkrun diagnostic: %w", err)
	}
	if runtime.GOOS == "linux" {
		device, e := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
		if e != nil {
			result.KVMError = e.Error()
		} else {
			result.KVMAccessible = true
			_ = device.Close()
		}
	}
	for _, symbol := range []string{
		"krun_create_ctx", "krun_free_ctx", "krun_set_vm_config", "krun_set_root",
		"krun_set_workdir", "krun_set_exec", "krun_set_rlimits",
		"krun_add_vsock", "krun_disable_implicit_vsock",
		"krun_set_port_map", "krun_start_enter",
	} {
		if !native.Symbols[symbol] {
			return result, fmt.Errorf("required libkrun 1.x symbol %s missing", symbol)
		}
	}
	if rootfs == "" {
		return result, nil
	}
	result.SmokeAttempted = true
	if runtime.GOOS == "linux" && !result.KVMAccessible {
		return result, fmt.Errorf("cannot boot VM: /dev/kvm: %s", result.KVMError)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	t, err := New(Options{RootFS: rootfs, LibraryPath: lib, Timeout: 10 * time.Second})
	if err != nil {
		return result, err
	}
	start := time.Now()
	run, err := t.Run(ctx, Request{Command: []string{"/bin/true"}})
	result.SmokeMillis = time.Since(start).Milliseconds()
	result.SmokeExitCode = run.ExitCode
	if err != nil {
		return result, fmt.Errorf("VM smoke boot: %w", err)
	}
	if run.ExitCode != 0 {
		return result, errors.New("VM smoke /bin/true returned nonzero")
	}
	return result, nil
}
