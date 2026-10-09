//go:build linux || darwin

package krunlet

import (
 "context"
 "net"
 "os"
 "strings"
 "testing"
)

func TestGVisorGatewayLifecycle(t *testing.T) {
 policy:=&NetworkPolicy{Mode:NetworkAllowlist,Allow:[]NetworkRule{{CIDR:"1.1.1.1/32",Port:443,Protocol:"tcp"}}}
 lease,err:=prepareNetwork(context.Background(),policy)
 if err!=nil {t.Fatal(err)}
 path:=lease.socket
 if path=="" {t.Fatal("gateway did not create socket")}
 info,err:=os.Stat(path)
 if err!=nil {t.Fatal(err)}
 if info.Mode().Perm() & 077 != 0 {t.Fatalf("gateway socket permissions too permissive: %o",info.Mode().Perm())}
 conn,err:=net.Dial("unix",path)
 if err!=nil {t.Fatal(err)}
 _=conn.Close()
 lease.Close()
 if _,err:=os.Stat(path);!os.IsNotExist(err){t.Fatalf("socket not cleaned up: %v",err)}
 lease.Close()
}

func TestGVisorIPv6RulesFailClosed(t *testing.T) {
 _,err:=prepareNetwork(context.Background(),&NetworkPolicy{Mode:NetworkAllowlist,Allow:[]NetworkRule{{CIDR:"2001:db8::/32"}}})
 if err==nil||!strings.Contains(err.Error(),"IPv4-only"){t.Fatalf("expected IPv6 rejection, got %v",err)}
}
