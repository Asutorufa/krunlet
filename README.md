# Krunlet

**Small microVM sandbox runner for Go LLM agents.** Uses [libkrun 1.19.x](https://github.com/libkrun/libkrun) via [purego](https://github.com/ebitengine/purego), without cgo. Provides a Go API and a CLI, disposable filesystem copies, timeouts, CPU/RAM configuration, bounded output, file exchange, and opt-in networking.

> **Experimental, not production security-hardened.** libkrun's VMM and virtio-fs operate with host-side privileges. Do not give an untrusted guest access to sensitive host files. A production deployment additionally needs OS-level confinement (Linux mount/user/network namespaces and cgroups, or equivalent), especially to contain virtio-fs filesystem traversal, host resources, and disk exhaustion. Our per-run rootfs copy is **not** a replacement for host confinement. No network/virtio-fs security claim has been validated by an end-to-end VM test yet.

## Requirements

- **Linux x86-64 / ARM64** with `/dev/kvm`, or **macOS Apple Silicon** (HVF). macOS requires the appropriate Hypervisor entitlement when applicable.
- **libkrun v1.19.x** and its `libkrunfw` runtime dependencies; **NOT libkrun 2.x `main`**.
- Trusted, prepared **Linux rootfs directory**, e.g. from an image you built and verified. It must contain the invoked binaries and libraries.
- Go 1.23+ to build from source. No C compiler or cgo required to build the Go binary.

## Install

```sh
CGO_ENABLED=0 go install github.com/Asutorufa/krunlet/cmd/krunlet@latest
krunlet doctor
```

To build the CLI locally:

```sh
CGO_ENABLED=0 go build -o krunlet ./cmd/krunlet
./krunlet doctor
```

## Run

```sh
krunlet run --rootfs ./rootfs --timeout 10s -- /bin/sh -lc 'echo hello'
krunlet run --rootfs ./rootfs --cpus 2 --memory 512 --json -- /usr/bin/python3 -V
krunlet run --rootfs ./rootfs --network -- /bin/sh -lc 'curl https://example.org'
```

No network is the default. `--network` enables TSI through the **host network**, not a separate routed namespace. The `--persistent` switch disables ephemeral filesystem cloning and writes **directly to the supplied rootfs**, so do not use it for untrusted work.

The `run`/`exec` CLI executes one command in one new VM. VM startup costs occur per call; there is no persistent REPL or interactive agent session yet.

## Go API

```go
s, err := krunlet.New(krunlet.Options{
    RootFS: "/path/to/trusted/rootfs",
    CPUs: 2, MemoryMiB: 512,
    Timeout: 10 * time.Second,
    Network: false,
})
if err != nil { panic(err) }

result, err := s.Run(ctx, krunlet.Request{
    Command: []string{"/bin/sh", "-lc", "cat /workspace/in.txt > /workspace/out.txt"},
    Files: map[string][]byte{"/workspace/in.txt": []byte("hello")},
    Collect: []string{"/workspace/out.txt"},
})
if err != nil { panic(err) }
fmt.Println(result.ExitCode, string(result.Files["/workspace/out.txt"]))
```

No standalone CLI installation is necessary for Go imports. Krunlet re-executes the current Go binary as a private helper. Set `Options.HelperPath` to use an explicitly installed `krunlet` CLI instead.

`Runner.Shell(ctx, script)` is shorthand for `Run(ctx, {Command: ["/bin/sh", "-lc", script]})`.

**Important:** A Go library cannot safely call `krun_start_enter()` from the calling process, because libkrun terminates that process when the VM exits. The library launches the `krunlet __helper` subprocess. The library automatically re-executes the importing application, with optional `Options.HelperPath` override. The public API is thread-safe for independent runs (each run uses its own subprocess and private temporary rootfs).

### Supported options

| Feature | API | Notes |
| --- | --- | --- |
| VM isolation | libkrun KVM/HVF | One VM per Run |
| CPU and memory | `CPUs`, `MemoryMiB` | Configured at VM startup |
| Timeout/cancellation | `Timeout`, context | Kills helper on expiry |
| stdout/stderr | `Result.Stdout`, `.Stderr` | Shared total output cap; kills helper if exceeded |
| Exit codes | `Result.ExitCode` | Nonzero is normal result, not a Go error |
| File input/output | `Files`, `Collect` | Strict absolute guest paths; symlink paths rejected |
| Disposable changes | default | Full copy of rootfs per execution |
| Reusable rootfs | `Persistent` | Explicit unsafe opt-in |
| Environment | `Request.Env` | Explicit allowlist, no full host env inheritance |
| Standard input | `Request.Stdin` | Provided as a string |
| Inbound ports | `PortMaps` | Host:guest, no implicit wildcard forwarding |
| Guest rlimits | `RLimits` | Numeric Linux resource IDs, e.g. `7=256:256` |
| Networking | `Network` | Disabled by default via no-TSI vsock; enabling allows host-mediated egress |
| Native library override | `LibraryPath` | Defaults to libkrun.so.1 / libkrun.dylib |

## Isolation and limitations

- `libkrun` defaults to TSI network when no conventional NIC is attached. Krunlet explicitly requests a vsock without TSI features when network is disabled, and sets an empty inbound port map. This needs runtime verification against your exact libkrun build before it can be relied upon as a security boundary.
- libkrun 1.x virtio-fs is **not** a complete host filesystem sandbox. The helper must be confined by host OS policies to mitigate filesystem traversal and other host access. Do not run it as root.
- Rootfs copies consume disk space proportional to the rootfs and do not yet enforce disk quotas. Refuse untrusted oversized base images; use OS quotas/cgroups in production.
- No container image pulling, persistent multi-command VM, streaming protocol, DNS/domain allowlist, snapshot/overlay storage, structured per-syscall network policy, or remote execution in v0.1.0.
- `Doctor` checks dynamic ABI symbol presence and `/dev/kvm`, not guest boot success.

## Development

```sh
go test ./...
go vet ./...
CGO_ENABLED=0 go test ./...
```

For a real virtualization smoke test, install libkrun 1.19.x, prepare a rootfs, and run:

```sh
krunlet run --rootfs ./rootfs -- /bin/sh -c 'echo guest-ok'
```

## License

MIT.

## Stateful agent workflow (across VM restarts)

Use `NewSession` to copy a rootfs **once**, keep changed files for later commands, and remove the sandbox with `Close`:

```go
session, err := krunlet.NewSession(ctx, krunlet.Options{
    RootFS: "/trusted/rootfs",
    Network: false,
})
if err != nil { panic(err) }
defer session.Close()

if err := session.WriteFile("/workspace/hello.txt", []byte("hello")); err != nil { panic(err) }
result, err := session.Shell(ctx, "cp /workspace/hello.txt /workspace/copy.txt")
if err != nil { panic(err) }
if result.ExitCode != 0 { panic(result.Stderr) }
resultFile, err := session.ReadFile("/workspace/copy.txt")
fmt.Println(string(resultFile), err)
```

`Session` also provides `ListDir`, `RemoveFile`, and `RunBatch` (sequential, stop on first nonzero exit code). **This is filesystem persistence, not an always-running VM.**

### Advanced CLI flags

```sh
# Pass a file, allow specific inbound port, and collect a result as JSON
krunlet run --rootfs ./rootfs --network --port 18080:8080 \
  --env CI=true --rlimit 7=256:256 \
  --file /workspace/input.txt=./input.txt \
  --collect /workspace/result.txt --json \
  -- /bin/sh -c 'cp /workspace/input.txt /workspace/result.txt'

# Supply program input from a file or a pipe
printf 'hello' | krunlet run --rootfs ./rootfs --stdin-file - -- /bin/cat
```

The JSON `files` map uses Go's `[]byte` JSON encoding (base64), and the helper captures stdout/stderr separately up to `--output-limit` total bytes.
