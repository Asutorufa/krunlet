#!/usr/bin/env bash
# The old installer accepted any matching hashes and would install this
# tampered, unsigned binary. The new one must reject it before installation.
set -euo pipefail
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/server" "$tmp/tools"
case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; *) exit 0 ;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) exit 0 ;; esac
asset="krunlet-$os-$arch"
printf '#!/bin/sh\necho malicious executable\n' > "$tmp/server/$asset"
chmod +x "$tmp/server/$asset"
(
 cd "$tmp/server"
 if [ "$os" = darwin ]; then
   shasum -a 256 "$asset" > checksums.txt
 else
   sha256sum "$asset" > checksums.txt
 fi
)
printf 'untrusted signature\n' > "$tmp/server/checksums.sig"
cat > "$tmp/tools/curl" <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
url= output=
while [ "$#" -gt 0 ]; do
 case "$1" in
   -o) output="$2"; shift 2 ;;
   -*) shift ;;
   *) url="$1"; shift ;;
 esac
done
cp "$KRUNLET_TEST_SERVER/$(basename "$url")" "$output"
SCRIPT
chmod +x "$tmp/tools/curl"
if PATH="$tmp/tools:$PATH" KRUNLET_TEST_SERVER="$tmp/server" \
   KRUNLET_INSTALL_DIR="$tmp/install" KRUNLET_VERSION=invalid-test \
   bash install.sh > "$tmp/log" 2>&1; then
 echo 'Installer accepted tampered manifest and unsigned binary' >&2
 exit 1
fi
if [ -e "$tmp/install/krunlet" ]; then
 echo 'Installer wrote executable before signature verification' >&2
 exit 1
fi
grep -qi 'signature' "$tmp/log"
