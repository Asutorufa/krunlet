package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
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
		if f.Parse(os.Args[2:]) != nil {
			return 2
		}
		if e := krunlet.Doctor(*lib); e != nil {
			fmt.Fprintln(os.Stderr, "not ready:", e)
			return 1
		}
		fmt.Println("libkrun ABI symbols found; run a VM to verify virtualization permissions")
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
		net := f.Bool("network", false, "allow guest outbound network via TSI")
		persistent := f.Bool("persistent", false, "allow changes to original rootfs (unsafe)")
		jsonOut := f.Bool("json", false, "print structured result JSON")
		var envs, ports, limits, inputFiles, outputFiles repeated
		f.Var(&envs, "env", "guest environment KEY=VALUE (repeatable)")
		f.Var(&ports, "port", "inbound port HOST:GUEST (repeatable; requires --network)")
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
		runner, e := krunlet.New(krunlet.Options{RootFS: *root, CPUs: uint8(*cpu), MemoryMiB: uint32(*mem),
			Timeout: *timeout, MaxOutputBytes: *output, MaxFileBytes: *maxFile, Network: *net,
			PortMaps: ports, RLimits: limits, Persistent: *persistent, LibraryPath: *lib})
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
