#!/usr/bin/env bash
# Install a prebuilt Krunlet CLI with embedded libkrun and libkrunfw.
set -euo pipefail

repo="${KRUNLET_REPO:-Asutorufa/krunlet}"
version="${KRUNLET_VERSION:-main}"
install_dir="${KRUNLET_INSTALL_DIR:-/usr/local/bin}"

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "Unsupported operating system: $(uname -s)" >&2; exit 1 ;;
esac

case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) echo "Unsupported CPU architecture: $(uname -m)" >&2; exit 1 ;;
esac
if [ "$os" = darwin ] && [ "$arch" != arm64 ]; then
  echo "Krunlet prebuilt CLI requires Apple Silicon on macOS" >&2
  exit 1
fi

asset="krunlet-${os}-${arch}"
base="https://github.com/$repo/releases/download/$version"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
umask 077

curl --fail --location --retry 3 "$base/$asset" -o "$tmp/$asset"
curl --fail --location --retry 3 "$base/checksums.txt" -o "$tmp/checksums.txt"
curl --fail --location --retry 3 "$base/checksums.sig" -o "$tmp/checksums.sig"

if ! command -v openssl >/dev/null 2>&1; then
  echo "Cannot verify Krunlet Release signature: openssl command missing" >&2
  exit 1
fi
# The signing public key is pinned here, not downloaded from the Release.
cat > "$tmp/release-public.pem" <<'KRUNLET_RELEASE_PUBKEY'
-----BEGIN PUBLIC KEY-----
MIIBojANBgkqhkiG9w0BAQEFAAOCAY8AMIIBigKCAYEAj8lwYTDpz4v9g9Lw+By+
kxpQ/wJFm4Oj4uw7iN9/7gAq9iujFw2ComK+kgxw+zWmKD52GTfx7CIuKKT+o20g
zNQFX4wg4T1pdoP0l4M7dNy0GBh1TOaaXCk5PZOeITwCTqScKZq5biknoVNm5Awb
dDtTm1CL+r+2mOHXHZ6i1R8jmOBl0Xmh9nmH28SuwGz1SAjh6EsfXkKqwG3m2wAQ
uHpr5pr6ZSaJ2etmuMTMr6QBQ3z50cvyUjHTx1vSbhL8PlE71FmRB/X/qp42II8v
m0X0bU/EIgURSNpkfk25Z1+Mt6t8ZojBz0MWaw5dcm7ijFCwxVRcnLp2T5JxXZVm
2fdV0BNbKTGyCtauDfQc4BksL47hWZ3nQ7ZJLxKMh8Jvq1HUM/kQmxlhM+j4Gw6M
afieUEHN9XQjSP3XDbC6bMDgKrJN/ZSEICO0IakV6TxJ3w1SzWP2TOn2EOCEi2ju
AgKGV+EENHftotH4ruEBzDXO+amCkRNfWwf5mOLV8grTAgMBAAE=
-----END PUBLIC KEY-----
KRUNLET_RELEASE_PUBKEY
if ! openssl dgst -sha256 -verify "$tmp/release-public.pem" \
  -signature "$tmp/checksums.sig" "$tmp/checksums.txt"; then
  echo "Krunlet Release manifest signature INVALID: refusing installation" >&2
  exit 1
fi

(
  cd "$tmp"
  # Refuse unlisted or duplicate hashes. The manifest is tied to the
  # same GitHub Release as the binary.
  awk -v target="$asset" '$2 == target {print; found++} END {if (found != 1) exit 1}' checksums.txt > asset.sha256
  if [ "$os" = darwin ]; then
    shasum -a 256 -c asset.sha256
  else
    sha256sum -c asset.sha256
  fi
)

# Nothing requires root until the final install, and callers can choose a
# user-writable directory with KRUNLET_INSTALL_DIR.
mkdir -p "$install_dir"
install -m 0755 "$tmp/$asset" "$install_dir/krunlet"
"$install_dir/krunlet" version
echo "Krunlet CLI installed to $install_dir/krunlet"
echo "Embedded libkrun 1.19.6 and libkrunfw are included; host KVM/HVF and a trusted rootfs are still required."
