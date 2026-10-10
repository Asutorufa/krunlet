//go:build !linux

package krunlet

import (
	"errors"
	"os/exec"
)

// CgroupV2 is unavailable on non-Linux hosts.
type CgroupV2 struct {
	Parent string
	MemoryMaxBytes int64
	PidsMax int64
}
type cgroupScope struct{}
func validateCgroupConfig(c *CgroupV2) error {
	if c!=nil{return errors.New("cgroup v2 requires Linux")}
	return nil
}
func prepareVMCgroup(c *CgroupV2,_ uint32)(*cgroupScope,error){
	if c!=nil{return nil,errors.New("cgroup v2 requires Linux")}
	return nil,nil
}
func (*cgroupScope) attach(_ *exec.Cmd) error {return nil}
func (*cgroupScope) close() error {return nil}
func (*cgroupScope) peakMiB() float64 {return 0}
