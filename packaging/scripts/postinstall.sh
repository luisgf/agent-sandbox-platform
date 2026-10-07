#!/bin/sh
# After the files are in place. It never starts or restarts a service: a restart of the
# control plane forgets a memory store, and the first upgrade of a node-agent to a
# version that adopts VMs (ADR-0014) still stops the VMs it finds.
set -e
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload >/dev/null 2>&1 || true
	for unit in asp-control-plane asp-node-agent; do
		if systemctl is-active --quiet "$unit.service" 2>/dev/null; then
			echo "$unit is running the version it had before: systemctl restart $unit runs this one."
		fi
	done
fi
exit 0
