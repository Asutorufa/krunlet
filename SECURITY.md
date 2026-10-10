# Security policy and threat model

Krunlet is experimental infrastructure, **not** a fully hardened security
boundary for arbitrary adversarial VMs. Report vulnerabilities privately via
GitHub's security advisory feature rather than a public issue.

## Trust assumptions

- **Trusted:** the host administrator, libkrun/libkrunfw installation,
  compiled native libraries, the host kernel/hypervisor, the rootfs template
  and supplied custom kernel. The caller that constructs `Options` is trusted.
- **Untrusted:** guest commands, their arguments, network traffic and files
  produced inside each disposable guest. The guest may attempt privilege
  escalation or cause resource exhaustion.
- **Host privilege:** libkrun's VMM and virtio-fs run within a host helper
  process with the effective privileges of the caller. The helper is not a
  privilege boundary and must not run as root unless an independently
  reviewed deployment requires it.
- A microVM provides a virtualization boundary that generally exceeds
  ordinary shared-kernel containers, but Krunlet does **not** provide
  Firecracker's hardened, jailer-oriented deployment model by default.
  Hypervisor escape and virtio device attacks remain in scope.

## Isolation limitations

A per-run rootfs copy prevents **normal filesystem writes** from appearing in
other run copies. It does not confine the host-side virtio-fs process or
guarantee no host filesystem access through VMM vulnerabilities. Use host
mount/user namespaces, cgroup v2 limits, seccomp profiles, Linux LSM policy
or platform equivalents around the helper. Separate the privileged VM
launcher from untrusted API tenants.

Krunlet applies both a per-Runner semaphore and a process-wide VM
admission limit shared by **all** Runner instances in one Go process.
`ConfigureProcessVMLimit` may be called only before the first admission.
Separate host processes still require external service-level admission or
a host scheduler; the in-process limit is not a multi-process quota. `MaxRootFSBytes`
counts logical file bytes at staging time, not all allocated blocks, inodes,
copy-on-write amplification, or ongoing guest filesystem writes. Put the
staging directory on a quota-limited filesystem.

A parent-exit watcher protects the **built-in helper** (Linux pidfd plus
PR_SET_PDEATHSIG; macOS kqueue NOTE_EXIT). A custom `HelperPath` program may
not implement that watcher, even though Linux still supplies Pdeathsig.
On Linux, configure `CgroupV2{Parent: ...}` with an administrator-
delegated writable cgroup v2 subtree to place the helper directly in a
per-VM kernel cgroup via `CLONE_INTO_CGROUP`. Krunlet sets
`memory.max` and `pids.max` and invokes `cgroup.kill` at teardown.
This catches descendants that escape a Unix process group via
`setsid`/`setpgid`. If cgroup delegation or controllers are missing,
startup fails closed rather than silently downgrading to PGID cleanup.
When no cgroup is configured, process groups **cannot** contain such
escaped descendants. A parent SIGKILL also requires independent host
supervision of stale cgroup directories; the builtin helper attempts a
cgroup.kill when its parent-exit monitor fires. `SIGKILL` leaves no opportunity for in-process cleanup; only
lock-marked stale staging directories are eligible for eventual collection.

Krunlet defaults to network disabled. `Network=true` **without**
`NetworkPolicy` or `Yuhaiin` enables libkrun's host-mediated **TSI**;
guest connections can reach host network services and should not be used for
untrusted agents. With a policy, Krunlet uses a **gVisor Netstack + virtio-net**
outbound TCP/UDP gateway; the host's libkrun must be built with `NET=1`.
The network policy constrains host dial destinations, not encrypted content,
DNS identities, guest-originated arbitrary protocols, or kernel/VMM escapes.
The optional native yuhaiin inbound transfers reconstructed TCP/UDP to
yuhaiin's routing rules; it does not add a second isolation layer by itself.
The helper cgroup does **not** contain the gVisor gateway, which currently
runs as goroutines of the Krunlet parent process, nor any independent
yuhaiin service. To confine these to the same per-VM limit they must
be deployed as separate managed processes/cgroups under a trusted host
supervisor. Do not describe the current `CgroupV2` option as complete
host-network-backend isolation.

Do not publish administrative host ports through `PortMaps` to untrusted VMs.
A private Unix socket with mode 0600 is only as isolated as its parent
directory and the principals allowed to open it.

## Deployment checklist

1. Run the helper without root privileges, under a dedicated service user.
2. The administrator (not the agent or guest) must prepare a trusted
   immutable rootfs template. Store the source and template directory
   outside guest-writable mounts, restrict host permissions, and ensure the
   guest never receives a writeable path to the template itself.
   Never use `/`, the host home directory or a shared mutable tree.
   `PrepareTemplate(...).NewRunner(...)` **forces Persistent=false**
   even if the caller supplied true: each run clones the template.
   Outside template mode, `--persistent` directly modifies the original
   trusted rootfs and is unsafe for adversarial workloads.
3. Enforce host-level disk/inode/pid/memory limits **outside** Krunlet.
4. Disable networking unless explicitly required. Prefer allowlist rules.
5. Run virtualization and network-bypass tests against the exact native
   libkrun/libkrunfw versions on KVM/HVF hosts before exposing to tenants.
6. Keep the Go dependency graph and native VMM libraries patched.
