//go:build !linux && !darwin

package krunlet

import (
	"context"
	"errors"
)

type networkLease struct{ socket string }

func (l *networkLease) Close() {}
func prepareNetwork(_ context.Context, policy *NetworkPolicy, _ ...*YuhaiinConfig) (*networkLease, error) {
	if policy != nil {
		return nil, errors.New("gVisor networking only supports Linux and macOS")
	}
	return &networkLease{}, nil
}
