package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Asutorufa/krunlet"
	"github.com/Asutorufa/krunlet/internal/krunffi"
)

const version = "0.2.0-dev"

type repeated []string

func (v *repeated) String() string     { return strings.Join(*v, ",") }
func (v *repeated) Set(s string) error { *v = append(*v, s); return nil }

func main() { os.Exit(run()) }
func run() int {
	if len(os.Args) < 2 {
		usage()
		return 2
	}
	switch os.Args[1] {
	case "__helper":
		f := flag.NewFlagSet("__helper", flag.ContinueOnError)
		name := f.String("config", "", "helper config")
		if f.Parse(os.Args[2:]) != nil || *name == "" {
			return 125
		}
		if e := krunlet.HelperParentWatch(); e != nil {
			fmt.Fprintln(os.Stderr, e)
			return 125
		}
		h, e := os.Open(*name)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			return 125
		}
		var config krunffi.Config
		e = json.NewDecoder(h).Decode(&config)
		h.Close()
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			return 125
		}
		if e = krunffi.Enter(config); e != nil {
			if config.ErrorPath != "" {
				_ = os.WriteFile(config.ErrorPath, []byte(e.Error()), 0600)
			}
			fmt.Fprintln(os.Stderr, e)
			return 125
		}
		return 125 // native Enter success never returns
	case "doctor":
		f := flag.NewFlagSet("doctor", flag.ContinueOnError)
		lib := f.String("lib", "", "libkrun shared library path")
		rootfs := f.String("rootfs", "", "trusted rootfs for a real /bin/true VM smoke test (optional)")
		if f.Parse(os.Args[2:]) != nil {
			return 2
		}
		report, e := krunlet.DoctorDetailed(context.Background(), *lib, *rootfs)
		_ = json.NewEncoder(os.Stdout).Encode(report)
		if e != nil {
			fmt.Fprintln(os.Stderr, "not ready:", e)
			return 1
		}
		return 0
	case "version":
		fmt.Println("krunlet", version)
		return 0
	case "run", "exec":
		f := flag.NewFlagSet("run", flag.ContinueOnError)
		root := f.String("rootfs", "", "trusted Linux root filesystem directory (required)")
		work := f.String("workdir", "/", "guest working directory")
		cpu := f.Uint("cpus", 2, "vCPU count")
		mem := f.Uint("memory", 512, "guest memory MiB")
		timeout := f.Duration("timeout", 30*time.Second, "wall clock limit")
		output := f.Int64("output-limit", 4<<20, "stdout+stderr cap in bytes")
		maxFile := f.Int64("file-limit", 4<<20, "input/output file cap in bytes")
		stdinString := f.String("stdin", "", "literal guest stdin")
		stdinFile := f.String("stdin-file", "", "guest stdin from host file or '-' for piped stdin")
		lib := f.String("lib", "", "libkrun library path")
		kernel := f.String("kernel", "", "host path to a custom Linux kernel image")
		kernelFormat := f.String("kernel-format", "raw", "kernel format: raw, elf, pe-gz, image-bz2, image-gz, image-zstd")
		initramfs := f.String("initramfs", "", "optional host path to initramfs (requires --kernel)")
		kernelCmdline := f.String("kernel-cmdline", "", "optional Linux kernel command line (requires --kernel)")
		net := f.Bool("network", false, "allow guest outbound network via TSI")
		netMode := f.String("net-mode", "", "restricted egress: allowlist or blocklist (requires --network)")
		yuhaiin := f.String("yuhaiin-socket", "", "delegate guest networking to local yuhaiin krunlet inbound Unix socket")
		blockPrivate := f.Bool("block-private", false, "reject private/local IP destinations in restricted mode")
		persistent := f.Bool("persistent", false, "allow changes to original rootfs (unsafe)")
		maxConcurrent := f.Int("max-concurrent-vms", 0, "per-Runner concurrent VM limit (default min(4,NumCPU/2))")
		processMax := f.Int("process-max-vms", 0, "global VM limit shared by every Runner in this Go process")
		cgroupParent := f.String("cgroup-parent", "", "administrator-delegated cgroup v2 parent (Linux)")
		cgroupMemory := f.Int64("cgroup-memory-max-bytes", 0, "host helper cgroup memory.max, 0 uses guest memory plus overhead")
		cgroupPids := f.Int64("cgroup-pids-max", 0, "host helper cgroup pids.max, 0 defaults to 128")
		failFast := f.Bool("fail-fast", false, "return ErrTooManyVMs instead of waiting")
		maxRootFS := f.Int64("max-rootfs-bytes", 1<<30, "logical bytes allowed in staged rootfs")

		jsonOut := f.Bool("json", false, "print structured result JSON")
		var envs, ports, limits, inputFiles, outputFiles, allowCIDRs, blockCIDRs repeated
		f.Var(&envs, "env", "guest environment KEY=VALUE (repeatable)")
		f.Var(&ports, "port", "inbound port HOST:GUEST (repeatable; requires --network)")
		f.Var(&allowCIDRs, "allow-cidr", "allow destination: IP_OR_CIDR[,PORT[,tcp|udp]] (repeatable)")
		f.Var(&blockCIDRs, "block-cidr", "block destination: IP_OR_CIDR[,PORT[,tcp|udp]] (repeatable)")
		f.Var(&limits, "rlimit", "Linux rlimit ID=SOFT:HARD (repeatable)")
		f.Var(&inputFiles, "file", "host file into guest: GUEST=/host/path (repeatable)")
		f.Var(&outputFiles, "collect", "guest result file to collect (repeatable; requires --json)")
		if e := f.Parse(os.Args[2:]); e != nil {
			return 2
		}
		if *cpu > 255 || *mem > uint(^uint32(0)) {
			fmt.Fprintln(os.Stderr, "invalid VM resources")
			return 2
		}
		command := f.Args()
		if len(command) > 0 && command[0] == "--" {
			command = command[1:]
		}
		if *root == "" || len(command) == 0 {
			f.Usage()
			return 2
		}
		if *stdinFile != "" && *stdinString != "" {
			fmt.Fprintln(os.Stderr, "--stdin and --stdin-file are mutually exclusive")
			return 2
		}
		if len(outputFiles) > 0 && !(*jsonOut) {
			fmt.Fprintln(os.Stderr, "--collect requires --json")
			return 2
		}
		env := map[string]string{}
		for _, s := range envs {
			k, v, ok := strings.Cut(s, "=")
			if !ok || k == "" {
				fmt.Fprintln(os.Stderr, "invalid --env:", s)
				return 2
			}
			env[k] = v
		}
		inputs := map[string][]byte{}
		for _, s := range inputFiles {
			guest, host, ok := strings.Cut(s, "=")
			if !ok || guest == "" || host == "" {
				fmt.Fprintln(os.Stderr, "invalid --file:", s)
				return 2
			}
			b, e := readLimited(host, *maxFile)
			if e != nil {
				fmt.Fprintln(os.Stderr, "--file:", e)
				return 2
			}
			inputs[guest] = b
		}
		stdin := *stdinString
		if *stdinFile != "" {
			var e error
			if *stdinFile == "-" {
				var b []byte
				b, e = io.ReadAll(io.LimitReader(os.Stdin, *maxFile+1))
				stdin = string(b)
				if int64(len(b)) > *maxFile {
					e = errors.New("stdin limit exceeded")
				}
			} else {
				var b []byte
				b, e = readLimited(*stdinFile, *maxFile)
				stdin = string(b)
			}
			if e != nil {
				fmt.Fprintln(os.Stderr, "stdin:", e)
				return 2
			}
		}
		var customKernel *krunlet.KernelConfig
		if *kernel != "" {
			format, e := krunlet.ParseKernelFormat(*kernelFormat)
			if e != nil {
				fmt.Fprintln(os.Stderr, e)
				return 2
			}
			customKernel = &krunlet.KernelConfig{
				Path: *kernel, Format: format, Initrd: *initramfs, Cmdline: *kernelCmdline,
			}
		} else {
			var extraKernelFlag bool
			f.Visit(func(fl *flag.Flag) {
				switch fl.Name {
				case "kernel-format", "initramfs", "kernel-cmdline":
					extraKernelFlag = true
				}
			})
			if extraKernelFlag {
				fmt.Fprintln(os.Stderr, "--kernel-format, --initramfs and --kernel-cmdline require --kernel")
				return 2
			}
		}
		var networkPolicy *krunlet.NetworkPolicy
		if *netMode != "" {
			networkPolicy = &krunlet.NetworkPolicy{
				Mode: krunlet.NetworkMode(*netMode), BlockPrivateNetworks: *blockPrivate,
			}
			for _, spec := range allowCIDRs {
				rule, e := parseNetworkRule(spec)
				if e != nil {
					fmt.Fprintln(os.Stderr, "--allow-cidr:", e)
					return 2
				}
				networkPolicy.Allow = append(networkPolicy.Allow, rule)
			}
			for _, spec := range blockCIDRs {
				rule, e := parseNetworkRule(spec)
				if e != nil {
					fmt.Fprintln(os.Stderr, "--block-cidr:", e)
					return 2
				}
				networkPolicy.Block = append(networkPolicy.Block, rule)
			}
		} else if *blockPrivate || len(allowCIDRs) > 0 || len(blockCIDRs) > 0 {
			fmt.Fprintln(os.Stderr, "--block-private, --allow-cidr, --block-cidr require --net-mode")
			return 2
		}
		if *persistent {
			fmt.Fprintln(os.Stderr, "WARNING: --persistent writes directly into the trusted host rootfs; this is not safe for untrusted commands")
		}
		if *processMax != 0 {
			if e:=krunlet.ConfigureProcessVMLimit(*processMax);e!=nil {
				fmt.Fprintln(os.Stderr,"--process-max-vms:",e)
				return 2
			}
		}
		if *cgroupParent == "" && (*cgroupMemory != 0 || *cgroupPids != 0) {
			fmt.Fprintln(os.Stderr,"cgroup memory/pids limits require --cgroup-parent")
			return 2
		}
		var cg *krunlet.CgroupV2
		if *cgroupParent!="" {
			cg=&krunlet.CgroupV2{Parent:*cgroupParent,MemoryMaxBytes:*cgroupMemory,PidsMax:*cgroupPids}
		}
		runner, e := krunlet.New(krunlet.Options{RootFS: *root, CPUs: uint8(*cpu), MemoryMiB: uint32(*mem),
			Timeout: *timeout, MaxOutputBytes: *output, MaxFileBytes: *maxFile, Network: *net,
			PortMaps: ports, RLimits: limits, Persistent: *persistent, LibraryPath: *lib,
			Kernel: customKernel, NetworkPolicy: networkPolicy,
			MaxConcurrentVMs: *maxConcurrent, FailFast: *failFast, MaxRootFSBytes: *maxRootFS,
			CgroupV2: cg,
			Yuhaiin: func() *krunlet.YuhaiinConfig {
				if *yuhaiin == "" {
					return nil
				}
				return &krunlet.YuhaiinConfig{Socket: *yuhaiin}
			}()})
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			return 125
		}
		res, e := runner.Run(context.Background(), krunlet.Request{Command: command, WorkDir: *work, Env: env, Stdin: stdin, Files: inputs, Collect: outputFiles})
		if *jsonOut {
			_ = json.NewEncoder(os.Stdout).Encode(res)
		} else {
			fmt.Fprint(os.Stdout, res.Stdout)
			fmt.Fprint(os.Stderr, res.Stderr)
		}
		if e != nil {
			fmt.Fprintln(os.Stderr, "krunlet:", e)
			if errors.Is(e, context.DeadlineExceeded) {
				return 124
			}
			return 125
		}
		if res.ExitCode < 0 {
			return 125
		}
		return res.ExitCode
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", strings.Join(os.Args[1:], " "))
		usage()
		return 2
	}
}
func readLimited(path string, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, errors.New("invalid file limit")
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	fi, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !fi.Mode().IsRegular() || fi.Size() > limit {
		return nil, errors.New("not a regular file or file too large")
	}
	data, e := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(data)) > limit {
		return nil, errors.New("file too large")
	}
	return data, e
}
func usage() {
	fmt.Fprintln(os.Stderr, "Usage: krunlet run --rootfs DIR [flags] -- /bin/sh -lc 'echo hello' | doctor | version")
}

func parseNetworkRule(spec string) (krunlet.NetworkRule, error) {
	pieces := strings.Split(spec, ",")
	if len(pieces) < 1 || len(pieces) > 3 || pieces[0] == "" {
		return krunlet.NetworkRule{}, fmt.Errorf("invalid rule %q", spec)
	}
	r := krunlet.NetworkRule{CIDR: pieces[0]}
	if len(pieces) >= 2 {
		p, err := strconv.ParseUint(pieces[1], 10, 16)
		if err != nil || p == 0 {
			return r, fmt.Errorf("invalid port in %q", spec)
		}
		r.Port = uint16(p)
	}
	if len(pieces) == 3 {
		r.Protocol = pieces[2]
	}
	return r, nil
}
