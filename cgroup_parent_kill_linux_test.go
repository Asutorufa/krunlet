//go:build vm_integration && linux

package krunlet

import (
 "context"
 "os"
 "os/exec"
 "path/filepath"
 "strings"
 "testing"
 "time"
)

// This test uses a REAL libkrun VM, not the fake helper. It SIGKILLs the
// parent Go process and verifies kernel cgroup membership becomes empty.
func TestRealVMParentSIGKILLDrainsCgroup(t *testing.T) {
 if os.Getenv("KRUNLET_REAL_PARENT_CHILD")=="1" {
   r,err:=New(Options{
     RootFS:os.Getenv("KRUNLET_TEST_ROOTFS"),
     CgroupParent:os.Getenv("KRUNLET_TEST_CGROUP_PARENT"),
     Timeout:45*time.Second,
   })
   if err!=nil{t.Fatal(err)}
   _,_=r.Run(context.Background(),Request{Command:[]string{"/bin/sh","-c","sleep 30"}})
   return
 }
 parent:=integrationCgroup(t)
 _=integrationRootFS(t)
 baseline:=map[string]bool{}
 before,err:=os.ReadDir(parent)
 if err!=nil{t.Fatal(err)}
 for _,e:=range before {baseline[e.Name()]=true}
 cmd:=exec.Command(os.Args[0],"-test.run=^TestRealVMParentSIGKILLDrainsCgroup$")
 cmd.Env=append(os.Environ(),"KRUNLET_REAL_PARENT_CHILD=1")
 if err:=cmd.Start();err!=nil{t.Fatal(err)}
 defer func(){_ = cmd.Process.Kill(); _ = cmd.Wait()}()
 var group string
 until:=time.Now().Add(20*time.Second)
 for time.Now().Before(until) {
   entries,err:=os.ReadDir(parent)
   if err!=nil{t.Fatal(err)}
   for _,e:=range entries {
     if baseline[e.Name()] || !strings.HasPrefix(e.Name(),"krunlet-vm-"){continue}
     path:=filepath.Join(parent,e.Name())
     procs,err:=os.ReadFile(filepath.Join(path,"cgroup.procs"))
     if err==nil && len(strings.TrimSpace(string(procs)))>0 {group=path;break}
   }
   if group!=""{break}
   time.Sleep(25*time.Millisecond)
 }
 if group==""{t.Fatal("real VM parent never populated a per-VM cgroup")}
 if err:=cmd.Process.Kill();err!=nil{t.Fatal(err)}
 _=cmd.Wait()
 until=time.Now().Add(10*time.Second)
 for time.Now().Before(until) {
   events,err:=os.ReadFile(filepath.Join(group,"cgroup.events"))
   if err==nil && strings.Contains("\n"+string(events),"\npopulated 0\n"){
     if err:=os.Remove(group);err!=nil{t.Fatalf("dead VM cgroup not removable: %v",err)}
     return
   }
   time.Sleep(30*time.Millisecond)
 }
 t.Fatalf("live host tasks remain after parent SIGKILL in cgroup %s",group)
}
