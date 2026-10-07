#!/usr/bin/env bash
# The VMM of a sandbox runs as an unprivileged user of its own, on a real KVM host.
#
# A Cloud Hypervisor that a guest escaped into must land in a process that is not
# root, has no capability, and can reach the files of that one VM and nothing else.
# This boots two sandboxes and checks that, then that stop, resume, delete, a restart
# of the agent and a crashed VMM still work and leave nothing owned by a VM's user.
#
# Needs root, /dev/kvm, systemd, cloud-hypervisor, virtiofsd, setpriv and a guest
# kernel and rootfs built from this tree. Nothing it starts is bound to the real
# node-agent of the host: it uses its own directories, ports, node id and guest
# subnet, and stops only the units of its own sandboxes.
#
# Usage:
#   sudo ASP_SMOKE_ROOTFS=/path/rootfs.img [ASP_SMOKE_KERNEL=/opt/sandbox/vmlinux] \
#        [ASP_SMOKE_BIN=build] ./scripts/smoke-vmm-user-kvm.sh
# ASP_SMOKE_BIN holds the api, node-agent and asp binaries (linux).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${ASP_SMOKE_BIN:-$ROOT/build}"
KERNEL="${ASP_SMOKE_KERNEL:-/opt/sandbox/vmlinux}"
ROOTFS="${ASP_SMOKE_ROOTFS:?set ASP_SMOKE_ROOTFS to the guest rootfs image}"
CH="${ASP_SMOKE_CH:-cloud-hypervisor}"
WORK="${ASP_SMOKE_DIR:-/var/tmp/asp-smoke-vmm-user}"
CP_PORT=18399
AGENT_PORT=19301

NODE_ID=smoke-vmm-user
GUEST_SUBNET=10.235.0.0/16
# shellcheck source=scripts/smoke-lib.sh
source "$ROOT/scripts/smoke-lib.sh"
# shellcheck source=scripts/kvm-lib.sh
source "$ROOT/scripts/kvm-lib.sh"
kvm_check_requirements
trap kvm_cleanup EXIT

kvm_prepare
kvm_start_cp

echo "==> start the node (--vm-unprivileged=on)"
kvm_start_agent
grep -q "unprivileged=true" "$AGENT_LOG" || fail "the agent does not run its VMMs as users: $(grep -E 'unprivileged|WARN|ERROR' "$AGENT_LOG" | head -3)"

echo "==> two sandboxes"
asp session start --name a --workspace "$WORK/ws/default/proj" --timeout 180s >/dev/null || fail "sandbox a did not start"
asp session start --name b --timeout 180s >/dev/null || fail "sandbox b did not start"
A=$(sid a)
B=$(sid b)
A_VMM=$(vmm_pid "$A")
B_VMM=$(vmm_pid "$B")
A_UID=$(ps -o uid= -p "$A_VMM" | tr -d ' ')
B_UID=$(ps -o uid= -p "$B_VMM" | tr -d ' ')

echo "==> the VMM is not root and can do nothing else"
[[ "$A_UID" -ge "$UID_BASE" ]] || fail "VMM of a runs as uid $A_UID, below the user base $UID_BASE"
[[ "$A_UID" != "$B_UID" ]] || fail "both VMMs run as uid $A_UID"
echo "  ok   a runs as $A_UID, b as $B_UID"
want "no capability in the effective set" '^0+$' "$(status_of "$A_VMM" CapEff)"
want "none in the bounding set, so none can come back" '^0+$' "$(status_of "$A_VMM" CapBnd)"
want "no_new_privs" '^1$' "$(status_of "$A_VMM" NoNewPrivs)"
want "seccomp filter on" '^2$' "$(status_of "$A_VMM" Seccomp)"
want "its own cgroup" "asp-vms.slice/asp-vm-$A.service" "$(cat "/proc/$A_VMM/cgroup")"
want "the unit forbids IP sockets" 'AF_UNIX' "$(systemctl show -p RestrictAddressFamilies --value "asp-vm-$A.service")"
want "and IP traffic" '0.0.0.0/0' "$(systemctl show -p IPAddressDeny --value "asp-vm-$A.service")"
# virtiofsd stays root (it must act as whichever user owns the workspace files), chrooted.
FS_PID=$(systemctl show -p MainPID --value "asp-vm-$A-fs.service")
want "virtiofsd is chrooted into the workspace" '--sandbox chroot' "$(tr '\0' ' ' <"/proc/$FS_PID/cmdline")"

echo "==> what the VMM was handed"
want "its disk" "^$A_UID\$" "$(owner_uid "$WORK/disks/rootfs-$A.img")"
want "its directory" "^$A_UID\$" "$(owner_uid "$VMDIR/$A")"
want "its TAP" "user $A_UID" "$(ip -d link show "asp-${A:0:8}")"
want "the node's private directory is not reachable by it" 'Permission denied' \
  "$(setpriv --reuid="$A_UID" --regid="$A_UID" --clear-groups -- ls "$WORK/run" 2>&1 || true)"

echo "==> a guest works"
want "exec" 'sandboxd' "$(gx a id)"
want "the workspace is shared both ways" 'host-file' "$(gx a 'cat /workspace/host.txt')"
gx a 'sh -c "echo from-guest > /workspace/guest.txt"' >/dev/null
want "a file the guest wrote" 'from-guest' "$(cat "$WORK/ws/default/proj/guest.txt")"
# More than a read and a write: what virtiofsd does in the workspace under its own limits.
want "directories, links, renames, a big file" '^50331648 ok$' "$(gx a 'sh -c "cd /workspace && mkdir -p d && ln -sf ../host.txt d/link && mv guest.txt d/moved.txt && chmod 640 d/moved.txt && dd if=/dev/zero of=big bs=1M count=48 2>/dev/null && sync && echo \$(stat -c %s big) \$(cat d/link >/dev/null && echo ok)"')"
want "the same on the host" 'moved.txt' "$(ls "$WORK/ws/default/proj/d")"
GUEST_IP=$(ip -o -4 addr show dev "asp-${A:0:8}" | awk '{print $4}' | cut -d/ -f1 | awk -F. '{printf "%s.%s.%s.%d", $1,$2,$3,$4+1}')
ping -c1 -W3 "$GUEST_IP" >/dev/null || fail "the host cannot reach the guest at $GUEST_IP through the TAP"
echo "  ok   the TAP carries traffic to $GUEST_IP"
SSH_REPLY=$(asp session exec --name a --buffered -- perl -MIO::Socket::UNIX -e '$s=IO::Socket::UNIX->new(Peer=>q(/run/agent-sandbox/ssh-agent.sock)) or die "connect: $!"; print $s pack(q(NC),1,11); read($s,$h,4) or die "no reply"; $n=unpack(q(N),$h); read($s,$b,$n); printf("type=%d\n", ord(substr($b,0,1)))' 2>&1)
want "the SSH agent acceptor answers through the VMM" 'type=12' "$SSH_REPLY"

echo "==> one VM's user cannot touch another VM"
as_a() { setpriv --reuid="$A_UID" --regid="$A_UID" --clear-groups --no-new-privs -- "$@"; }
as_a sh -c "exec 3<>'$WORK/disks/rootfs-$A.img'" || fail "a cannot open its own disk"
for p in "$WORK/disks/rootfs-$B.img" "$VMDIR/$B/api.sock"; do
  if as_a sh -c "exec 3<>'$p'" 2>/dev/null; then fail "a's user opened $p"; fi
done
as_a sh -c "ls '$VMDIR/$B'" >/dev/null 2>&1 && fail "a's user lists the directory of b"
as_a sh -c "ls '$VMDIR'" >/dev/null 2>&1 && fail "a's user lists the VM directory"
echo "  ok   a's user cannot open b's disk or socket, enter its directory or list the others"

echo "==> stop and resume"
gx a 'sh -c "echo kept > /var/tmp/marker; sync"' >/dev/null
asp session stop --name a >/dev/null || fail "stop"
want "the disk is root's again" '^0$' "$(owner_uid "$WORK/disks/rootfs-$A.img")"
want "the VM directory is gone" '^gone$' "$(owner_uid "$VMDIR/$A")"
ip link show "asp-${A:0:8}" >/dev/null 2>&1 && fail "the TAP of a stayed"
asp session resume --name a >/dev/null || fail "resume"
A_VMM=$(vmm_pid "$A")
A_UID=$(ps -o uid= -p "$A_VMM" | tr -d ' ')
[[ "$A_UID" -ge "$UID_BASE" ]] || fail "the resumed VMM runs as $A_UID"
want "the disk is the VM's user's while it runs" "^$A_UID\$" "$(owner_uid "$WORK/disks/rootfs-$A.img")"
want "the guest data survived" 'kept' "$(gx a 'cat /var/tmp/marker')"

echo "==> the agent restarts: both VMs are adopted, as they are"
UP_A=$(gx a 'cut -d. -f1 /proc/uptime')
kvm_kill_agent
[[ "$(vmm_pid "$A")" == "$A_VMM" ]] || fail "the VMM of a did not survive the agent"
: >"$AGENT_LOG"
kvm_start_agent
grep -q "adopted the VMs a previous agent left running" "$AGENT_LOG" || fail "nothing was adopted: $(tail -5 "$AGENT_LOG")"
[[ "$(vmm_pid "$A")" == "$A_VMM" ]] || fail "the VMM of a changed"
UP_A2=$(gx a 'cut -d. -f1 /proc/uptime')
[[ "$UP_A2" -ge "$UP_A" ]] || fail "the guest rebooted ($UP_A -> $UP_A2)"
echo "  ok   same VMM $A_VMM, guest uptime $UP_A -> $UP_A2"
want "exec after the restart" 'kept' "$(gx a 'cat /var/tmp/marker')"
SSH_REPLY=$(asp session exec --name a --buffered -- perl -MIO::Socket::UNIX -e '$s=IO::Socket::UNIX->new(Peer=>q(/run/agent-sandbox/ssh-agent.sock)) or die "connect: $!"; print $s pack(q(NC),1,11); read($s,$h,4) or die "no reply"; $n=unpack(q(N),$h); read($s,$b,$n); printf("type=%d\n", ord(substr($b,0,1)))' 2>&1)
want "the acceptors the new agent opened are the VMM's" 'type=12' "$SSH_REPLY"

echo "==> the VMM of b is killed: released, nothing left to its user"
kill -9 "$B_VMM"
for _ in $(seq 1 30); do
  st=$(curl -fsS "$CP/v1/sandboxes/$B" | json_field state)
  [[ "$st" == "stopped" ]] && break
  sleep 1
done
[[ "$st" == "stopped" ]] || fail "b is $st after its VMM died"
want "its disk is root's" '^0$' "$(owner_uid "$WORK/disks/rootfs-$B.img")"
want "its directory is gone" '^gone$' "$(owner_uid "$VMDIR/$B")"

echo "==> an agent that dies, and a VMM that dies meanwhile: the next start takes it back"
asp session resume --name b >/dev/null || fail "resume b"
B_VMM=$(vmm_pid "$B")
B_UID=$(ps -o uid= -p "$B_VMM" | tr -d ' ')
kvm_kill_agent
kill -9 "$B_VMM"
sleep 2
want "the disk stays its user's while nobody looks" "^$B_UID\$" "$(owner_uid "$WORK/disks/rootfs-$B.img")"
: >"$AGENT_LOG"
kvm_start_agent
want "the disk is root's again after the cleanup" '^0$' "$(owner_uid "$WORK/disks/rootfs-$B.img")"
want "and its directory is gone" '^gone$' "$(owner_uid "$VMDIR/$B")"
want "a is still running, adopted" 'kept' "$(gx a 'cat /var/tmp/marker')"

echo "==> delete"
for n in a b; do
  id=$(sid "$n")
  asp session rm --name "$n" >/dev/null || true
  wait_state "$CP" "$id" deleted 60 || fail "$n was not deleted"
  want "no disk of $n" '^gone$' "$(owner_uid "$WORK/disks/rootfs-$id.img")"
  want "no directory of $n" '^gone$' "$(owner_uid "$VMDIR/$id")"
  systemctl is-active --quiet "asp-vm-$id.service" && fail "the unit of $n is still active"
done
echo "OK smoke-vmm-user-kvm"
