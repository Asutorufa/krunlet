# Native component corresponding sources

Krunlet embeds precompiled `libkrun` and `libkrunfw`. The release assets contain
a separate `native-manifest-OS-ARCH.json` for each target, with SHA-256 digests
of the **exact files** used by go:embed. Use the manifest, not this document,
to select a specific embedded firmware release.

- libkrun 1.19.6, commit `227b2de6ed323fe180e02f871c5f325a90c13cc2`:
  https://github.com/libkrun/libkrun/tree/v1.19.6
- libkrunfw 5.5.0, Linux ABI 5 (distribution package 5.5.0+ds-1):
  https://github.com/libkrun/libkrunfw/tree/v5.5.0
- libkrunfw 5.6.2, macOS ABI 5:
  https://github.com/libkrun/libkrunfw/tree/v5.6.2
- Linux kernel 6.12.91 source (libkrunfw v5.5.0):
  https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.12.91.tar.xz
- Linux kernel 6.12.109 source (libkrunfw v5.6.2):
  https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.12.109.tar.xz
- Corresponding kernel patch series:
  https://github.com/libkrun/libkrunfw/tree/v5.5.0/patches
  https://github.com/libkrun/libkrunfw/tree/v5.6.2/patches
- Distribution source for the exact Ubuntu libkrunfw5 `5.5.0+ds-1`
  packaging and any downstream patches must also accompany/source the
  precompiled Debian package. See the Ubuntu source package
  https://launchpad.net/ubuntu/+source/libkrunfw/5.5.0+ds-1

License notices: libkrun is Apache-2.0. libkrunfw is LGPL-2.1-only and
includes a GPL-2.0-only Linux kernel. The license texts are distributed in
this repository and in the Release assets. Redistributors must satisfy
the applicable corresponding-source requirements; a link alone may not
suffice for all distributions.
