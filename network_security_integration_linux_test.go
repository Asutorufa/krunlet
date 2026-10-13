//go:build linux && vm_integration

package krunlet

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVMNetworkPolicyAgainstHostCanary(t *testing.T) {
	rootFS := integrationRootFS(t)
	hostIP := integrationHostIPv4(t)
	tests := []struct {
		name     string
		network  bool
		policy   *NetworkPolicy
		wantDial bool
	}{
		{name: "unrestricted_network_control", network: true, wantDial: true},
		{name: "empty_allowlist_blocks", network: true, policy: &NetworkPolicy{Mode: NetworkAllowlist}},
		{
			name:     "allowlist_permits_exact_destination",
			network:  true,
			policy:   &NetworkPolicy{Mode: NetworkAllowlist, Allow: []NetworkRule{{CIDR: netip.PrefixFrom(hostIP, 32).String(), Protocol: "tcp"}}},
			wantDial: true,
		},
		{
			name:    "private_network_block_overrides_allow",
			network: true,
			policy:  &NetworkPolicy{Mode: NetworkAllowlist, Allow: []NetworkRule{{CIDR: netip.PrefixFrom(hostIP, 32).String(), Protocol: "tcp"}}, BlockPrivateNetworks: true},
		},
		{name: "network_disabled", network: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "private_network_block_overrides_allow" && !hostIP.IsPrivate() {
				t.Skip("no private host IPv4 address is available for this policy check")
			}
			listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IP(hostIP.AsSlice())})
			if err != nil {
				t.Fatalf("listen on host canary address %s: %v", hostIP, err)
			}
			defer listener.Close()
			accepted := make(chan struct{}, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				accepted <- struct{}{}
				_, _ = fmt.Fprint(conn, "canary")
			}()

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			runner, err := New(Options{
				RootFS: rootFS, CPUs: 1, MemoryMiB: 256,
				Network: tt.network, NetworkPolicy: tt.policy,
				CgroupParent: integrationCgroup(t), Timeout: 15 * time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			port := listener.Addr().(*net.TCPAddr).Port
			result, runErr := runner.Run(ctx, Request{Command: []string{"/bin/busybox", "nc", "-w", "3", hostIP.String(), fmt.Sprint(port)}})
			if runErr != nil {
				t.Fatalf("run guest network probe: %v (stdout=%q stderr=%q)", runErr, result.Stdout, result.Stderr)
			}
			if tt.wantDial {
				if result.ExitCode != 0 {
					t.Fatalf("allowed guest connection failed: exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
				}
				select {
				case <-accepted:
				case <-time.After(time.Second):
					t.Fatal("host canary did not observe the allowed guest connection")
				}
				return
			}
			if result.ExitCode == 0 {
				t.Fatalf("guest reached blocked host canary: exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
			}
			select {
			case <-accepted:
				t.Fatal("host canary observed a connection that policy should block")
			case <-time.After(200 * time.Millisecond):
			}
		})
	}
}

func TestVMIoCanaryCannotEscapeGuestRoot(t *testing.T) {
	rootFS := filepath.Join(t.TempDir(), "fixture-rootfs")
	if err := os.Mkdir(rootFS, 0700); err != nil {
		t.Fatal(err)
	}
	if err := copyRootFS(context.Background(), integrationRootFS(t), rootFS); err != nil {
		t.Fatalf("copy test rootfs: %v", err)
	}
	const marker = "krunlet-host-canary-7c94d2"
	outside := t.TempDir()
	canary := filepath.Join(outside, "canary")
	if err := os.WriteFile(canary, []byte(marker), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootFS, "etc"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"krunlet-canary-absolute", "krunlet-canary-relative"} {
		if err := os.Remove(filepath.Join(rootFS, "etc", name)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(canary, filepath.Join(rootFS, "etc", "krunlet-canary-absolute")); err != nil {
		t.Fatal(err)
	}

	// Krunlet stages each ephemeral guest under TMPDIR. Placing the relative
	// canary beside that private staging directory makes upward traversal
	// attempts observable without exposing any real host data.
	scratch := t.TempDir()
	relativeCanary := filepath.Join(scratch, "canary")
	if err := os.WriteFile(relativeCanary, []byte(marker), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../canary", filepath.Join(rootFS, "etc", "krunlet-canary-relative")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", scratch)

	runner, err := New(Options{
		RootFS: rootFS, CPUs: 1, MemoryMiB: 256, CgroupParent: integrationCgroup(t),
		Timeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), Request{Command: []string{
		"/bin/busybox", "cat", "/etc/krunlet-canary-absolute", "/etc/krunlet-canary-relative",
	}})
	if err != nil {
		t.Fatalf("run guest filesystem probe: %v (stdout=%q stderr=%q)", err, result.Stdout, result.Stderr)
	}
	if result.ExitCode == 0 || strings.Contains(result.Stdout, marker) {
		t.Fatalf("guest read host canary outside its root: exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
}

func integrationHostIPv4(t *testing.T) netip.Addr {
	t.Helper()
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	var fallback netip.Addr
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err != nil {
				continue
			}
			ip := prefix.Addr().Unmap()
			if !ip.Is4() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if ip.IsPrivate() {
				return ip
			}
			fallback = ip
		}
	}
	if fallback.IsValid() {
		return fallback
	}
	t.Fatal("no usable non-loopback host IPv4 address for the network canary")
	return netip.Addr{}
}
