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
# mktemp -d creates the directory 0700, and `cp -a "$TMP"/.` below copies that
# mode onto the image root. Only root could then traverse / in the guest.
chmod 0755 "$TMP"
docker export "$CID" | tar -C "$TMP" -xf -
# docker create injects /.dockerenv. systemd then treats the VM as a container.
rm -f "$TMP/.dockerenv"

# Optional kernel modules for the guest vmlinux. AF_VSOCK is modular on the
# Ubuntu kernel (socket() returns EAFNOSUPPORT until vsock.ko is loaded).
# The directory name must be the guest kernel's uname -r.
if [[ -n "${ASP_GUEST_MODULES:-}" ]]; then
  if [[ ! -d "$ASP_GUEST_MODULES" ]]; then
    echo "ASP_GUEST_MODULES is not a directory: $ASP_GUEST_MODULES" >&2
    exit 1
  fi
  ver="$(basename "$ASP_GUEST_MODULES")"
  mkdir -p "$TMP/lib/modules"
  cp -a "$ASP_GUEST_MODULES" "$TMP/lib/modules/$ver"
fi

# Ensure runtime dirs exist for systemd / pod-daemon / ssh-agent-vsock
mkdir -p "$TMP/run/agent-sandbox" "$TMP/etc/systemd/system"
for unit in pod-daemon.service ssh-agent-vsock.service workspace-virtiofs.service cmdline-ip.service; do
  if [[ -f "$ROOT/images/guest/systemd/$unit" ]]; then
    cp "$ROOT/images/guest/systemd/$unit" "$TMP/etc/systemd/system/"
  fi
done
mkdir -p "$TMP/usr/local/share/asp"
for helper in ssh-agent-vsock-socat.sh mount-virtiofs-workspace.sh cmdline-ip.sh; do
  if [[ -f "$ROOT/images/guest/helpers/$helper" ]]; then
    cp "$ROOT/images/guest/helpers/$helper" "$TMP/usr/local/share/asp/"
    chmod 0755 "$TMP/usr/local/share/asp/$helper"
  fi
done

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
    ln -sfn /etc/systemd/system/workspace-virtiofs.service \
      "$MNT/etc/systemd/system/multi-user.target.wants/workspace-virtiofs.service" 2>/dev/null || true
    ln -sfn /etc/systemd/system/cmdline-ip.service \
      "$MNT/etc/systemd/system/multi-user.target.wants/cmdline-ip.service" 2>/dev/null || true
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
