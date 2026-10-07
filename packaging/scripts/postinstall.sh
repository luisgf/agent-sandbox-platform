#!/bin/sh
# After the files are in place. It never starts or restarts a service: a restart of the
# control plane forgets a memory store, and the first upgrade of a node-agent to a
# version that adopts VMs (ADR-0014) still stops the VMs it finds.
set -e
# The control plane runs as a system user of its own, with no login.
if [ -e /lib/systemd/system/asp-control-plane.service ] || [ -e /usr/lib/systemd/system/asp-control-plane.service ]; then
	if ! getent passwd asp-control-plane >/dev/null 2>&1 && command -v useradd >/dev/null 2>&1; then
		useradd --system --user-group --home-dir /var/lib/asp-control-plane --no-create-home \
			--shell /usr/sbin/nologin asp-control-plane || true
	fi
fi
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload >/dev/null 2>&1 || true
	for unit in asp-control-plane asp-node-agent; do
		if systemctl is-active --quiet "$unit.service" 2>/dev/null; then
			echo "$unit is running the version it had before: systemctl restart $unit runs this one."
		fi
	done
fi
exit 0
