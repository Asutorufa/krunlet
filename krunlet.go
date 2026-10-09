// Package krunlet executes untrusted commands in disposable libkrun microVMs.
// It runs libkrun inside a separate helper process, because the native
// krun_start_enter routine terminates its calling process on VM shutdown.
package krunlet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/Asutorufa/krunlet/internal/krunffi"
)

// Options are runner-wide defaults. A trusted, prepared Linux rootfs directory
// is required; krunlet does not download unverified container images.
type Options struct {
	RootFS      string
	HelperPath  string
	LibraryPath string
	// Kernel selects a host-side kernel image. Nil uses the bundled libkrunfw
	// kernel. Custom kernel paths are trusted host inputs, not guest paths.
	Kernel         *KernelConfig
	CPUs           uint8
	MemoryMiB      uint32
	Timeout        time.Duration
	MaxOutputBytes int64
	MaxFileBytes   int64
	// Network opts into libkrun's TSI forwarding through the host network.
	// False disables socket hijacking with krun_add_vsock(ctx, 0).
	Network bool
	// NetworkPolicy enables a gVisor Netstack virtio-net gateway and filters
	// outbound TCP/UDP connections before any host-side dial. It requires
	// Network=true. Without a policy, Network=true uses unrestricted TSI.
	NetworkPolicy *NetworkPolicy
	// Ephemeral copies the trusted rootfs into a private temp directory for
	// every invocation, preventing guest changes from persisting. Default true.
	// Setting false runs directly on RootFS and is not recommended for agents.
	Persistent bool
	// PortMaps is an allow-list of host:guest TCP ports, empty denies inbound.
	PortMaps []string
	// RLimits are Linux rlimit strings like "7=256:256" (RLIMIT_NOFILE).
	RLimits []string
}

type Request struct {
	Command []string
	WorkDir string
	Env     map[string]string
	Stdin   string
	// Files are written into a private rootfs before launching the VM.
	Files map[string][]byte
	// Collect lists guest-absolute output paths to retrieve after shutdown.
	Collect []string
	Timeout time.Duration
}

type Result struct {
	ExitCode      int               `json:"exit_code"`
	Stdout        string            `json:"stdout"`
	Stderr        string            `json:"stderr"`
	Duration      time.Duration     `json:"duration"`
	TimedOut      bool              `json:"timed_out"`
	OutputLimited bool              `json:"output_limited"`
	Files         map[string][]byte `json:"files,omitempty"`
}

type Runner struct {
	cfg        Options
	autoHelper bool
}

func New(opts Options) (*Runner, error) {
	if opts.RootFS == "" {
		return nil, errors.New("RootFS is required")
	}
	root, err := filepath.Abs(opts.RootFS)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, errors.New("rootfs must be a directory")
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	opts.RootFS = root
	if opts.Kernel, err = normalizeKernel(opts.Kernel); err != nil {
		return nil, fmt.Errorf("kernel: %w", err)
	}
	if opts.CPUs == 0 {
		opts.CPUs = 2
	}
	if opts.MemoryMiB == 0 {
		opts.MemoryMiB = 512
	}
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.MaxOutputBytes == 0 {
		opts.MaxOutputBytes = 4 << 20
	}
	if opts.MaxFileBytes == 0 {
		opts.MaxFileBytes = 4 << 20
	}
	if opts.CPUs > 64 || opts.MemoryMiB < 128 || opts.MemoryMiB > 65536 {
		return nil, errors.New("CPU or memory limit out of range")
	}
	if opts.Timeout < 0 || opts.MaxOutputBytes < 0 || opts.MaxFileBytes < 0 {
		return nil, errors.New("limits cannot be negative")
	}
	// The library is self-contained: by default its importing executable
	// re-executes itself and the package init dispatches directly to the helper.
	// Callers do not need to install the CLI alongside their Go application.
	autoHelper := opts.HelperPath == ""
	if autoHelper {
		opts.HelperPath, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("locate caller executable: %w", err)
		}
	}
	for _, pm := range opts.PortMaps {
		if err = validatePortMap(pm); err != nil {
			return nil, err
		}
	}
	if !opts.Network && len(opts.PortMaps) > 0 {
		return nil, errors.New("port mappings require Network=true")
	}
	if opts.NetworkPolicy != nil {
		if !opts.Network {
			return nil, errors.New("NetworkPolicy requires Network=true")
		}
		if len(opts.PortMaps) != 0 {
			return nil, errors.New("NetworkPolicy does not support inbound PortMaps")
		}
		if opts.NetworkPolicy, err = normalizeNetworkPolicy(opts.NetworkPolicy); err != nil {
			return nil, fmt.Errorf("network policy: %w", err)
		}
	}
	for _, rl := range opts.RLimits {
		if err = validateRLimit(rl); err != nil {
			return nil, err
		}
	}
	return &Runner{cfg: opts, autoHelper: autoHelper}, nil
}

// Run executes a one-shot microVM, capturing stdout and stderr.
func (r *Runner) Run(ctx context.Context, req Request) (Result, error) {
	return r.run(ctx, req, nil, nil, nil)
}

// RunStream executes a one-shot microVM, streaming stdout/stderr to the
// supplied writers while also retaining the bounded output in Result.
// A writer failure aborts the VM. Nil writers discard the streamed copy.
func (r *Runner) RunStream(ctx context.Context, req Request, stdout, stderr io.Writer) (Result, error) {
	return r.run(ctx, req, nil, stdout, stderr)
}

// RunIO streams input from stdin and output to stdout/stderr. If stdin is
// nil, Request.Stdin is used; a non-nil reader overrides it. Cancellation
// stops the microVM. The Result still captures bounded stdout and stderr.
func (r *Runner) RunIO(ctx context.Context, req Request, stdin io.Reader, stdout, stderr io.Writer) (Result, error) {
	return r.run(ctx, req, stdin, stdout, stderr)
}

func (r *Runner) run(ctx context.Context, req Request, stdin io.Reader, stdout, stderr io.Writer) (Result, error) {
	start := time.Now()
	result := Result{ExitCode: -1, Files: map[string][]byte{}}
	if len(req.Command) == 0 {
		return result, errors.New("guest command is required")
	}
	for _, arg := range req.Command {
		if strings.IndexByte(arg, 0) >= 0 {
			return result, errors.New("NUL in guest command")
		}
	}
	if req.WorkDir == "" {
		req.WorkDir = "/"
	}
	if _, err := guestPath(req.WorkDir); err != nil {
		return result, fmt.Errorf("workdir: %w", err)
	}
	root := r.cfg.RootFS
	if !r.cfg.Persistent {
		var err error
		root, err = os.MkdirTemp("", "krunlet-root-*")
		if err != nil {
			return result, err
		}
		defer os.RemoveAll(root)
		if err = copyRootFS(ctx, r.cfg.RootFS, root); err != nil {
			return result, fmt.Errorf("stage rootfs: %w", err)
		}
	}
	for dest, content := range req.Files {
		p, err := checkedHostPath(root, dest)
		if err != nil {
			return result, fmt.Errorf("input %q: %w", dest, err)
		}
		if int64(len(content)) > r.cfg.MaxFileBytes {
			return result, fmt.Errorf("input %q exceeds max file bytes", dest)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			return result, err
		}
		// Recheck after creating parents, and disallow symlinks.
		if p, err = checkedHostPath(root, dest); err != nil {
			return result, err
		}
		if err = os.WriteFile(p, content, 0600); err != nil {
			return result, err
		}
	}
	env := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root", "LANG=C.UTF-8"}
	keys := make([]string, 0, len(req.Env))
	for k, v := range req.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.IndexByte(v, 0) >= 0 {
			return result, fmt.Errorf("invalid environment key/value: %q", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+req.Env[key])
	}
	timeout := r.cfg.Timeout
	if req.Timeout != 0 {
		timeout = req.Timeout
	}
	if timeout < 0 {
		return result, errors.New("negative timeout")
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	lease, err := prepareNetwork(callCtx, r.cfg.NetworkPolicy)
	if err != nil { return result, fmt.Errorf("gVisor network setup: %w", err) }
	defer lease.Close()
	// Only the helper writes to this private status file. It distinguishes
	// VMM startup failures from a guest process legitimately exiting 125.
	status, err := os.CreateTemp("", "krunlet-status-*.txt")
	if err != nil {
		return result, err
	}
	defer os.Remove(status.Name())
	if err = status.Close(); err != nil {
		return result, err
	}
	payload := krunffi.Config{ErrorPath: status.Name(), RootFS: root, WorkDir: req.WorkDir, Command: req.Command, Env: env,
		CPUs: r.cfg.CPUs, MemoryMiB: r.cfg.MemoryMiB, Network: r.cfg.Network,
		Ports: r.cfg.PortMaps, RLimits: r.cfg.RLimits, Library: r.cfg.LibraryPath,
		Kernel: ffiKernel(r.cfg.Kernel), RestrictedNetwork: r.cfg.NetworkPolicy != nil, NetSocket: lease.socket}
	configFile, err := os.CreateTemp("", "krunlet-config-*.json")
	if err != nil {
		return result, err
	}
	defer os.Remove(configFile.Name())
	if err = json.NewEncoder(configFile).Encode(payload); err != nil {
		configFile.Close()
		return result, err
	}
	if err = configFile.Close(); err != nil {
		return result, err
	}
	// The helper is the only process allowed to load libkrun. Its own exit
	// status is mapped onto the guest's exit status by krun_start_enter.
	helperArg := "__krunlet_self_helper"
	if !r.autoHelper {
		// Explicit HelperPath keeps compatibility with the standalone CLI.
		helperArg = "__helper"
	}
	cmd := exec.CommandContext(callCtx, r.cfg.HelperPath, helperArg, "--config", configFile.Name())
	if stdin == nil {
		stdin = strings.NewReader(req.Stdin)
	}
	cmd.Stdin = stdin
	// A blocked caller-provided reader must not indefinitely delay Wait on cancellation.
	cmd.WaitDelay = 2 * time.Second
	combined := &outputBudget{limit: r.cfg.MaxOutputBytes}
	out := &limitedWriter{budget: combined, mirror: stdout}
	errout := &limitedWriter{budget: combined, mirror: stderr}
	cmd.Stdout = out
	cmd.Stderr = errout
	err = cmd.Start()
	if err != nil {
		return result, fmt.Errorf("start helper: %w", err)
	}
	combined.setKill(func() { _ = cmd.Process.Kill() })
	waitErr := cmd.Wait()
	result.Duration = time.Since(start)
	result.Stdout = out.String()
	result.Stderr = errout.String()
	result.OutputLimited = combined.exceeded()
	result.TimedOut = errors.Is(callCtx.Err(), context.DeadlineExceeded)
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if waitErr != nil {
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) && callCtx.Err() == nil {
			return result, fmt.Errorf("wait helper: %w", waitErr)
		}
	}
	if result.OutputLimited {
		return result, errors.New("guest output limit exceeded")
	}
	if e := out.mirrorError(); e != nil {
		return result, fmt.Errorf("stdout stream: %w", e)
	}
	if e := errout.mirrorError(); e != nil {
		return result, fmt.Errorf("stderr stream: %w", e)
	}
	if statusErr, readErr := os.ReadFile(status.Name()); readErr == nil && len(statusErr) > 0 {
		return result, fmt.Errorf("libkrun helper failed: %s", strings.TrimSpace(string(statusErr)))
	}
	if result.TimedOut {
		return result, context.DeadlineExceeded
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	for _, p := range req.Collect {
		host, e := checkedHostPath(root, p)
		if e != nil {
			return result, fmt.Errorf("collect %q: %w", p, e)
		}
		f, e := os.Open(host)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return result, e
		}
		info, e := f.Stat()
		if e != nil {
			f.Close()
			return result, e
		}
		if !info.Mode().IsRegular() || info.Size() > r.cfg.MaxFileBytes {
			f.Close()
			return result, fmt.Errorf("invalid/oversized result file %q", p)
		}
		data, e := io.ReadAll(io.LimitReader(f, r.cfg.MaxFileBytes+1))
		f.Close()
		if e != nil {
			return result, e
		}
		if int64(len(data)) > r.cfg.MaxFileBytes {
			return result, fmt.Errorf("oversized result file %q", p)
		}
		result.Files[p] = data
	}
	return result, nil
}

func (r *Runner) Shell(ctx context.Context, script string) (Result, error) {
	return r.Run(ctx, Request{Command: []string{"/bin/sh", "-lc", script}})
}

// Doctor checks whether the native library is present. It cannot prove that
// the host has permission to start a VM; run an integration smoke test too.
func Doctor(lib string) error {
	if runtime.GOOS == "darwin" && runtime.GOARCH != "arm64" {
		return errors.New("libkrun requires Apple Silicon on macOS")
	}
	return krunffi.Available(lib)
}
