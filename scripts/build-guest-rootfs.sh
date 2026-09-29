#!/usr/bin/env bash
# Build guest OCI image and document/extract a raw ext4 rootfs.img for Cloud Hypervisor.
# Usage (from repo root):
#   ./scripts/build-guest-rootfs.sh [output-rootfs.img]
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${1:-$ROOT/build/rootfs.img}"
IMG_TAG="${ASP_GUEST_IMAGE_TAG:-agent-sandbox-guest:local}"
SIZE_MB="${ASP_ROOTFS_SIZE_MB:-512}"

mkdir -p "$(dirname "$OUT")"

echo "==> docker build $IMG_TAG"
docker build -t "$IMG_TAG" -f "$ROOT/images/guest/Dockerfile" "$ROOT"

echo "==> export container filesystem"
CID="$(docker create "$IMG_TAG")"
cleanup() { docker rm -f "$CID" >/dev/null 2>&1 || true; }
trap cleanup EXIT

TMP="$(mktemp -d)"
docker export "$CID" | tar -C "$TMP" -xf -

# Ensure runtime dirs exist for systemd / pod-daemon / ssh-agent-vsock
mkdir -p "$TMP/run/agent-sandbox" "$TMP/etc/systemd/system"
for unit in pod-daemon.service ssh-agent-vsock.service; do
  if [[ -f "$ROOT/images/guest/systemd/$unit" ]]; then
    cp "$ROOT/images/guest/systemd/$unit" "$TMP/etc/systemd/system/"
  fi
done
if [[ -f "$ROOT/images/guest/helpers/ssh-agent-vsock-socat.sh" ]]; then
  mkdir -p "$TMP/usr/local/share/asp"
  cp "$ROOT/images/guest/helpers/ssh-agent-vsock-socat.sh" "$TMP/usr/local/share/asp/"
fi

echo "==> create sparse ext4 image (${SIZE_MB}M) at $OUT"
rm -f "$OUT"
dd if=/dev/zero of="$OUT" bs=1M count="$SIZE_MB" status=none
mkfs.ext4 -F -L asp-rootfs "$OUT" >/dev/null

MNT="$(mktemp -d)"
if mount -o loop "$OUT" "$MNT" 2>/dev/null; then
  cp -a "$TMP"/. "$MNT"/
  # Enable unit if systemd is present in the tree
  if [[ -d "$MNT/etc/systemd/system" ]]; then
    mkdir -p "$MNT/etc/systemd/system/multi-user.target.wants"
    ln -sfn /etc/systemd/system/pod-daemon.service \
      "$MNT/etc/systemd/system/multi-user.target.wants/pod-daemon.service" 2>/dev/null || true
    ln -sfn /etc/systemd/system/ssh-agent-vsock.service \
      "$MNT/etc/systemd/system/multi-user.target.wants/ssh-agent-vsock.service" 2>/dev/null || true
  fi
  sync
  umount "$MNT"
  rmdir "$MNT"
  echo "OK: $OUT"
  echo "Symlink for reconciler defaults:"
  echo "  sudo ln -sfn $OUT /opt/sandbox/rootfs.img"
else
  rmdir "$MNT"
  echo "NOTE: loop mount requires privileges. Filesystem tree left at:"
  echo "  $TMP"
  echo "Manual:"
  echo "  dd if=/dev/zero of=$OUT bs=1M count=$SIZE_MB"
  echo "  mkfs.ext4 -L asp-rootfs $OUT"
  echo "  sudo mount -o loop $OUT /mnt && sudo cp -a $TMP/. /mnt/ && sudo umount /mnt"
  # Keep TMP for operator; disable cleanup of tree only
  trap - EXIT
  docker rm -f "$CID" >/dev/null 2>&1 || true
  exit 0
fi
rm -rf "$TMP"
