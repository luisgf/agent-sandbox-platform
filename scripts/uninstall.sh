#!/bin/sh
# Removes ASP from this host: stops its VMs and services, takes away the packages (or the files of a
# tarball install) and the links in /opt/sandbox. What is in /var/lib stays (the CA and keys of a
# control plane, the disks of stopped sandboxes, the guest images) unless --purge says otherwise.
# Installed by install.sh as /usr/local/bin/asp-uninstall.sh.
#
#   asp-uninstall.sh [--purge] [--yes]
set -u
purge=0
yes=0
for a in "$@"; do
	case "$a" in
	--purge) purge=1 ;;
	--yes | -y) yes=1 ;;
	*) echo "usage: asp-uninstall.sh [--purge] [--yes]" >&2; exit 2 ;;
	esac
done
[ "$(id -u)" -eq 0 ] || { echo "asp-uninstall: run as root" >&2; exit 1; }

if [ "$purge" -eq 1 ] && [ "$yes" -ne 1 ]; then
	echo "--purge deletes /etc/asp, /var/lib/asp and /var/lib/asp-control-plane:"
	echo "the control plane's CA and keys, the disks of stopped sandboxes, the guest images."
	printf 'Type "delete" to go on: '
	read -r answer
	[ "$answer" = delete ] || { echo "not purging"; exit 1; }
fi

# The VMs and the node-agent.
if [ -x /usr/local/bin/asp-killall.sh ]; then
	/usr/local/bin/asp-killall.sh
elif [ -x "$(dirname "$0")/killall.sh" ]; then
	"$(dirname "$0")/killall.sh"
fi
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
	systemctl disable --now asp-node-agent.service asp-control-plane.service 2>/dev/null
fi

# The packages, or the files of a tarball install.
if command -v dpkg >/dev/null 2>&1 && { dpkg -s asp-node-agent || dpkg -s asp-control-plane || dpkg -s asp; } >/dev/null 2>&1; then
	flag=-r
	[ "$purge" -eq 1 ] && flag=-P
	dpkg "$flag" asp-node-agent asp-control-plane asp 2>/dev/null
elif command -v rpm >/dev/null 2>&1; then
	for p in asp-node-agent asp-control-plane asp; do
		rpm -q "$p" >/dev/null 2>&1 && rpm -e "$p"
	done
fi
for b in asp asp-control-plane asp-node-agent; do rm -f "/usr/local/bin/$b"; done
rm -f /etc/systemd/system/asp-node-agent.service /etc/systemd/system/asp-control-plane.service
command -v systemctl >/dev/null 2>&1 && systemctl daemon-reload 2>/dev/null

# The links asp image pull made; a file of somebody's own is left.
for f in vmlinux rootfs.img SHA256SUMS image.json; do
	[ -L "/opt/sandbox/$f" ] && rm -f "/opt/sandbox/$f"
done
rmdir /opt/sandbox 2>/dev/null

if [ "$purge" -eq 1 ]; then
	rm -rf /etc/asp /var/lib/asp /var/lib/asp-control-plane /run/asp /run/asp-vm
	getent passwd asp-control-plane >/dev/null 2>&1 && userdel asp-control-plane 2>/dev/null
	echo "asp-uninstall: ASP and its data are gone"
else
	echo "asp-uninstall: ASP is gone; /etc/asp and /var/lib/asp* were kept (--purge removes them)"
fi
rm -f /usr/local/bin/asp-killall.sh /usr/local/bin/asp-uninstall.sh
exit 0
