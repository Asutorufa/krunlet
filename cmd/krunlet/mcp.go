package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/Asutorufa/krunlet"
	"github.com/Asutorufa/krunlet/internal/mcp"
)

// runMCP is intentionally narrower than "run": tool callers cannot enable
// persistence, read host files, change the rootfs, or alter network policy.
func runMCP(args []string) int {
	f := flag.NewFlagSet("mcp", flag.ContinueOnError)
	f.SetOutput(os.Stderr)
	rootfs := f.String("rootfs", "", "trusted absolute host rootfs directory (required)")
	cpus := f.Uint("cpus", 2, "VM vCPU count")
	memory := f.Uint("memory", 512, "VM memory in MiB")
	timeout := f.Duration("timeout", 30*time.Second, "maximum wall clock time per tool call")
	outputLimit := f.Int64("output-limit", 4<<20, "combined guest stdout/stderr limit")
	fileLimit := f.Int64("file-limit", 4<<20, "maximum size of each input/output guest file")
	rootfsLimit := f.Int64("max-rootfs-bytes", 1<<30, "maximum logical bytes in staged rootfs")
	concurrent := f.Int("max-concurrent-vms", 0, "per-Runner concurrency limit (0=default)")
	failFast := f.Bool("fail-fast", false, "reject instead of queueing when VM limit is reached")
	lib := f.String("lib", "", "trusted host libkrun library path")
	cgroupParent := f.String("cgroup-parent", "", "delegated cgroup v2 directory for Linux host containment")
	cgroupMemory := f.Int64("cgroup-memory-max", 0, "host VMM memory.max (0=guest+256MiB)")
	cgroupPids := f.Int64("cgroup-pids-max", 0, "host VMM pids.max (0=256)")
	network := f.Bool("network", false, "enable restricted outbound TCP/UDP; requires --allow-cidr")
	blockPrivate := f.Bool("block-private", true, "block private/link-local address space when networking is enabled")
	var allowCIDRs, blockCIDRs repeated
	f.Var(&allowCIDRs, "allow-cidr", "permitted IP/CIDR[,PORT[,tcp|udp]] (repeatable)")
	f.Var(&blockCIDRs, "block-cidr", "blocked IP/CIDR[,PORT[,tcp|udp]] (repeatable)")
	if err := f.Parse(args); err != nil {
		return 2
	}
	if *rootfs == "" || f.NArg() != 0 || *cpus == 0 || *cpus > 64 || *memory < 128 || *memory > 65536 ||
		*timeout <= 0 || *outputLimit <= 0 || *fileLimit <= 0 || *rootfsLimit <= 0 || *concurrent < 0 {
		fmt.Fprintln(os.Stderr, "invalid MCP flags: --rootfs is required and resource limits must be positive")
		f.Usage()
		return 2
	}
	if (!*network && (len(allowCIDRs) != 0 || len(blockCIDRs) != 0)) || (*network && len(allowCIDRs) == 0) {
		fmt.Fprintln(os.Stderr, "MCP networking requires --network and at least one --allow-cidr; unrestricted TSI is not supported")
		return 2
	}
	var policy *krunlet.NetworkPolicy
	if *network {
		policy = &krunlet.NetworkPolicy{Mode: krunlet.NetworkAllowlist, BlockPrivateNetworks: *blockPrivate}
		for _, spec := range allowCIDRs {
			rule, err := parseNetworkRule(spec)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 2
			}
			policy.Allow = append(policy.Allow, rule)
		}
		for _, spec := range blockCIDRs {
			rule, err := parseNetworkRule(spec)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 2
			}
			policy.Block = append(policy.Block, rule)
		}
	}
	runner, err := krunlet.New(krunlet.Options{
		RootFS: *rootfs, CPUs: uint8(*cpus), MemoryMiB: uint32(*memory),
		Timeout: *timeout, MaxOutputBytes: *outputLimit, MaxFileBytes: *fileLimit,
		MaxRootFSBytes: *rootfsLimit, MaxConcurrentVMs: *concurrent, FailFast: *failFast,
		LibraryPath: *lib, Network: *network, NetworkPolicy: policy,
		CgroupParent: *cgroupParent, CgroupMemoryMaxBytes: *cgroupMemory, CgroupPidsMax: *cgroupPids,
		// Persistent is always false; guest changes must not affect the host template.
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "krunlet mcp:", err)
		return 125
	}
	if runtime.GOOS == "linux" && *cgroupParent == "" {
		fmt.Fprintln(os.Stderr, "krunlet mcp: WARNING: no --cgroup-parent; process-group cleanup alone cannot contain setsid descendants")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Stop the blocking scanner on a shutdown signal, then let Serve cancel
	// and reap any currently running microVMs.
	go func() {
		<-ctx.Done()
		_ = os.Stdin.Close()
	}()
	if err := mcp.Serve(ctx, os.Stdin, os.Stdout, runner, *timeout, *fileLimit, version); err != nil {
		fmt.Fprintln(os.Stderr, "krunlet mcp:", err)
		return 125
	}
	return 0
}
