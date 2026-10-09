//go:build linux

package krunlet

import (
	"strings"
	"testing"
)

func TestNFTAllowlistScript(t *testing.T) {
	c, err := compileNetworkPolicy(&NetworkPolicy{
		Mode: NetworkAllowlist, Namespace: "private-netns", BlockPrivateNetworks: true,
		Allow: []NetworkRule{{CIDR: "1.1.1.1", Port: 443, Protocol: "tcp"}, {CIDR: "2001:db8::/32", Port: 53, Protocol: "udp"}},
		Block: []NetworkRule{{CIDR: "1.1.1.2"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := nftPolicyScript(c, "krunlet_test")
	for _, expect := range []string{
		"add table inet krunlet_test",
		"type filter hook output priority -200; policy drop;",
		"ip daddr 127.0.0.0/8 drop",
		"ip6 daddr fc00::/7 drop",
		"ip daddr 1.1.1.2/32 meta l4proto tcp drop",
		"ip daddr 1.1.1.1/32 meta l4proto tcp tcp dport 443 accept",
		"ip6 daddr 2001:db8::/32 meta l4proto udp udp dport 53 accept",
	} {
		if !strings.Contains(s, expect) {
			t.Errorf("missing %q in:\n%s", expect, s)
		}
	}
	if strings.Contains(s, "1.1.1.1/32 meta l4proto udp") {
		t.Fatal("TCP-only rule accidentally permits UDP")
	}
}

func TestNFTBlocklistScript(t *testing.T) {
	c, err := compileNetworkPolicy(&NetworkPolicy{Mode: NetworkBlocklist, Namespace: "private", Block: []NetworkRule{{CIDR: "198.51.100.0/24", Port: 8443}}})
	if err != nil {
		t.Fatal(err)
	}
	s := nftPolicyScript(c, "krunlet_test")
	if !strings.Contains(s, "policy accept;") {
		t.Fatal("blocklist should allow by default")
	}
	for _, proto := range []string{"tcp", "udp"} {
		if !strings.Contains(s, "ip daddr 198.51.100.0/24 meta l4proto "+proto+" "+proto+" dport 8443 drop") {
			t.Fatalf("missing protocol %s", proto)
		}
	}
}
