//go:build linux

package krunlet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCgroupV2RejectsNonDelegatedParents(t *testing.T) {
	for _,p:=range []string{"", ".", "/", "/sys/fs/cgroup"}{
		if err:=validateCgroupConfig(&CgroupV2{Parent:p});err==nil {
			t.Fatalf("accepted unsafe cgroup parent %q",p)
		}
	}
	bad:=&CgroupV2{Parent:"/sys/fs/cgroup/nonexistent-delegation",PidsMax:-1}
	if err:=validateCgroupConfig(bad);err==nil {t.Fatal("negative cgroup limit accepted")}
	if _,err:=prepareVMCgroup(&CgroupV2{Parent:"/sys/fs/cgroup/nonexistent-krunlet-scope"},256);err==nil {
		t.Fatal("unavailable cgroup did not fail closed")
	}
}

func TestCgroupV2DoesNotSilentlyCreateOrdinaryFiles(t *testing.T) {
	parent:=t.TempDir()
	_,err:=prepareVMCgroup(&CgroupV2{Parent:parent},256)
	if err==nil {t.Fatal("non-cgroup directory accepted")}
	if !strings.Contains(err.Error(),"cgroup") {t.Fatalf("unhelpful cgroup error: %v",err)}
	entries,e:=os.ReadDir(parent)
	if e!=nil {t.Fatal(e)}
	for _,x:=range entries {
		if strings.HasPrefix(x.Name(),"krunlet-vm-") {t.Errorf("cgroup directory left after failed preparation: %s",filepath.Join(parent,x.Name()))}
	}
}
