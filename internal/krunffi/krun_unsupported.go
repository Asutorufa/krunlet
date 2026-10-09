//go:build !(linux || darwin) || !(amd64 || arm64)

package krunffi

import "errors"

type Config struct {
	RootFS, WorkDir, Library     string
	Command, Env, Ports, RLimits []string
	CPUs                         uint8
	MemoryMiB                    uint32
	Network                      bool
}

var ErrUnsupported = errors.New("libkrun is only supported on linux/amd64, linux/arm64 and darwin/arm64")

func Enter(Config) error     { return ErrUnsupported }
func Available(string) error { return ErrUnsupported }
