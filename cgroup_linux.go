//go:build linux

package krunlet

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CgroupV2 applies host-side kernel memory/pid limits and cgroup.kill to a
// helper process tree, including grandchildren that call setsid/setpgid.
// Parent must be an administrator-delegated, writable cgroup v2 directory.
// The controller rejects missing delegation rather than degrading to PGIDs.
type CgroupV2 struct {
	Parent string
	MemoryMaxBytes int64 // 0: guest memory plus 256 MiB VMM overhead
	PidsMax int64 // 0: 128 processes
}

type cgroupScope struct {
	path string
	fd *os.File
}

func validateCgroupConfig(c *CgroupV2) error {
	if c == nil {return nil}
	if !filepath.IsAbs(c.Parent) {return errors.New("cgroup v2 parent must be absolute")}
	clean,err:=filepath.EvalSymlinks(c.Parent)
	if err!=nil {return fmt.Errorf("cgroup v2 parent: %w",err)}
	if clean!=filepath.Clean(c.Parent) {return errors.New("cgroup v2 parent must not contain symlinks")}
	if clean=="/sys/fs/cgroup" {return errors.New("cgroup v2 parent must be a delegated child, not the host root")}
	if !strings.HasPrefix(clean,"/sys/fs/cgroup/") {return errors.New("cgroup v2 parent must be beneath /sys/fs/cgroup")}
	if c.MemoryMaxBytes<0 || c.PidsMax<0 {return errors.New("cgroup limits cannot be negative")}
	if _,err:=os.Stat(filepath.Join(clean,"cgroup.controllers"));err!=nil {return fmt.Errorf("cgroup v2 controller delegation unavailable: %w",err)}
	return nil
}

func prepareVMCgroup(c *CgroupV2, guestMiB uint32) (*cgroupScope,error) {
	if c==nil{return nil,nil}
	if err:=validateCgroupConfig(c);err!=nil{return nil,err}
	dir,err:=os.MkdirTemp(c.Parent,"krunlet-vm-")
	if err!=nil{return nil,fmt.Errorf("create VM cgroup (delegation required): %w",err)}
	remove:=func(err error)(*cgroupScope,error){_ = os.Remove(dir);return nil,err}
	// Never turn regular directories into pseudo cgroup files: check the
	// controllers exist in this delegated leaf before writing limits.
	for _,name:=range []string{"cgroup.procs","cgroup.kill","memory.max","pids.max","memory.peak"} {
		if _,err:=os.Stat(filepath.Join(dir,name));err!=nil{return remove(fmt.Errorf("missing cgroup v2 %s: %w",name,err))}
	}
	mem:=c.MemoryMaxBytes
	if mem==0 {mem=(int64(guestMiB)+256)<<20}
	pids:=c.PidsMax
	if pids==0 {pids=128}
	if err:=os.WriteFile(filepath.Join(dir,"memory.max"),[]byte(strconv.FormatInt(mem,10)),0600);err!=nil{return remove(fmt.Errorf("set memory.max: %w",err))}
	if err:=os.WriteFile(filepath.Join(dir,"pids.max"),[]byte(strconv.FormatInt(pids,10)),0600);err!=nil{return remove(fmt.Errorf("set pids.max: %w",err))}
	fd,err:=os.Open(dir)
	if err!=nil{return remove(err)}
	return &cgroupScope{path:dir,fd:fd},nil
}

func (g *cgroupScope) attach(cmd *exec.Cmd) error {
	if g==nil{return nil}
	if cmd.SysProcAttr==nil{return errors.New("missing Linux helper SysProcAttr")}
	cmd.SysProcAttr.UseCgroupFD=true
	cmd.SysProcAttr.CgroupFD=int(g.fd.Fd())
	cmd.Env=append(cmd.Env,"KRUNLET_CGROUP_PATH="+g.path)
	return nil
}

func (g *cgroupScope) peakMiB() float64 {
	if g==nil{return 0}
	b,err:=os.ReadFile(filepath.Join(g.path,"memory.peak"))
	if err!=nil{return 0}
	n,err:=strconv.ParseInt(strings.TrimSpace(string(b)),10,64)
	if err!=nil{return 0}
	return float64(n)/(1024*1024)
}

// Close is idempotent from a single owner after helper termination.
// cgroup.kill kills escaped setsid children as well as the original PGID.
// If the cgroup remains populated, return an error rather than pretend it
// was cleaned.
func (g *cgroupScope) close() error {
	if g==nil{return nil}
	_ = g.fd.Close()
	if err:=os.WriteFile(filepath.Join(g.path,"cgroup.kill"),[]byte("1"),0600);err!=nil {
		return fmt.Errorf("cgroup.kill %s: %w",g.path,err)
	}
	var last error
	for i:=0;i<40;i++ {
		err:=os.Remove(g.path)
		if err==nil || errors.Is(err,os.ErrNotExist){return nil}
		last=err
		time.Sleep(50*time.Millisecond)
	}
	return fmt.Errorf("VM cgroup remains after cgroup.kill: %s: %w",g.path,last)
}

func killOrphanCgroup() {
	path:=os.Getenv("KRUNLET_CGROUP_PATH")
	if path=="" || !strings.HasPrefix(filepath.Clean(path),"/sys/fs/cgroup/"){return}
	_ = os.WriteFile(filepath.Join(path,"cgroup.kill"),[]byte("1"),0600)
}
