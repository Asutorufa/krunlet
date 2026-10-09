//go:build !(linux || darwin) || !(amd64 || arm64)

package krunffi

import "errors"

type KernelConfig struct {
	Path    string
	Format  uint32
	Initrd  string
	Cmdline string
}

type Config struct {
	Kernel                              *KernelConfig
	RootFS, WorkDir, Library, ErrorPath string
	Command, Env, Ports, RLimits        []string
	CPUs                                uint8
	MemoryMiB                           uint32
	Network                             bool
	RestrictedNetwork                   bool
}

var ErrUnsupported = errors.New("libkrun is only supported on linux/amd64, linux/arm64 and darwin/arm64")

func Enter(Config) error     { return ErrUnsupported }
func Available(string) error { return ErrUnsupported }
