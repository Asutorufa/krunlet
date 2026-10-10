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

Krunlet enforces a process-wide semaphore across all Runners, configured
with `SetGlobalVMLimit`, in addition to each Runner's own cap. The global
limit is **per OS process**, not across multiple processes or machines.
For untrusted agents in production on Linux, configure `CgroupParent` to
a dedicated, pre-delegated cgroup v2 subtree where `memory.max`, `pids.max`
and `cgroup.kill` are writable. Cgroup setup/attachment errors fail closed,
and the built-in helper waits for its host cgroup assignment before starting
libkrun or creating descendants. A per-VM cgroup contains setsid/setpgid
descendants that stay within the delegated cgroup hierarchy. `MaxRootFSBytes`
counts logical file bytes at staging time, not all allocated blocks, inodes,
copy-on-write amplification, or ongoing guest filesystem writes. Put the
staging directory on a quota-limited filesystem.

**Network service processes are outside the helper's per-VM cgroup.**
Krunlet's gVisor Netstack gateway executes inside the host-side Runner
process; a native yuhaiin inbound may execute in a completely separate,
shared service. `CgroupParent` does **not** bound memory or PID usage of
those services. For untrusted networking, configure **separate host-service
cgroup v2 scopes** for the Krunlet caller and yuhaiin daemon, with their own
`memory.max`, `pids.max`, service-level process supervision and
restart limits. An externally hosted yuhaiin daemon cannot safely be moved
into a per-VM cgroup when it serves multiple VMs. Until those service scopes
are validated, do not treat the overall network stack as bounded isolation.

A per-VM cgroup is not an independent privileged watchdog if the entire
Krunlet process and its helper are killed before a parent-death watcher can
execute. Use systemd transient scopes / a separate supervisor with
`KillMode=control-group` for stronger parent-SIGKILL guarantees. Never
delegate permission for guest-controlled code to migrate itself out of the
cgroup. Without `CgroupParent`, only the process-group kill path remains.

A parent-exit watcher protects the **built-in helper** (Linux pidfd plus
PR_SET_PDEATHSIG; macOS kqueue NOTE_EXIT). A custom `HelperPath` program may
not implement that watcher, even though Linux still supplies Pdeathsig.
Process-group cleanup cannot contain children that explicitly escape the
group using setsid/setpgid, and does not replace a cgroup v2 `cgroup.kill`
boundary. `SIGKILL` leaves no opportunity for in-process cleanup; only
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

Do not publish administrative host ports through `PortMaps` to untrusted VMs.
A private Unix socket with mode 0600 is only as isolated as its parent
directory and the principals allowed to open it.

## Deployment checklist

1. Run the helper without root privileges, under a dedicated service user.
2. An administrator must prepare and verify the rootfs template in a
   non-group/world-writable directory, inaccessible for writes to Guest
   identities. `PrepareTemplate` copies it into a private host snapshot and
   rejects group/other-writable source directories. A template-based Runner
   rejects `Persistent=true` (instead of silently accepting it); using
   `--persistent` directly modifies the host source rootfs and **must never**
   be allowed for untrusted agent commands. Never use `/` or host home.
3. Set `CgroupParent` for all untrusted Linux VM runs (admin-created
   delegated subtree), enforce disk/inode quotas separately, and ensure
   the parent PID watcher is used. Verify cgroup cleanup and descendant
   reaping on the actual hypervisor host.
4. Disable networking unless explicitly required. Prefer allowlist rules.
5. Run virtualization and network-bypass tests against the exact native
   libkrun/libkrunfw versions on KVM/HVF hosts before exposing to tenants.
6. Keep the Go dependency graph and native VMM libraries patched.
