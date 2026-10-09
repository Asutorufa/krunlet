//go:build !linux

package krunlet

import (
	"context"
	"errors"
	"os/exec"
)

type networkLease func()

func prepareNetwork(_ context.Context, p *NetworkPolicy, _ *exec.Cmd) (networkLease, error) {
	if p != nil {
		return nil, errors.New("restricted network policies require Linux network namespaces and nftables; unsupported on this host")
	}
	return func() {}, nil
}
