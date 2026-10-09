//go:build linux

package krunlet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// networkLease keeps the output firewall installed throughout the helper's
// lifetime. Cleanup must happen only after the helper has exited.
type networkLease func()

func prepareNetwork(ctx context.Context, p *NetworkPolicy, cmd *exec.Cmd) (networkLease, error) {
	if p == nil {
		return func() {}, nil
	}
	c, err := compileNetworkPolicy(p)
	if err != nil {
		return nil, err
	}
	name := c.namespace
	nsPath := filepath.Join("/run/netns", name)
	ns, err := os.Stat(nsPath)
	if err != nil {
		return nil, fmt.Errorf("network namespace %q: %w", name, err)
	}
	host, err := os.Stat("/proc/self/ns/net")
	if err != nil {
		return nil, err
	}
	if os.SameFile(ns, host) {
		return nil, fmt.Errorf("network namespace %q is the host network namespace", name)
	}
	ipPath, err := exec.LookPath("ip")
	if err != nil {
		return nil, fmt.Errorf("restricted networking requires iproute2: %w", err)
	}
	nftPath, err := exec.LookPath("nft")
	if err != nil {
		return nil, fmt.Errorf("restricted networking requires nftables: %w", err)
	}
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		return nil, err
	}
	table := "krunlet_" + hex.EncodeToString(random[:])
	script := nftPolicyScript(c, table)
	setupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	setup := exec.CommandContext(setupCtx, ipPath, "netns", "exec", name, nftPath, "-f", "-")
	setup.Stdin = strings.NewReader(script)
	output, err := setup.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("install restricted network rules in namespace %q: %w: %s", name, err, strings.TrimSpace(string(output)))
	}
	closeLease := func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		// If cleanup fails, a restrictive ruleset remains in the dedicated
		// namespace. Operators must not reuse it until they inspect it.
		_ = exec.CommandContext(cleanupCtx, ipPath, "netns", "exec", name,
			nftPath, "delete", "table", "inet", table).Run()
	}
	// ip netns exec performs setns before launching the actual helper.
	// Do not pass the command through a shell.
	original := append([]string(nil), cmd.Args...)
	cmd.Path = ipPath
	cmd.Args = append([]string{ipPath, "netns", "exec", name}, original...)
	return closeLease, nil
}

// nftPolicyScript emits kernel-enforced OUTPUT rules in a dedicated network
// namespace. An allowlist chain defaults to DROP for both IP families, and
// even an empty allowlist denies all connections.
func nftPolicyScript(p *compiledNetworkPolicy, table string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "add table inet %s\n", table)
	policy := "accept"
	if p.mode == NetworkAllowlist {
		policy = "drop"
	}
	fmt.Fprintf(&b, "add chain inet %s output { type filter hook output priority -200; policy %s; }\n", table, policy)
	add := func(rule string) {
		fmt.Fprintf(&b, "add rule inet %s output %s\n", table, rule)
	}
	if p.blockPrivate {
		for _, prefix := range blockedPrivatePrefixes {
			family := "ip"
			if prefix.Addr().Is6() {
				family = "ip6"
			}
			add(fmt.Sprintf("%s daddr %s drop", family, prefix))
		}
	}
	for _, rule := range p.block {
		nftRule(rule, "drop", add)
	}
	if p.mode == NetworkAllowlist {
		for _, rule := range p.allow {
			nftRule(rule, "accept", add)
		}
	}
	return b.String()
}

func nftRule(r compiledRule, verdict string, add func(string)) {
	family := "ip"
	if r.prefix.Addr().Is6() {
		family = "ip6"
	}
	base := family + " daddr " + r.prefix.String()
	for _, proto := range []string{"tcp", "udp"} {
		if r.protocol != "" && r.protocol != proto {
			continue
		}
		rule := base + " meta l4proto " + proto
		if r.port != 0 {
			rule += " " + proto + " dport " + strconv.FormatUint(uint64(r.port), 10)
		}
		add(rule + " " + verdict)
	}
}
