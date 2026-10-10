//go:build !linux

package krunlet

import (
 "errors"
)

type vmCgroup struct {}
func prepareVMCgroup(opts Options) (*vmCgroup,error) {
 if opts.CgroupParent!=""{return nil,errors.New("cgroup v2 is only supported on Linux")}
 return nil,nil
}
func startHelperInCgroup(s *helperSupervisor,_ *vmCgroup) error {return s.start()}
func waitCgroupStartupGate() error {return nil}
func killOwnCgroup() {}
func (cg *vmCgroup) Close() error {return nil}
