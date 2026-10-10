package krunlet

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Asutorufa/krunlet/internal/krunffi"
	"github.com/Asutorufa/krunlet/internal/nativebundle"
)

// VM is a single running libkrun microVM, with serialized executions.
// Unlike Session, VM does NOT reboot between commands. A timed-out or cancelled
// command terminates the entire VM to guarantee there is no orphaned process.
// Commands require /bin/sh in the guest; stdout/stderr are exchanged via
// virtio-fs files and become available when each command completes.
// This protocol is intended for trusted infrastructure and is not a security
// boundary against a malicious guest able to write to its control directory.
type VM struct {
	mu                    sync.Mutex
	stopOnce              sync.Once
	stop                  context.CancelFunc
	cmd                   *exec.Cmd
	stdin                 io.WriteCloser
	stdout                *bufio.Reader
	stderr                *boundedBuffer
	session               *Session
	control               string // guest absolute path
	hostControl           string
	statusPath            string
	configPath            string
	networkCleanup        *networkLease
	next                  uint64
	closed                bool
	supervisor            *helperSupervisor
	quotaRunner           *Runner
	cgroup                *vmCgroup
	previousOOMKills      uint64
	previousPidsMaxEvents uint64
	readyMillis           int64
	nativeInfo nativebundle.Info
	waitDone              chan struct{}
	waitErr               error
}

const liveDriver = `printf 'KRUNLET_READY\n'
while IFS= read -r job; do
  case "$job" in
    /*) ;;
    *) exit 99 ;;
  esac
  /bin/sh "$job" </dev/null >/dev/null 2>/dev/null
  code=$?
  printf 'KRUNLET_DONE:%s:%s\n' "${job##*/}" "$code"
done`

var shellVariable = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// NewVM boots one VM and keeps it running until Close, the opening context
// is cancelled, or a Run call fails/cancels. Each Run has a separate timeout.
// A prepared guest rootfs with /bin/sh is required. Do not pass an untrusted
// rootfs: libkrun's virtio-fs requires separate HOST mount confinement.
func NewVM(ctx context.Context, opts Options) (_ *VM, err error) {
	sess, err := NewSession(ctx, opts)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = sess.Close()
		}
	}()
	if err = sess.runner.acquire(ctx); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			sess.runner.release()
		}
	}()
	folder, err := os.MkdirTemp(sess.root, ".krunlet-control-")
	if err != nil {
		return nil, err
	}
	rel := strings.TrimPrefix(folder, sess.root)
	if rel == "" || rel[0] != os.PathSeparator {
		return nil, errors.New("invalid VM control directory")
	}
	control := filepath.ToSlash(rel)
	status, err := os.CreateTemp(filepath.Dir(sess.root), "status-*.txt")
	if err != nil {
		return nil, err
	}
	statusPath := status.Name()
	_ = status.Close()
	defer func() {
		if err != nil {
			_ = os.Remove(statusPath)
		}
	}()
	config, err := os.CreateTemp(filepath.Dir(sess.root), "config-*.json")
	if err != nil {
		return nil, err
	}
	configPath := config.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(configPath)
		}
	}()
	cfg := sess.runner.cfg
	lease, err := prepareNetwork(ctx, cfg.NetworkPolicy, cfg.Yuhaiin)
	if err != nil {
		return nil, fmt.Errorf("gVisor network setup: %w", err)
	}
	defer func() {
		if err != nil {
			lease.Close()
		}
	}()
	nativeInfo, err := nativebundle.ResolveWithFallback(cfg.LibraryPath, cfg.AllowHostLibraryFallback)
	if err != nil {
		return nil, fmt.Errorf("prepare libkrun: %w", err)
	}
	payload := krunffi.Config{RootFS: sess.root, WorkDir: "/", Command: []string{"/bin/sh", "-c", liveDriver},
		Env:  []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root", "LANG=C.UTF-8"},
		CPUs: cfg.CPUs, MemoryMiB: cfg.MemoryMiB, Network: cfg.Network, Ports: cfg.PortMaps, RLimits: cfg.RLimits,
		Library: nativeInfo.Library, Firmware: nativeInfo.Firmware, ErrorPath: statusPath, Kernel: ffiKernel(cfg.Kernel), RestrictedNetwork: cfg.NetworkPolicy != nil, NetSocket: lease.socket}
	if err = json.NewEncoder(config).Encode(payload); err != nil {
		_ = config.Close()
		return nil, err
	}
	if err = config.Close(); err != nil {
		return nil, err
	}
	helperArg := "__krunlet_self_helper"
	if !sess.runner.autoHelper {
		helperArg = "__helper"
	}
	vmCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(vmCtx, cfg.HelperPath, helperArg, "--config", configPath)
	supervisor := configureHelper(cmd)
	cg, err := prepareVMCgroup(cfg)
	if err != nil {
		cancel()
		return nil, err
	}
	defer func() {
		if err != nil && cg != nil {
			_ = cg.Close()
		}
	}()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		_ = stdin.Close()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		_ = stdin.Close()
		return nil, err
	}
	v := &VM{stop: cancel, cmd: cmd, stdin: stdin, stdout: bufio.NewReaderSize(stdout, 8192),
		networkCleanup: lease,
		stderr:         &boundedBuffer{limit: 65536}, session: sess, control: control, hostControl: folder, statusPath: statusPath, configPath: configPath,
		supervisor: supervisor, quotaRunner: sess.runner, cgroup: cg, waitDone: make(chan struct{}), nativeInfo: nativeInfo}
	startBoot := time.Now()
	if err = startHelperInCgroup(supervisor, cg); err != nil {
		cancel()
		_ = stdin.Close()
		return nil, fmt.Errorf("start VM helper: %w", err)
	}
	go func() { _, _ = io.Copy(v.stderr, stderr) }()
	// One goroutine owns Wait. This also detects helpers that crash or exit
	// without a caller issuing Close; Close releases the gateway and quota.
	go func() {
		v.waitErr = cmd.Wait()
		supervisor.finish()
		close(v.waitDone)
	}()
	startupCtx, done := context.WithTimeout(ctx, cfg.Timeout)
	defer done()
	if err = v.await(startupCtx, "KRUNLET_READY"); err != nil {
		v.terminate()
		<-v.waitDone
		if b, e := os.ReadFile(statusPath); e == nil && len(b) > 0 {
			return nil, fmt.Errorf("libkrun helper: %s", strings.TrimSpace(string(b)))
		}
		return nil, fmt.Errorf("boot VM: %w; helper stderr: %s", err, v.stderr.String())
	}
	v.readyMillis = time.Since(startBoot).Milliseconds()
	if cg != nil {
		initial, e := cg.usage()
		if e != nil {
			v.terminate()
			<-v.waitDone
			return nil, fmt.Errorf("read initial VM cgroup events: %w", e)
		}
		v.previousOOMKills = initial.OOMKills
		v.previousPidsMaxEvents = initial.PidsMaxEvents
	}
	// Closing the owning context also reclaims the rootfs, gateway socket
	// and VM permit even if the caller forgets an explicit Close.
	go func() {
		select {
		case <-vmCtx.Done():
		case <-v.waitDone:
		}
		_ = v.Close()
	}()
	return v, nil
}

func (v *VM) terminate() {
	v.stopOnce.Do(func() {
		v.stop()
		v.supervisor.terminate()
	})
}

// await is called only under v.mu, except during construction.
// The pipe reader is interrupted by terminating the helper if ctx expires.
func (v *VM) await(ctx context.Context, target string) error {
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 64; i++ {
			line, err := v.stdout.ReadString('\n')
			if err != nil {
				done <- err
				return
			}
			if len(line) > 8192 {
				done <- errors.New("oversized VM control response")
				return
			}
			if strings.TrimRight(line, "\r\n") == target {
				done <- nil
				return
			}
		}
		done <- errors.New("no matching VM control response")
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		v.terminate()
		<-done
		return ctx.Err()
	}
}

// Run executes in the already-booted VM. Commands are serialized. Unlike
// Runner.Run, stdout/stderr are buffered in guest files until exit.
func (v *VM) Run(ctx context.Context, req Request) (res Result, runErr error) {
	start := time.Now()
	runID := newRunID()
	v.mu.Lock()
	defer func() {
		var callback func(Stats)
		var policyEnabled bool
		if v.session != nil {
			callback = v.session.runner.cfg.OnStats
			policyEnabled = v.session.runner.cfg.NetworkPolicy != nil
		}
		if v.cgroup != nil {
			if usage, err := v.cgroup.usage(); err == nil {
				res.MemoryPeakBytes = usage.MemoryPeakBytes
				// memory.events and pids.events are cumulative for the lifetime
				// of a persistent VM. Only NEW events belong to this command.
				newOOM := usage.OOMKills > v.previousOOMKills
				newPids := usage.PidsMaxEvents > v.previousPidsMaxEvents
				v.previousOOMKills = usage.OOMKills
				v.previousPidsMaxEvents = usage.PidsMaxEvents
				if newOOM {
					res.TerminationReason = "oom"
					runErr = errors.Join(runErr, ErrOOM)
				} else if newPids {
					res.TerminationReason = "pids_limit"
					runErr = errors.Join(runErr, ErrPidsLimit)
				}
			} else {
				runErr = errors.Join(runErr, fmt.Errorf("read cgroup usage: %w", err))
			}
		}
		v.mu.Unlock()
		if callback != nil {
			callback(Stats{RunID: runID, ReadyMillis: v.readyMillis, DurationMillis: time.Since(start).Milliseconds(),
				ExitCode: res.ExitCode, HelperRSSBytes: 0, MemoryPeakBytes: res.MemoryPeakBytes,
				Containment: res.Containment, TerminationReason: res.TerminationReason,
				NetworkPolicy: policyEnabled, TimedOut: res.TimedOut})
		}
	}()
	res = Result{ExitCode: -1, RunID: runID, Containment: defaultContainment(), Files: map[string][]byte{}}
	res.Native = v.nativeInfo
	if v.cgroup != nil {
		res.Containment = "cgroup_v2"
	}
	if v.closed {
		return res, errors.New("VM is closed or has failed")
	}
	if len(req.Command) == 0 {
		return res, errors.New("missing command")
	}
	for _, x := range req.Command {
		if strings.ContainsRune(x, 0) {
			return res, errors.New("NUL byte in command")
		}
	}
	if req.WorkDir == "" {
		req.WorkDir = "/"
	}
	if _, err := guestPath(req.WorkDir); err != nil {
		return res, err
	}
	timeout := v.session.runner.cfg.Timeout
	if req.Timeout != 0 {
		timeout = req.Timeout
	}
	if timeout <= 0 {
		return res, errors.New("timeout must be positive")
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for name, b := range req.Files {
		if err := v.session.WriteFile(name, b); err != nil {
			return res, err
		}
	}
	v.next++
	id := fmt.Sprintf("job-%016x", v.next)
	guestBase := path.Join(v.control, id)
	hostBase := filepath.Join(v.hostControl, id)
	defer func() {
		for _, p := range []string{".sh", ".stdin", ".stdout", ".stderr"} {
			_ = os.Remove(hostBase + p)
		}
	}()
	if int64(len(req.Stdin)) > v.session.runner.cfg.MaxFileBytes {
		return res, errors.New("stdin exceeds file limit")
	}
	if err := os.WriteFile(hostBase+".stdin", []byte(req.Stdin), 0600); err != nil {
		return res, err
	}
	script, err := makeLiveScript(req, guestBase)
	if err != nil {
		return res, err
	}
	if err = os.WriteFile(hostBase+".sh", []byte(script), 0600); err != nil {
		return res, err
	}
	if err = runCtx.Err(); err != nil {
		v.closed = true
		v.terminate()
		return res, err
	}
	if _, err = io.WriteString(v.stdin, guestBase+".sh\n"); err != nil {
		v.closed = true
		v.terminate()
		return res, fmt.Errorf("send VM command: %w", err)
	}
	// A shell-owned control frame never includes user stdout/stderr bytes.
	targetPrefix := "KRUNLET_DONE:" + id + ".sh:"
	line, err := v.readCompletion(runCtx, targetPrefix)
	if err != nil {
		v.closed = true
		v.terminate()
		res.TimedOut = errors.Is(err, context.DeadlineExceeded)
		return res, err
	}
	code, err := strconv.Atoi(strings.TrimPrefix(line, targetPrefix))
	if err != nil || code < 0 || code > 255 {
		v.closed = true
		v.terminate()
		return res, errors.New("invalid guest exit status")
	}
	res.ExitCode = code
	res.Duration = time.Since(start)
	limit := v.session.runner.cfg.MaxOutputBytes
	for i, name := range []string{hostBase + ".stdout", hostBase + ".stderr"} {
		b, e := readOutputFile(name, limit)
		if e != nil {
			if errors.Is(e, errOutputTooLarge) {
				res.OutputLimited = true
			}
			return res, e
		}
		limit -= int64(len(b))
		if i == 0 {
			res.Stdout = string(b)
		} else {
			res.Stderr = string(b)
		}
	}
	for _, p := range req.Collect {
		data, e := v.session.ReadFile(p)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return res, e
		}
		res.Files[p] = data
	}
	return res, nil
}

func (v *VM) readCompletion(ctx context.Context, prefix string) (string, error) {
	done := make(chan struct {
		line string
		err  error
	}, 1)
	go func() {
		for i := 0; i < 64; i++ {
			line, e := v.stdout.ReadString('\n')
			if e != nil {
				done <- struct {
					line string
					err  error
				}{"", e}
				return
			}
			if len(line) > 8192 {
				done <- struct {
					line string
					err  error
				}{"", errors.New("control line too long")}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, prefix) {
				done <- struct {
					line string
					err  error
				}{line, nil}
				return
			}
		}
		done <- struct {
			line string
			err  error
		}{"", errors.New("no VM completion frame")}
	}()
	select {
	case r := <-done:
		return r.line, r.err
	case <-ctx.Done():
		v.terminate()
		<-done
		return "", ctx.Err()
	}
}

func makeLiveScript(req Request, base string) (string, error) {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("exec < " + shellQuote(base+".stdin") + " > " + shellQuote(base+".stdout") + " 2> " + shellQuote(base+".stderr") + "\n")
	b.WriteString("cd " + shellQuote(req.WorkDir) + " || exit 111\n")
	keys := make([]string, 0, len(req.Env))
	for key, val := range req.Env {
		if !shellVariable.MatchString(key) || strings.ContainsRune(val, 0) {
			return "", fmt.Errorf("invalid live VM environment variable %q", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		b.WriteString("export " + key + "=" + shellQuote(req.Env[key]) + "\n")
	}
	for i, arg := range req.Command {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(shellQuote(arg))
	}
	b.WriteString("\n")
	return b.String(), nil
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

var errOutputTooLarge = errors.New("VM output exceeds output limit")

func readOutputFile(name string, limit int64) ([]byte, error) {
	fd, e := syscall.Open(name, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("VM output is not a regular file")
	}
	if info.Size() > limit {
		return nil, errOutputTooLarge
	}
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) > limit {
		return nil, errOutputTooLarge
	}
	return b, nil
}

// Shell is a shorthand for a shell command in this persistent VM.
func (v *VM) Shell(ctx context.Context, script string) (Result, error) {
	return v.Run(ctx, Request{Command: []string{"/bin/sh", "-c", script}})
}

// Close stops the VM (also interrupting a currently executing command),
// waits for the helper and deletes the private filesystem.
func (v *VM) Close() error {
	v.terminate()
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed && v.session == nil {
		return nil
	}
	v.closed = true
	_ = v.stdin.Close()
	<-v.waitDone
	var groupErr error
	if v.cgroup != nil {
		groupErr = v.cgroup.Close()
		v.cgroup = nil
	}
	if v.networkCleanup != nil {
		v.networkCleanup.Close()
		v.networkCleanup = nil
	}
	_ = os.Remove(v.statusPath)
	_ = os.Remove(v.configPath)
	err := v.session.Close()
	v.session = nil
	if v.quotaRunner != nil {
		v.quotaRunner.release()
		v.quotaRunner = nil
	}
	if err != nil || groupErr != nil {
		return errors.Join(err, groupErr)
	}
	// Signal-terminated VMs are the expected close path.
	return nil
}

type boundedBuffer struct {
	mu    sync.Mutex
	buf   strings.Builder
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if b.buf.Len() < b.limit {
		k := n
		if k > b.limit-b.buf.Len() {
			k = b.limit - b.buf.Len()
		}
		_, _ = b.buf.Write(p[:k])
	}
	return n, nil
}
func (b *boundedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.String() }
