package main

import "testing"

func TestParseNetworkRule(t *testing.T) {
	for _, tt := range []struct{ spec, cidr string; port uint16; proto string }{
		{"1.1.1.1", "1.1.1.1", 0, ""},
		{"2001:db8::/32,443,tcp", "2001:db8::/32", 443, "tcp"},
		{"8.8.8.8,53,udp", "8.8.8.8", 53, "udp"},
	} {
		r,err:=parseNetworkRule(tt.spec)
		if err!=nil||r.CIDR!=tt.cidr||r.Port!=tt.port||r.Protocol!=tt.proto {
			t.Fatalf("%q parsed %+v: %v",tt.spec,r,err)
		}
	}
	for _,spec:=range []string{"",",443,tcp","1.1.1.1,0","1.1.1.1,65536","1.1.1.1,abc","1.1.1.1,443,tcp,extra"} {
		if _,err:=parseNetworkRule(spec);err==nil {t.Fatalf("accepted %q",spec)}
	}
}
