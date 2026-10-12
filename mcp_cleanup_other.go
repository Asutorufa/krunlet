//go:build !linux

package krunlet

import "errors"

func reclaimMCPVMGroups(parent string) error {
	if parent != "" {
		return errors.New("cgroup v2 is only supported on Linux")
	}
	return nil
}
