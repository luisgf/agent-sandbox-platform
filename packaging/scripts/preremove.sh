#!/bin/sh
# Before the files go. On an upgrade the service stays as it is; on a removal it is
# stopped and disabled (the VMs of a node that confines them keep running until
# `systemctl stop asp-vms.slice`: removing the agent must not kill workloads).
set -e
case "$1" in
remove | 0) # deb: remove; rpm: 0 left after this one
	if [ -d /run/systemd/system ]; then
		for unit in asp-control-plane asp-node-agent asp-server; do
			if [ -e "/lib/systemd/system/$unit.service" ] || [ -e "/usr/lib/systemd/system/$unit.service" ]; then
				systemctl disable --now "$unit.service" >/dev/null 2>&1 || true
			fi
		done
	fi
	;;
esac
exit 0
