# Required KVM release gate

The `vm-integration` job is part of **every pull request and every
main-branch push**. It is deliberately not conditional on
`workflow_dispatch`, `/dev/kvm` discovery, or `KRUNLET_STRESS_32` opt-in.

## Host prerequisites

Use a Linux machine with KVM, libkrun **v1.19.6** and its compatible
firmware, plus a libkrun binary built with **NET=1**. Upstream libkrun
source reference: `v1.19.6`, Git commit
`227b2de6ed323fe180e02f871c5f325a90c13cc2`.

Run the GitHub Actions runner as a dedicated unprivileged service user.
Do not use the root account. Register the labels:

```text
self-hosted, linux, kvm, krunlet-vm
```

Ensure the runner service environment defines these **absolute** paths:

- `KRUNLET_TEST_ROOTFS`: a trusted, administrator-maintained rootfs
  containing `/bin/sh`, `/bin/true`, `sleep` and their libraries.
  Guest writes must not affect the template.
- `KRUNLET_TEST_CGROUP_PARENT`: a private, writable, delegated Linux
  cgroup v2 subtree that has `memory` and `pids` controllers enabled
  for child cgroups, supports `cgroup.kill`, and has no process in a
  conflicting non-leaf group.

The runner must have access to `/dev/kvm`; it must also be able to
create child cgroups and set `memory.max` and `pids.max`, but must not
be given unrestricted cgroup root access. Use systemd delegation and
scope resource limits rather than chmod 777 on a system cgroup.

If gVisor networking or a yuhaiin inbound is enabled in production,
their **host service processes need their own** externally managed
memory/pid-limited scopes. The per-VM helper cgroup does not contain
these separate services.

## What the required job verifies

- A real `doctor --rootfs` VM boots using libkrun 1.19.6 and exits 0.
- Required native `NET=1` symbol is present.
- Real VM execution respects timeout and reaps its helper.
- 32 real VM runs execute with a global/Runner cap and leave no
  matched helper command, rootfs staging directory or per-VM cgroup.
- A host helper spawning `setsid sleep` is still killed by
  `cgroup.kill`.
- A real VM's parent is `SIGKILL`ed, and the former VM cgroup becomes
  unpopulated.

No built-in fake helper can substitute for these checks.

## GitHub repository protection

Repository administrators must configure **Settings → Rules → Rulesets**
(or Branch protection rules) for `main` and mark `vm-integration`
as a required status check. Also require the ordinary Ubuntu, macOS,
lint and vulnerability checks. The workflow itself cannot configure
GitHub branch protection.

If the self-hosted runner is missing, the job **stays queued**.
Do not disable or skip the job just to merge a PR.

## Release

Never tag `v0.1.0` until a successful `vm-integration` check exists
for the exact main commit to be tagged. The release workflow rejects
tags without this check. Release artifacts have SHA-256 hashes in
`checksums.txt`; compare them with `sha256sum -c checksums.txt`
after downloading all artifacts on Linux. On macOS the equivalent
command is `shasum -a 256 -c checksums.txt`.

For independently installed libkrun binaries, the release pins the
**upstream source version and commit**, not a single universally
portable binary checksum. Verify the native package or custom build
against that upstream commit and its own package-manager digest.
