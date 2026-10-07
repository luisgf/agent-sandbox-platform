#!/usr/bin/env bash
# The old name of scripts/build-guest-image.sh, which builds the guest image the same bytes
# every time and without root or a loop mount. This keeps the old call working:
#   ./scripts/build-guest-rootfs.sh [output-rootfs.img]
# The image is built into a temporary directory and rootfs.img is copied to the path given
# (default build/rootfs.img). ASP_GUEST_MODULES works as it did.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${1:-$ROOT/build/rootfs.img}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
"$ROOT/scripts/build-guest-image.sh" "$TMP/guest"
mkdir -p "$(dirname "$OUT")"
cp "$TMP/guest/rootfs.img" "$OUT"
echo "OK: $OUT"
echo "Symlink for reconciler defaults:"
echo "  sudo ln -sfn $OUT /opt/sandbox/rootfs.img"
echo "(scripts/build-guest-image.sh keeps image.json and SHA256SUMS too, and asp image pull installs a released one)"
