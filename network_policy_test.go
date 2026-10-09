package krunlet

import (
	"net/netip"
	"strings"
	"testing"
)

func TestNetworkPolicyAllowlist(t *testing.T) {
	p := &NetworkPolicy{
		Mode: NetworkAllowlist, BlockPrivateNetworks: true,
		Allow: []NetworkRule{
			{CIDR: "1.1.1.1", Port: 443, Protocol: "tcp"},
			{CIDR: "2001:4860:4860::/48", Port: 53, Protocol: "udp"},
			{CIDR: "10.0.0.0/8", Port: 443},
		},
	}
	for _, tt := range []struct {
		ip      string
		port    uint16
		proto   string
		allowed bool
	}{
		{"1.1.1.1", 443, "tcp", true},
		{"1.1.1.1", 443, "udp", false},
		{"1.1.1.1", 80, "tcp", false},
		{"1.1.1.2", 443, "tcp", false},
		{"2001:4860:4860::8888", 53, "udp", true},
		{"2001:4860:4860::8888", 53, "tcp", false},
		{"10.0.0.5", 443, "tcp", false},
		{"127.0.0.1", 443, "tcp", false},
		{"169.254.169.254", 443, "tcp", false},
		{"fd00::1", 443, "tcp", false},
		{"fe80::1", 443, "tcp", false},
	} {
		allow, err := p.Allows(netip.MustParseAddr(tt.ip), tt.port, tt.proto)
		if err != nil || allow != tt.allowed {
			t.Errorf("%s:%d/%s allowed=%t err=%v, want %t", tt.ip, tt.port, tt.proto, allow, err, tt.allowed)
		}
	}
}

func TestNetworkPolicyBlacklistAndPrecedence(t *testing.T) {
	p := &NetworkPolicy{
		Mode: NetworkBlocklist, Block: []NetworkRule{{CIDR: "198.51.100.0/24", Port: 443}, {CIDR: "::1/128"}},
	}
	for _, tt := range []struct {
		ip    string
		port  uint16
		allow bool
	}{
		{"198.51.100.4", 443, false},
		{"198.51.100.4", 80, true},
		{"198.51.101.4", 443, true},
		{"::1", 123, false},
	} {
		got, err := p.Allows(netip.MustParseAddr(tt.ip), tt.port, "tcp")
		if err != nil || got != tt.allow {
			t.Fatalf("got %v (%v) for %+v", got, err, tt)
		}
	}
	a := &NetworkPolicy{Mode: NetworkAllowlist, Allow: []NetworkRule{{CIDR: "203.0.113.0/24"}},
		Block: []NetworkRule{{CIDR: "203.0.113.2"}},
	}
	if ok, _ := a.Allows(netip.MustParseAddr("203.0.113.2"), 443, "tcp"); ok {
		t.Fatal("block should override allow")
	}
	if ok, _ := a.Allows(netip.MustParseAddr("203.0.113.3"), 443, "tcp"); !ok {
		t.Fatal("missing allow")
	}
}

func TestNetworkPolicyValidation(t *testing.T) {
	good := &NetworkPolicy{Mode: NetworkAllowlist, Allow: []NetworkRule{{CIDR: "192.0.2.5", Port: 443}}}
	for _, mutate := range []func(*NetworkPolicy){
		func(p *NetworkPolicy) { p.Mode = "anything" },


		func(p *NetworkPolicy) { p.Allow = []NetworkRule{{CIDR: "not-a-domain.example"}} },
		func(p *NetworkPolicy) { p.Allow = []NetworkRule{{CIDR: "::ffff:127.0.0.1/128"}} },
		func(p *NetworkPolicy) { p.Allow = []NetworkRule{{CIDR: "1.2.3.4", Protocol: "quic"}} },
	} {
		p := *good
		mutate(&p)
		if _, err := compileNetworkPolicy(&p); err == nil {
			t.Fatalf("accepted %+v", p)
		}
	}
	if _, err := compileNetworkPolicy(&NetworkPolicy{Mode: NetworkBlocklist, Allow: good.Allow}); err == nil {
		t.Fatal("accepted allow rules in blocklist")
	}
	p, err := normalizeNetworkPolicy(good)
	if err != nil {
		t.Fatal(err)
	}
	good.Allow[0].CIDR = "0.0.0.0/0"
	if p.Allow[0].CIDR != "192.0.2.5" {
		t.Fatal("policy rules share caller storage")
	}
	if ok, _ := p.Allows(netip.MustParseAddr("192.0.2.5"), 443, "tcp"); !ok {
		t.Fatal("lost normalized policy")
	}
	if _, err := p.Allows(netip.MustParseAddr("192.0.2.5"), 0, "tcp"); err == nil {
		t.Fatal("accepted port zero destination")
	}
}

func TestNetworkPolicyRequiresExplicitNetworking(t *testing.T) {
	root := t.TempDir()
	_, err := New(Options{RootFS: root, NetworkPolicy: &NetworkPolicy{Mode: NetworkAllowlist, }})
	if err == nil || !strings.Contains(err.Error(), "Network=true") {
		t.Fatalf("got %v", err)
	}
}
