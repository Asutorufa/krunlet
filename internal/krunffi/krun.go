//go:build (linux || darwin) && (amd64 || arm64)

// Package krunffi implements the legacy libkrun 1.x C ABI using purego.
// Never call Enter from an application process: libkrun calls exit(3).
package krunffi

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"unsafe"

	"github.com/ebitengine/purego"
)

type KernelConfig struct {
	Path    string
	Format  uint32
	Initrd  string
	Cmdline string
}

type Config struct {
	Kernel    *KernelConfig
	RootFS    string
	WorkDir   string
	Command   []string
	Env       []string
	CPUs      uint8
	MemoryMiB uint32
	Network   bool
	Ports     []string
	RLimits   []string
	Library   string
	ErrorPath string
}

type api struct {
	create               func() int32
	free                 func(uint32) int32
	vmConfig             func(uint32, uint8, uint32) int32
	root                 func(uint32, string) int32
	workdir              func(uint32, string) int32
	exec                 func(uint32, string, unsafe.Pointer, unsafe.Pointer) int32
	limits               func(uint32, unsafe.Pointer) int32
	vsock                func(uint32, uint32) int32
	disableImplicitVsock func(uint32) int32
	kernel               func(uint32, string, uint32, unsafe.Pointer, unsafe.Pointer) int32
	ports                func(uint32, unsafe.Pointer) int32
	enter                func(uint32) int32
}

// Enter configures and starts a VM; success never returns. Only invoke in a
// dedicated subprocess. The native lib is loaded at runtime, not link time.
func Enter(c Config) error {
	libName := c.Library
	if libName == "" {
		switch runtime.GOOS {
		case "darwin":
			libName = "libkrun.dylib"
		case "linux":
			libName = "libkrun.so.1"
		}
	}
	lib, err := purego.Dlopen(libName, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return fmt.Errorf("load %s: %w (requires libkrun 1.19.x)", libName, err)
	}
	defer purego.Dlclose(lib)
	a := api{}
	for _, sym := range []struct {
		name string
		fn   any
	}{
		{"krun_create_ctx", &a.create}, {"krun_free_ctx", &a.free},
		{"krun_set_vm_config", &a.vmConfig}, {"krun_set_root", &a.root},
		{"krun_set_workdir", &a.workdir}, {"krun_set_exec", &a.exec},
		{"krun_set_rlimits", &a.limits}, {"krun_disable_implicit_vsock", &a.disableImplicitVsock}, {"krun_add_vsock", &a.vsock},
		{"krun_set_port_map", &a.ports}, {"krun_start_enter", &a.enter},
	} {
		symbol, e := purego.Dlsym(lib, sym.name)
		if e != nil {
			return fmt.Errorf("symbol %s missing (libkrun 2.x is incompatible): %w", sym.name, e)
		}
		purego.RegisterFunc(sym.fn, symbol)
	}
	// The custom-kernel symbol is only required when a custom kernel was
	// requested, preserving compatibility with default-kernel installations.
	if c.Kernel != nil {
		symbol, e := purego.Dlsym(lib, "krun_set_kernel")
		if e != nil {
			return fmt.Errorf("custom kernel requires krun_set_kernel: %w", e)
		}
		purego.RegisterFunc(&a.kernel, symbol)
	}
	id := a.create()
	if id < 0 {
		return nativeError("krun_create_ctx", id)
	}
	ctx := uint32(id)
	// If krun_start_enter succeeds it terminates the entire helper, so this
	// defer only runs when configuration or VM startup fails.
	defer a.free(ctx)
	if err := check("krun_set_vm_config", a.vmConfig(ctx, c.CPUs, c.MemoryMiB)); err != nil {
		return err
	}
	if c.Kernel != nil {
		if err := withOptionalCString(c.Kernel.Initrd, func(initrd unsafe.Pointer) error {
			return withOptionalCString(c.Kernel.Cmdline, func(cmdline unsafe.Pointer) error {
				return check("krun_set_kernel", a.kernel(ctx, c.Kernel.Path, c.Kernel.Format, initrd, cmdline))
			})
		}); err != nil {
			return err
		}
	}
	if err := check("krun_set_root", a.root(ctx, c.RootFS)); err != nil {
		return err
	}
	if err := check("krun_set_workdir", a.workdir(ctx, c.WorkDir)); err != nil {
		return err
	}
	if len(c.RLimits) > 0 {
		if err := withCStringArray(c.RLimits, func(p unsafe.Pointer) error { return check("krun_set_rlimits", a.limits(ctx, p)) }); err != nil {
			return err
		}
	}
	// Explicit vsock with no TSI flags disables TSI socket hijacking. This is
	// essential: with no explicit override, legacy libkrun enables host egress.
	if !c.Network {
		// libkrun 1.19 requires disabling its implicit vsock before adding
		// a custom device with no TSI features.
		if err := check("krun_disable_implicit_vsock", a.disableImplicitVsock(ctx)); err != nil {
			return err
		}
		if err := check("krun_add_vsock(no TSI)", a.vsock(ctx, 0)); err != nil {
			return err
		}
	}
	// An explicitly empty port map prevents implicit wildcard forwarding.
	if err := withCStringArray(c.Ports, func(p unsafe.Pointer) error { return check("krun_set_port_map", a.ports(ctx, p)) }); err != nil {
		return err
	}
	if len(c.Command) == 0 {
		return fmt.Errorf("empty guest command")
	}
	args := c.Command[1:]
	return withCStringArray(args, func(argv unsafe.Pointer) error {
		return withCStringArray(c.Env, func(env unsafe.Pointer) error {
			if err := check("krun_set_exec", a.exec(ctx, c.Command[0], argv, env)); err != nil {
				return err
			}
			rc := a.enter(ctx)
			return fmt.Errorf("krun_start_enter returned unexpectedly: %w", nativeError("krun_start_enter", rc))
		})
	})
}

func check(name string, rc int32) error {
	if rc < 0 {
		return nativeError(name, rc)
	}
	return nil
}
func nativeError(name string, rc int32) error {
	return fmt.Errorf("%s failed: %d (errno %d)", name, rc, -rc)
}

// Arrays are NUL-terminated C string vectors. Both the vectors and their byte
// buffers are kept alive until the C function returns.
func withCStringArray(v []string, fn func(unsafe.Pointer) error) error {
	bytes := make([][]byte, len(v))
	ptrs := make([]*byte, len(v)+1)
	for i, s := range v {
		if strings.IndexByte(s, 0) >= 0 {
			return fmt.Errorf("NUL byte in C string at index %d", i)
		}
		bytes[i] = append([]byte(s), 0)
		ptrs[i] = &bytes[i][0]
	}
	err := fn(unsafe.Pointer(&ptrs[0]))
	runtime.KeepAlive(bytes)
	runtime.KeepAlive(ptrs)
	return err
}

// withOptionalCString passes nil for unset optional C strings. Unlike a
// Go string argument, a nil pointer is meaningful to krun_set_kernel.
func withOptionalCString(s string, fn func(unsafe.Pointer) error) error {
	if s == "" {
		return fn(nil)
	}
	if strings.IndexByte(s, 0) >= 0 {
		return fmt.Errorf("NUL byte in optional C string")
	}
	buf := append([]byte(s), 0)
	err := fn(unsafe.Pointer(&buf[0]))
	runtime.KeepAlive(buf)
	return err
}

func Available(lib string) error {
	if lib == "" {
		if runtime.GOOS == "linux" {
			lib = "libkrun.so.1"
		} else {
			lib = "libkrun.dylib"
		}
	}
	h, e := purego.Dlopen(lib, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if e != nil {
		return e
	}
	defer purego.Dlclose(h)
	for _, symbol := range []string{"krun_create_ctx", "krun_start_enter", "krun_add_vsock", "krun_disable_implicit_vsock"} {
		if _, e = purego.Dlsym(h, symbol); e != nil {
			return e
		}
	}
	if runtime.GOOS == "linux" {
		if _, e := os.Stat("/dev/kvm"); e != nil {
			return fmt.Errorf("/dev/kvm: %w", e)
		}
	}
	return nil
}
