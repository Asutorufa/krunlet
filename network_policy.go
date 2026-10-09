package krunlet

import (
	"fmt"
	"net/netip"
	"strings"
)

// NetworkMode chooses whether destination rules allow or block outbound
// connections. Rules never control inbound port forwarding.
type NetworkMode string

const (
	NetworkAllowlist NetworkMode = "allowlist"
	NetworkBlocklist NetworkMode = "blocklist"
)

// NetworkRule matches an IP address or CIDR, optionally restricted by port
// and transport protocol. An empty protocol means both TCP and UDP; port 0
// means all ports. Only IP literals and CIDRs are supported, not domains.
type NetworkRule struct {
	CIDR     string
	Port     uint16
	Protocol string // "", "tcp", or "udp"
}

// NetworkPolicy is enforced at gVisor's TCP and UDP host-dial boundary.
// IPv4 and IPv6 TCP/UDP are supported. Inbound connections, other IP
// protocols, and DNS forwarding are not enabled.
type NetworkPolicy struct {
	Mode                 NetworkMode
	Allow                []NetworkRule
	Block                []NetworkRule
	BlockPrivateNetworks bool
}

type compiledRule struct {
	prefix   netip.Prefix
	port     uint16
	protocol string
}

type compiledNetworkPolicy struct {
	mode         NetworkMode
	allow        []compiledRule
	block        []compiledRule
	blockPrivate bool
}

func normalizeNetworkPolicy(p *NetworkPolicy) (*NetworkPolicy, error) {
	if p == nil {
		return nil, nil
	}
	copy := *p
	copy.Allow = append([]NetworkRule(nil), p.Allow...)
	copy.Block = append([]NetworkRule(nil), p.Block...)
	if _, err := compileNetworkPolicy(&copy); err != nil {
		return nil, err
	}
	return &copy, nil
}

func compileNetworkPolicy(p *NetworkPolicy) (*compiledNetworkPolicy, error) {
	if p == nil {
		return nil, nil
	}
	if p.Mode != NetworkAllowlist && p.Mode != NetworkBlocklist {
		return nil, fmt.Errorf("network policy mode must be allowlist or blocklist")
	}
	out := &compiledNetworkPolicy{mode: p.Mode, blockPrivate: p.BlockPrivateNetworks}
	parse := func(label string, rules []NetworkRule) ([]compiledRule, error) {
		result := make([]compiledRule, 0, len(rules))
		for i, r := range rules {
			var prefix netip.Prefix
			var err error
			if strings.Contains(r.CIDR, "/") {
				prefix, err = netip.ParsePrefix(r.CIDR)
			} else {
				var addr netip.Addr
				addr, err = netip.ParseAddr(r.CIDR)
				if err == nil {
					prefix = netip.PrefixFrom(addr, addr.BitLen())
				}
			}
			if err != nil || !prefix.IsValid() || prefix.Addr().Is4In6() || prefix.Addr().Zone() != "" {
				return nil, fmt.Errorf("%s[%d]: invalid IP/CIDR %q", label, i, r.CIDR)
			}
			if r.Protocol != "" && r.Protocol != "tcp" && r.Protocol != "udp" {
				return nil, fmt.Errorf("%s[%d]: protocol must be tcp or udp", label, i)
			}
			result = append(result, compiledRule{prefix: prefix.Masked(), port: r.Port, protocol: r.Protocol})
		}
		return result, nil
	}
	var err error
	if out.allow, err = parse("allow", p.Allow); err != nil {
		return nil, err
	}
	if out.block, err = parse("block", p.Block); err != nil {
		return nil, err
	}
	if p.Mode == NetworkBlocklist && len(p.Allow) > 0 {
		return nil, fmt.Errorf("allow rules are only valid in allowlist mode")
	}
	return out, nil
}

// Allows reports the destination decision used by the gVisor TCP/UDP
// forwarders BEFORE they open a host connection. DNS is never resolved here.
func (p *NetworkPolicy) Allows(ip netip.Addr, port uint16, protocol string) (bool, error) {
	c, err := compileNetworkPolicy(p)
	if err != nil {
		return false, err
	}
	if !ip.IsValid() || ip.Zone() != "" || port == 0 ||
		(protocol != "tcp" && protocol != "udp") {
		return false, fmt.Errorf("invalid destination")
	}
	ip = ip.Unmap()
	if c.blockPrivate && isBlockedPrivate(ip) {
		return false, nil
	}
	match := func(rules []compiledRule) bool {
		for _, r := range rules {
			if r.prefix.Contains(ip) && (r.port == 0 || r.port == port) &&
				(r.protocol == "" || r.protocol == protocol) {
				return true
			}
		}
		return false
	}
	if match(c.block) {
		return false, nil
	}
	if c.mode == NetworkAllowlist {
		return match(c.allow), nil
	}
	return true, nil
}

var blockedPrivatePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

func isBlockedPrivate(addr netip.Addr) bool {
	for _, p := range blockedPrivatePrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
