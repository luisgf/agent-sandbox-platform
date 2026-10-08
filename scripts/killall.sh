#!/bin/sh
# Stops every sandbox VM on this node and removes what they leave behind: the units asp-vm-*, their
# TAPs and sockets, the nftables table of the egress proxy. The control plane is not touched, and the
# stopped sandboxes keep their disks. Installed by install.sh as /usr/local/bin/asp-killall.sh.
#
# A node-agent that restarts adopts the VMs it finds (they outlive it), so stopping the agent is
# not enough to stop the VMs: this is what does.
set -u
[ "$(id -u)" -eq 0 ] || { echo "asp-killall: run as root" >&2; exit 1; }

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
	# The agent first, so that it does not boot a sandbox while the others go down.
	systemctl stop asp-node-agent.service 2>/dev/null
	systemctl stop asp-vms.slice 2>/dev/null
	for u in $(systemctl list-units --all --plain --no-legend 'asp-vm-*' 2>/dev/null | awk '{print $1}'); do
		systemctl stop "$u" 2>/dev/null
	done
	systemctl reset-failed 'asp-vm-*' 2>/dev/null
fi
# What the agent's ExecStopPost (--reap-only, with the node's own settings) did not reach: the TAPs and
# tunnels this host's ASP created.
for l in $(ip -o link show 2>/dev/null | awk -F': ' '{print $2}' | cut -d@ -f1 | grep -E '^(asp-|wg-asp-)'); do
	ip link delete "$l" 2>/dev/null
done
if command -v nft >/dev/null 2>&1; then
	nft delete table ip asp_egress 2>/dev/null
	nft delete table ip6 asp_egress 2>/dev/null
fi
echo "asp-killall: the sandboxes' VMs are stopped"
exit 0
