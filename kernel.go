package krunlet

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Asutorufa/krunlet/internal/krunffi"
)

// KernelFormat selects the kernel image encoding accepted by libkrun 1.19.x.
// The numeric values mirror KRUN_KERNEL_FORMAT_* in include/libkrun.h.
type KernelFormat uint32

const (
	KernelFormatRaw KernelFormat = iota
	KernelFormatELF
	KernelFormatPEGZ
	KernelFormatImageBZ2
	KernelFormatImageGZ
	KernelFormatImageZSTD
)

func (f KernelFormat) String() string {
	switch f {
	case KernelFormatRaw:
		return "raw"
	case KernelFormatELF:
		return "elf"
	case KernelFormatPEGZ:
		return "pe-gz"
	case KernelFormatImageBZ2:
		return "image-bz2"
	case KernelFormatImageGZ:
		return "image-gz"
	case KernelFormatImageZSTD:
		return "image-zstd"
	default:
		return fmt.Sprintf("unknown(%d)", f)
	}
}

// ParseKernelFormat converts a CLI-friendly name into a libkrun kernel format.
func ParseKernelFormat(name string) (KernelFormat, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "raw":
		return KernelFormatRaw, nil
	case "elf":
		return KernelFormatELF, nil
	case "pe-gz":
		return KernelFormatPEGZ, nil
	case "image-bz2":
		return KernelFormatImageBZ2, nil
	case "image-gz":
		return KernelFormatImageGZ, nil
	case "image-zstd":
		return KernelFormatImageZSTD, nil
	default:
		return 0, fmt.Errorf("unsupported kernel format %q (valid: raw, elf, pe-gz, image-bz2, image-gz, image-zstd)", name)
	}
}

// KernelConfig selects an external Linux kernel rather than libkrunfw's
// embedded kernel payload. Path and Initrd are HOST file paths, not paths
// inside RootFS. An empty Initrd or Cmdline passes a NULL argument to
// krun_set_kernel, letting libkrun apply its own default behavior.
//
// This configuration is trusted host infrastructure input. Never allow an
// untrusted LLM agent to choose arbitrary kernel or initramfs paths.
type KernelConfig struct {
	Path    string
	Format  KernelFormat
	Initrd  string
	Cmdline string
}

func normalizeKernel(k *KernelConfig) (*KernelConfig, error) {
	if k == nil {
		return nil, nil
	}
	copy := *k // avoid mutation of the caller's struct after New returns
	if copy.Format > KernelFormatImageZSTD {
		return nil, fmt.Errorf("invalid format: %d", copy.Format)
	}
	var err error
	if copy.Path, err = kernelFile("path", copy.Path, true); err != nil {
		return nil, err
	}
	if copy.Initrd, err = kernelFile("initramfs", copy.Initrd, false); err != nil {
		return nil, err
	}
	if strings.IndexByte(copy.Cmdline, 0) >= 0 {
		return nil, fmt.Errorf("kernel command line contains NUL")
	}
	// libkrun 1.19.x maps x86_64 raw images with map_kernel(), which
	// returns before processing optional initramfs and cmdline arguments.
	if runtime.GOARCH == "amd64" && copy.Format == KernelFormatRaw &&
		(copy.Initrd != "" || copy.Cmdline != "") {
		return nil, fmt.Errorf("libkrun 1.19.x ignores initramfs and cmdline for x86-64 raw kernels; choose another format")
	}
	return &copy, nil
}

func kernelFile(field, name string, required bool) (string, error) {
	if name == "" {
		if required {
			return "", fmt.Errorf("%s is required for a custom kernel", field)
		}
		return "", nil
	}
	if strings.IndexByte(name, 0) >= 0 {
		return "", fmt.Errorf("%s contains NUL", field)
	}
	full, err := filepath.Abs(name)
	if err != nil {
		return "", fmt.Errorf("%s: %w", field, err)
	}
	full, err = filepath.EvalSymlinks(full)
	if err != nil {
		return "", fmt.Errorf("%s: %w", field, err)
	}
	info, err := os.Stat(full)
	if err != nil {
		return "", fmt.Errorf("%s: %w", field, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s must be a regular file", field)
	}
	file, err := os.Open(full)
	if err != nil {
		return "", fmt.Errorf("%s: %w", field, err)
	}
	_ = file.Close()
	return full, nil
}

func ffiKernel(k *KernelConfig) *krunffi.KernelConfig {
	if k == nil {
		return nil
	}
	return &krunffi.KernelConfig{
		Path: k.Path, Format: uint32(k.Format), Initrd: k.Initrd, Cmdline: k.Cmdline,
	}
}
