#!/bin/sh
# Turns the tar of the guest's filesystem into the ext4 image a node boots, the same bytes
# every time. It runs in the "packer" stage of the Dockerfile (docker run --tmpfs /work ...):
#
#   pack-rootfs <rootfs.tar> <rootfs.img> <size in MiB> <SOURCE_DATE_EPOCH>
#
# What would otherwise differ between two runs is fixed:
#   - the time: E2FSPROGS_FAKE_TIME for the filesystem's own stamps, SOURCE_DATE_EPOCH for
#     the times of the files (mke2fs 1.47.1 or later: it would otherwise copy the ctime the
#     extraction gave each one), and every mtime set to the build date as well;
#   - the identifiers mke2fs draws at random: the UUID and the directory hash seed;
#   - the order the files are laid out in: mke2fs -d walks the directory in the order the
#     filesystem under it lists it, and a tmpfs lists in the order the files were
#     created, which is the order of the tar. /work must be a tmpfs.
set -eu

tar_in=$1 img_out=$2 size_mib=$3 epoch=$4
work=/work/root
# Fixed, and the same for every build of every version: a disk image is copied from
# this one for each sandbox, so they share it anyway.
uuid=8f3a5c1e-5a0e-4b1d-9d6c-2b1f6f1d0a11

if ! grep -q ' /work tmpfs ' /proc/mounts; then
	echo "pack-rootfs: /work must be a tmpfs (docker run --tmpfs /work:exec,mode=0755): the layout of the image follows the order the files are listed in" >&2
	exit 1
fi

mkdir -p "$work"
tar -C "$work" --numeric-owner --same-owner -xpf "$tar_in"
# What docker puts in a container it creates (the tar of a classic build is an export of one):
# not part of the image. The guest writes /etc/hostname, hosts and resolv.conf itself at boot
# (cmdline-ip.service), and the kernel mounts devtmpfs over /dev.
rm -f "$work/.dockerenv" "$work/etc/hostname" "$work/etc/hosts" "$work/etc/resolv.conf"
find "$work/dev" -mindepth 1 -delete 2>/dev/null || true
# No file is newer than the build date, and none depends on when it was extracted.
find "$work" -depth -exec touch -h -d "@$epoch" {} +

rm -f "$img_out"
# SOURCE_DATE_EPOCH gives every file's times (mke2fs 1.47.1+); E2FSPROGS_FAKE_TIME the
# filesystem's own.
export SOURCE_DATE_EPOCH="$epoch" E2FSPROGS_FAKE_TIME="$epoch"
# orphan_file is left off so a kernel older than 6.5 mounts the image too.
mke2fs -q -t ext4 -F -L asp-rootfs -U "$uuid" -m 1 -O ^orphan_file \
	-E "hash_seed=$uuid,lazy_itable_init=0,lazy_journal_init=0,root_owner=0:0" \
	-d "$work" "$img_out" "${size_mib}M"
e2fsck -fn "$img_out" >/dev/null
# What the kernel and systemd need to find, through the usr-merge symlinks: an image without
# them boots into "No working init found".
for f in /sbin/init /lib/systemd/systemd /usr/local/bin/pod-daemon; do
	if ! debugfs -R "stat $f" "$img_out" 2>/dev/null | grep -q '^Inode:'; then
		echo "pack-rootfs: $f is not in the image" >&2
		exit 1
	fi
done
# The compressed copy that gets downloaded: gzip -n keeps no name or time in it, and
# this gzip is the one of the snapshot, so its bytes do not depend on the host either.
gzip -9n -c "$img_out" >"$img_out.gz"
