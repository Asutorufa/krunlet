//go:build vm_integration && linux

package krunlet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A real cgroup v2 kernel test: a host helper creates a child in a NEW
// session, so a PGID kill cannot reach it. Only cgroup.kill can clean it.
func TestNativeCgroupKillEscapedSetsidChild(t *testing.T) {
	parent:=os.Getenv("KRUNLET_CGROUP_PARENT")
	if parent=="" {t.Fatal("KRUNLET_CGROUP_PARENT is required for native VM checks")}
	root:=t.TempDir()
	script:=filepath.Join(t.TempDir(),"helper.sh")
	pidPath:=filepath.Join(t.TempDir(),"child.pid")
	body:="#!/bin/sh\nsetsid sleep 30 &\necho $! > \"$KRUNLET_ESCAPE_PIDFILE\"\nwait\n"
	if err:=os.WriteFile(script,[]byte(body),0700);err!=nil{t.Fatal(err)}
	t.Setenv("KRUNLET_ESCAPE_PIDFILE",pidPath)
	r,err:=New(Options{RootFS:root,HelperPath:script,Timeout:2*time.Second,
		CgroupV2:&CgroupV2{Parent:parent,MemoryMaxBytes:256<<20,PidsMax:32}})
	if err!=nil {t.Fatal(err)}
	_,err=r.Run(context.Background(),Request{Command:[]string{"/bin/true"}})
	if !errors.Is(err,context.DeadlineExceeded) {t.Fatalf("expected timeout: %v",err)}
	data,err:=os.ReadFile(pidPath)
	if err!=nil {t.Fatal(err)}
	pid,err:=strconv.Atoi(strings.TrimSpace(string(data)))
	if err!=nil {t.Fatal(err)}
	deadline:=time.Now().Add(3*time.Second)
	for time.Now().Before(deadline) {
		if err:=syscall.Kill(pid,0);errors.Is(err,syscall.ESRCH) {return}
		if ps,err:=os.ReadFile(fmt.Sprintf("/proc/%d/stat",pid));err==nil {
			if k:=strings.LastIndex(string(ps),") ");k>=0&&len(ps)>k+2&&ps[k+2]=='Z'{return}
		}
		time.Sleep(30*time.Millisecond)
	}
	t.Fatalf("setsid child pid %d survived cgroup.kill",pid)
}
