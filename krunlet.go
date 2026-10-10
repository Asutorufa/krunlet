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
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
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
	// Yuhaiin redirects all gVisor TCP/UDP flows into a local yuhaiin
	// krunlet inbound. This is not a SOCKS5 proxy and never falls back
	// to a direct host connection. Requires Network=true.
	Yuhaiin *YuhaiinConfig
	// Ephemeral copies the trusted rootfs into a private temp directory for
	// every invocation, preventing guest changes from persisting. Default true.
	// Setting false runs directly on RootFS and is not recommended for agents.
	Persistent bool
	// PortMaps is an allow-list of host:guest TCP ports, empty denies inbound.
	PortMaps []string
	// RLimits are Linux rlimit strings like "7=256:256" (RLIMIT_NOFILE).
	RLimits []string
	// MaxConcurrentVMs bounds concurrently executing helpers per Runner.
	// Zero defaults to max(1, min(4, runtime.NumCPU()/2)).
	MaxConcurrentVMs int
	// FailFast rejects a run with ErrTooManyVMs instead of queuing.
	FailFast bool
	// MaxRootFSBytes limits logical file bytes in a disposable rootfs.
	// Zero defaults to 1 GiB.
	MaxRootFSBytes int64
	// OnStats receives one snapshot for each completed or failed run.
	// It is invoked after resource cleanup without holding Runner locks.
	OnStats func(Stats)
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
	RunID         string            `json:"run_id,omitempty"`
	Stdout        string            `json:"stdout"`
	Stderr        string            `json:"stderr"`
	Duration      time.Duration     `json:"duration"`
	TimedOut      bool              `json:"timed_out"`
	OutputLimited bool              `json:"output_limited"`
	Files         map[string][]byte `json:"files,omitempty"`
}

type Runner struct {
	cfg         Options
	autoHelper  bool
	permits     chan struct{}
	cleanupOnce sync.Once
}

func New(opts Options) (*Runner, error) {
	if opts.RootFS == "" {
		return nil, errors.New("RootFS is required")
	}
	if !filepath.IsAbs(opts.RootFS) {
		return nil, errors.New("rootfs must be an absolute host path")
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
	if root == string(filepath.Separator) {
		return nil, errors.New("host root / cannot be a VM rootfs")
	}
	if home, e := os.UserHomeDir(); e == nil && home != "" {
		if canonical, e := filepath.EvalSymlinks(home); e == nil && root == canonical {
			return nil, errors.New("host home directory cannot be a VM rootfs")
		}
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
	if opts.MaxRootFSBytes == 0 {
		opts.MaxRootFSBytes = 1 << 30
	}
	if opts.MaxConcurrentVMs == 0 {
		opts.MaxConcurrentVMs = runtime.NumCPU() / 2
		if opts.MaxConcurrentVMs < 1 {
			opts.MaxConcurrentVMs = 1
		}
		if opts.MaxConcurrentVMs > 4 {
			opts.MaxConcurrentVMs = 4
		}
	}
	if opts.CPUs > 64 || opts.MemoryMiB < 128 || opts.MemoryMiB > 65536 {
		return nil, errors.New("CPU or memory limit out of range")
	}
	if opts.Timeout < 0 || opts.MaxOutputBytes < 0 || opts.MaxFileBytes < 0 || opts.MaxRootFSBytes < 0 || opts.MaxConcurrentVMs < 0 {
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
	if opts.Yuhaiin != nil {
		if !opts.Network {
			return nil, errors.New("yuhaiin requires Network=true")
		}
		if opts.Yuhaiin, err = normalizeYuhaiin(opts.Yuhaiin); err != nil {
			return nil, fmt.Errorf("yuhaiin inbound: %w", err)
		}
		// A non-nil policy selects virtio-net (disabling TSI).
		// Default no local CIDR restrictions; routing rules belong to yuhaiin.
		if opts.NetworkPolicy == nil {
			opts.NetworkPolicy = &NetworkPolicy{Mode: NetworkBlocklist}
		}
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
	return &Runner{cfg: opts, autoHelper: autoHelper, permits: make(chan struct{}, opts.MaxConcurrentVMs)}, nil
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

func (r *Runner) run(ctx context.Context, req Request, stdin io.Reader, stdout, stderr io.Writer) (result Result, retErr error) {
	start := time.Now()
	runID := newRunID()
	result = Result{ExitCode: -1, RunID: runID, Files: map[string][]byte{}}
	var helperState *os.ProcessState
	defer func() {
		if cb := r.cfg.OnStats; cb != nil {
			cb(Stats{RunID: runID, ReadyMillis: -1, DurationMillis: time.Since(start).Milliseconds(),
				ExitCode: result.ExitCode, PeakMemoryMiB: helperPeakMemory(helperState),
				NetworkPolicy: r.cfg.NetworkPolicy != nil, TimedOut: result.TimedOut})
		}
	}()
	slog.Debug("krunlet run admitted", "run_id", runID)
	timeout := r.cfg.Timeout
	if req.Timeout != 0 {
		timeout = req.Timeout
	}
	if timeout < 0 {
		return result, errors.New("negative timeout")
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := r.acquire(callCtx); err != nil {
		return result, err
	}
	defer r.release()
	r.cleanupOnce.Do(func() { _ = cleanupStaleRoots(os.TempDir(), 24*time.Hour) })

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
	var stateDir string
	if !r.cfg.Persistent {
		if err := checkRootFSSize(callCtx, root, r.cfg.MaxRootFSBytes); err != nil {
			return result, fmt.Errorf("rootfs preflight: %w", err)
		}
		var err error
		container, err := os.MkdirTemp("", "krunlet-root-*")
		if err != nil {
			return result, err
		}
		marker, err := markTempRoot(container)
		if err != nil {
			_ = os.RemoveAll(container)
			return result, err
		}
		defer func() { _ = marker.Close(); _ = os.RemoveAll(container) }()
		stateDir = container
		root = filepath.Join(container, "rootfs")
		if err := os.Mkdir(root, 0700); err != nil {
			return result, err
		}
		if err = copyRootFS(callCtx, r.cfg.RootFS, root); err != nil {
			return result, fmt.Errorf("stage rootfs: %w", err)
		}
	}
	if stateDir == "" {
		// Persistent mode still needs a signed scratch directory so helper
		// config/status files can be collected after the parent is SIGKILLed.
		dir, err := os.MkdirTemp("", "krunlet-state-*")
		if err != nil {
			return result, err
		}
		marker, err := markTempRoot(dir)
		if err != nil {
			_ = os.RemoveAll(dir)
			return result, err
		}
		defer func() { _ = marker.Close(); _ = os.RemoveAll(dir) }()
		stateDir = dir
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
	lease, err := prepareNetwork(callCtx, r.cfg.NetworkPolicy, r.cfg.Yuhaiin)
	if err != nil {
		return result, fmt.Errorf("gVisor network setup: %w", err)
	}
	defer lease.Close()
	// Only the helper writes to this private status file. It distinguishes
	// VMM startup failures from a guest process legitimately exiting 125.
	status, err := os.CreateTemp(stateDir, "status-*.txt")
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
		Kernel: ffiKernel(r.cfg.Kernel), RestrictedNetwork: r.cfg.NetworkPolicy != nil, NetSocket: lease.socket,
		RunID: runID}
	configFile, err := os.CreateTemp(stateDir, "config-*.json")
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
	supervisor := configureHelper(cmd)
	if stdin == nil {
		stdin = strings.NewReader(req.Stdin)
	}
	cmd.Stdin = stdin
	// A blocked caller-provided reader must not indefinitely delay Wait on cancellation.
	cmd.WaitDelay = 3 * time.Second
	combined := &outputBudget{limit: r.cfg.MaxOutputBytes}
	out := &limitedWriter{budget: combined, mirror: stdout}
	errout := &limitedWriter{budget: combined, mirror: stderr}
	cmd.Stdout = out
	cmd.Stderr = errout
	err = supervisor.start()
	if err != nil {
		return result, fmt.Errorf("start helper: %w", err)
	}
	combined.setKill(func() { signalHelperGroup(cmd, true) })
	waitErr := cmd.Wait()
	// Reap descendant processes even if the helper exited successfully.
	supervisor.finish()
	helperState = cmd.ProcessState
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
