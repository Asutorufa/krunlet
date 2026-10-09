# Krunlet

**Small microVM sandbox runner for Go LLM agents.** Uses [libkrun 1.19.x](https://github.com/libkrun/libkrun) via [purego](https://github.com/ebitengine/purego), without cgo. Provides a Go API and a CLI, disposable filesystem copies, timeouts, CPU/RAM configuration, bounded output, file exchange, and opt-in networking.

> **Experimental, not production security-hardened.** libkrun's VMM and virtio-fs operate with host-side privileges. Do not give an untrusted guest access to sensitive host files. A production deployment additionally needs OS-level confinement (Linux mount/user/network namespaces and cgroups, or equivalent), especially to contain virtio-fs filesystem traversal, host resources, and disk exhaustion. Our per-run rootfs copy is **not** a replacement for host confinement. No network/virtio-fs security claim has been validated by an end-to-end VM test yet.

## Requirements

- **Linux x86-64 / ARM64** with `/dev/kvm`, or **macOS Apple Silicon** (HVF). macOS requires the appropriate Hypervisor entitlement when applicable.
- **libkrun v1.19.x** (**NOT** the incompatible libkrun 2.x `main` API). The default kernel needs `libkrunfw`; an external kernel supplied through `KernelConfig` can boot without it.
- Trusted, prepared **Linux rootfs directory**, e.g. from an image you built and verified. It must contain the invoked binaries and libraries.
- Go 1.23+ to build from source. No C compiler or cgo required to build the Go binary.

## Installing the native `libkrun` libraries

**You do not put `libkrun.so` inside your Go module or inside the guest rootfs.**
The `krunlet` helper runs on the **host** and calls `purego.Dlopen` at runtime.
The host needs a native libkrun **1.19.x** shared library, its dependencies,
and, for the default kernel, `libkrunfw` (the guest kernel payload).
Compiling Krunlet with `CGO_ENABLED=0` only removes the Go/C build dependency;
it does **not** bundle these native libraries or the firmware.

### macOS (Apple Silicon, macOS 14+)

Homebrew provides the libraries. The community tap used by libkrun is available
at [libkrun/homebrew-krun](https://github.com/libkrun/homebrew-krun):

```sh
brew tap libkrun/krun
# With Homebrew versions requiring third-party tap trust:
brew trust libkrun/krun
brew install libkrun libkrunfw

# The exact location is shown by Homebrew, typically under /opt/homebrew/opt/:
ls -l "$(brew --prefix libkrun)/lib/"*krun*.dylib
krunlet doctor --lib "$(brew --prefix libkrun)/lib/libkrun.dylib"
```

When using this path from Go, set
`Options.LibraryPath = "/opt/homebrew/opt/libkrun/lib/libkrun.dylib"`
(adjust to your `brew --prefix libkrun`). Otherwise Krunlet attempts
`Dlopen("libkrun.dylib")`, which must be discoverable by the system dynamic
loader. `DYLD_LIBRARY_PATH` can affect loading on non-protected executables,
but an explicit absolute `LibraryPath` is recommended.

For macOS virtualization, the **actual signed executable** (CLI *or your Go
application importing Krunlet*) may need the Hypervisor entitlement. An
example is in [`docs/krunlet.entitlements`](docs/krunlet.entitlements):

```sh
codesign --force --sign - --entitlements docs/krunlet.entitlements ./krunlet
```

### Fedora Linux (x86-64 / aarch64)

Use distribution packages if available, or enable the libkrun COPRs:

```sh
sudo dnf copr enable -y slp/libkrunfw
sudo dnf copr enable -y slp/libkrun
sudo dnf install -y libkrun libkrunfw
ls -l /usr/lib64/libkrun.so*
krunlet doctor
ls -l /dev/kvm
```

Ensure the account running the helper can access `/dev/kvm` (often via the
`kvm` group). If libkrun is installed in a custom location, inspect it using
`ldconfig -p | grep libkrun` and pass the **full path to the versioned**
`libkrun.so.1` via `--lib` or `Options.LibraryPath`.

### Other Linux distributions / building from source

Check whether your distribution provides compatible `libkrun` and `libkrunfw`
packages. Otherwise follow the upstream installation guides:

1. [libkrunfw](https://github.com/libkrun/libkrunfw) for the guest kernel.
2. [libkrun v1.19.6](https://github.com/libkrun/libkrun/tree/v1.19.6) for the shared library (use the versioned tag, **not** the 2.x development API).

For a source build after installing the documented toolchain and firmware:

```sh
git clone --branch v1.19.6 --depth 1 https://github.com/libkrun/libkrun.git
cd libkrun
make
sudo make install
sudo ldconfig  # Linux, when using a standard library path
```

The upstream GitHub release page provides source releases, **not** a universal
prebuilt `libkrun.so` asset. Usual Linux paths are `/usr/lib64/`, `/usr/lib/`,
`/usr/local/lib/` and `/usr/local/lib64/`; the path depends on your package
manager. If using a custom directory, configure the **host** dynamic loader
(e.g. `LD_LIBRARY_PATH=/path/to/libdir` or `ldconfig`) so dependent shared
libraries such as `libkrunfw` can also be located. Passing `LibraryPath`
changes only which libkrun binary `purego` opens.

### Verify

```sh
krunlet doctor                          # resolves the system default library
krunlet doctor --lib /custom/libkrun.so.1
krunlet run --rootfs /path/to/rootfs -- /bin/sh -c 'echo guest-ok'
```

`doctor` is a dynamic loader/ABI check. A successful result is **not** proof
that an actual VM boots or that host isolation is correctly configured.

## Custom Linux kernels

Krunlet defaults to the kernel shipped by `libkrunfw`. To boot your own
kernel, set `Options.Kernel` (library) or `--kernel` (CLI). This calls
`krun_set_kernel` in **libkrun 1.19.x** with a host-side kernel image,
optional host-side initramfs, and optional command line. Nothing needs
to be copied into the guest rootfs for these paths.

**With an external kernel, the 1.19.x VM startup path skips the default
`libkrunfw` kernel payload.** You still need `libkrun`, a compatible
kernel, and the trusted guest rootfs. The default mode still requires
`libkrunfw`. See the
[libkrun header](https://github.com/libkrun/libkrun/blob/v1.19.6/include/libkrun.h)
and [firmware sources](https://github.com/libkrun/libkrunfw).

### Go library

```go
runner, err := krunlet.New(krunlet.Options{
    RootFS: "/path/to/rootfs",
    Kernel: &krunlet.KernelConfig{
        Path:    "/host/kernels/Image",
        Format:  krunlet.KernelFormatRaw,
        Initrd:  "/host/kernels/initramfs.cpio.gz", // optional
        Cmdline: "console=hvc0",                   // optional
    },
})
if err != nil { panic(err) }
result, err := runner.Shell(ctx, "uname -a")
```

The same `Options.Kernel` is supported by `NewSession` and `NewVM`.
`New` verifies that the kernel and optional initramfs are readable regular
host files, resolves them to absolute paths, and rejects unsupported formats.
The file must remain accessible to the helper until VM startup. Paths and
kernel command lines are **trusted host configuration**, never LLM-supplied.

### CLI

```sh
# Example: ARM64 raw Image (adapt to your architecture)
krunlet run --rootfs ./rootfs \
  --kernel /host/kernels/Image --kernel-format raw \
  --initramfs /host/kernels/initramfs.cpio.gz \
  --kernel-cmdline "console=hvc0" -- /bin/uname -a

# Omit --initramfs and --kernel-cmdline to let libkrun use its defaults.
krunlet run --rootfs ./rootfs --kernel /host/kernels/vmlinux \
  --kernel-format elf -- /bin/uname -a
```

Kernel formats: `raw` (0), `elf` (1), `pe-gz` (2),
`image-bz2` (3), `image-gz` (4), `image-zstd` (5).
`--kernel-format` defaults to `raw` when `--kernel` is set.
`--initramfs`, `--kernel-cmdline`, and `--kernel-format` require `--kernel`.

**x86-64 caveat:** libkrun 1.19.x handles `raw` kernels through
`map_kernel()`, which ignores the separately supplied initramfs and
cmdline. Krunlet rejects that combination to avoid silent misconfiguration.
Choose a supported non-raw format if you need those options on x86-64.

A generic distribution kernel is **not guaranteed to boot**: it must have
the libkrun guest drivers and compatible init/virtio-fs behavior. TSI
networking depends on libkrun-specific kernel support. Start from
[libkrunfw's kernel config and patches](https://github.com/libkrun/libkrunfw)
when possible. `doctor` only validates the native lib and host basics;
run a real guest smoke test to validate your custom image.

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

The `run`/`exec` CLI executes one command in one new VM. VM startup costs occur per call; the Go library also offers `NewVM` for one running VM serving multiple commands.

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
| VM isolation | libkrun KVM/HVF | One VM per `Runner.Run`; reuse via `NewVM` |
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
| Restricted egress | `NetworkPolicy` | gVisor user-space virtio-net gateway, IPv4 TCP/UDP rules, Linux/macOS |
| Native library override | `LibraryPath` | Defaults to libkrun.so.1 / libkrun.dylib |
| Custom guest kernel | `Kernel *KernelConfig` | Host path, format, optional initramfs and cmdline; nil uses libkrunfw |


## Restricted outbound networking with gVisor (Linux and macOS)

By default Krunlet disables networking. If `Network=true` without a
`NetworkPolicy`, libkrun uses unrestricted host-mediated TSI networking
(**not suitable for untrusted agents**).

With `NetworkPolicy`, Krunlet instead creates an isolated **gVisor Netstack
TCP/UDP gateway** inside the parent Go process and attaches the guest using
a QEMU-framed Unix stream and libkrun's `krun_add_net_unixstream` API.
Each allowed destination is checked **before** Netstack's TCP/UDP forwarder
opens any host socket. Unlisted traffic is dropped in allowlist mode; explicit
block rules take precedence over allow rules. Restricted mode disables
libkrun TSI, including its host AF_UNIX socket impersonation feature.

**Extra native requirement:** libkrun 1.19.x must be built with `NET=1`.
Normal libkrun builds do not necessarily contain `krun_add_net_unixstream`.
Krunlet fails closed if that symbol is missing:

```sh
git clone --depth 1 --branch v1.19.6 https://github.com/libkrun/libkrun.git
cd libkrun
make NET=1
sudo make install
```

Go usage:

```go
runner, err := krunlet.New(krunlet.Options{
    RootFS: "/trusted/rootfs",
    Network: true,
    NetworkPolicy: &krunlet.NetworkPolicy{
        Mode: krunlet.NetworkAllowlist,
        BlockPrivateNetworks: true,
        Allow: []krunlet.NetworkRule{
            {CIDR: "1.1.1.1/32", Port: 443, Protocol: "tcp"},
        },
        Block: []krunlet.NetworkRule{
            {CIDR: "1.1.1.2/32"},
        },
    },
})
```

CLI equivalent:

```sh
krunlet run --rootfs ./rootfs --network \
  --net-mode allowlist --block-private \
  --allow-cidr 1.1.1.1,443,tcp --block-cidr 1.1.1.2 \
  -- /bin/sh -c 'echo policy-enabled'
```

Rules accept `IP_OR_CIDR[,PORT[,tcp|udp]]` with port and protocol optional.
Network policies work with one-shot `Runner`, reusable-rootfs `Session`,
and long-running `NewVM`. `PortMaps` cannot be combined with strict mode.
No `nftables`, netns, or root permissions are needed for the gateway itself.

**Current restrictions:**

- The gateway handles **IPv4 TCP/UDP** and DHCP. IPv6 rules are rejected
  at network startup rather than silently ignored. Arbitrary IP protocols
  are not forwarded.
- **DNS forwarding is deliberately disabled** pending an enforceable DNS
  policy. Use literal IP endpoints or trusted application-level resolution
  outside the guest. Permitting port 53 to a public resolver does not itself
  enable DNS inside this guest; the gateway is not a generic NAT router.
- The guest must include virtio-net support; libkrun's built-in guest DHCP
  client is requested. Custom kernels must provide the required drivers.
- Local host and guest files still require host OS confinement. gVisor
  controls the intended TCP/UDP forwarding path; this is **not** a claim
  of complete security isolation of libkrun or virtio-fs.
- This backend depends on `github.com/containers/gvisor-tap-vsock`
  **v0.8.0** (Go 1.22 compatible), using its TAP and DHCP components with
  Krunlet-controlled outbound forwarders. Do not replace it with unmodified
  gvproxy, whose default forwarders directly call host `net.Dial`.
- Unit tests cover rule validation and gateway lifecycle. **Actual guest
  boot and egress policy bypass tests require a libkrun NET=1 host**.

## Isolation and limitations

- `libkrun` defaults to TSI network when no conventional NIC is attached. Krunlet explicitly requests a vsock without TSI features when network is disabled, and sets an empty inbound port map. This needs runtime verification against your exact libkrun build before it can be relied upon as a security boundary.
- libkrun 1.x virtio-fs is **not** a complete host filesystem sandbox. The helper must be confined by host OS policies to mitigate filesystem traversal and other host access. Do not run it as root.
- Rootfs copies consume disk space proportional to the rootfs and do not yet enforce disk quotas. Refuse untrusted oversized base images; use OS quotas/cgroups in production.
- No container image pulling, per-host disk quotas, DNS/domain allowlist, snapshot/overlay storage, structured per-syscall network policy, or remote execution yet.
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

## Running multiple commands in one VM (Go library)

`Session` shares a filesystem but restarts a fresh VM per command.
For agents that need an actual **running** VM (no reboot between calls), use
`NewVM`. This requires `/bin/sh` in the guest image:

```go
vm, err := krunlet.NewVM(ctx, krunlet.Options{
    RootFS: "/path/to/rootfs", Timeout: 20*time.Second,
    Network: false,
})
if err != nil { return err }
defer vm.Close()

first, err := vm.Run(ctx, krunlet.Request{
    Command: []string{"/bin/sh", "-c", "echo one > /tmp/state"},
})
if err != nil { return err }
if first.ExitCode != 0 { return fmt.Errorf("first command exited %d", first.ExitCode) }

second, err := vm.Shell(ctx, "cat /tmp/state")
if err != nil { return err }
fmt.Println(second.Stdout) // one
```

The library uses a small POSIX shell command loop in the guest, and keeps its
control and per-command stdout/stderr files in the **private rootfs clone**.
Commands run **sequentially**; `VM.Run` stdout/stderr are available **after**
the command exits. Cancelling a command or exceeding its timeout destroys
the VM, preventing a contaminated VM from being reused. `Close` also aborts
an in-progress command. Control files are not a security barrier against a
malicious guest, and the shared directory requires additional host confinement.
Do not pass secrets in `Request.Env` unless the guest is allowed to read them.

### Stream output from one-shot VM executions

`Runner.RunStream` sends output to Go `io.Writer` interfaces as it arrives,
while still collecting bounded strings in `Result`:

```go
result, err := runner.RunStream(ctx, krunlet.Request{
    Command: []string{"/bin/sh", "-c", "echo starting; sleep 1; echo finished"},
}, os.Stdout, os.Stderr)
```

The same combined `MaxOutputBytes` limit applies, and an `io.Writer` failure
aborts execution. Real-time streams for the long-running `VM` mode require
a guest-side framing protocol and are not yet supported.

### Isolation checklist for untrusted agents

- Treat the helper **and guest** as the same host security principal.
- Run the helper under a dedicated low-privilege UID with mount namespace
  confinement on Linux; use system policies to limit what virtio-fs can reach.
- Confine the **helper's** host network namespace / firewall rules; simply
  omitting an explicit guest NIC does not disable TSI.
- Add host-enforced filesystem quotas and process/memory limits; VM memory
  settings and output caps do not protect host storage from guest writes.
- Do not allow untrusted inputs to select `RootFS`, `HelperPath`, `LibraryPath`,
  or `Persistent` mode.

The implementation intentionally does **not** claim production-safe host
confinement without those platform-specific controls.
### Streaming request input

Use `RunIO` to forward an `io.Reader` to the guest without first
buffering stdin in `Request.Stdin`. Streaming stdout and stderr remain
bounded by `MaxOutputBytes`:

```go
result, err := runner.RunIO(ctx, krunlet.Request{
    Command: []string{"/bin/cat"},
}, inputReader, outputWriter, errorWriter)
```

`Session.RunIO` and `Session.RunStream` provide the same API for a
filesystem-persistent session (each call still starts its own VM). A nil
reader falls back to `Request.Stdin`. If a provided reader blocks forever,
cancellation terminates the helper; the OS-level read itself may continue
until the reader unblocks, so use context-aware input sources when possible.
