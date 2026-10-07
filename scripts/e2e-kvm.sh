#!/usr/bin/env bash
# The life of a sandbox, end to end, on a real KVM host: start it with a workspace, run a command
# as the workspace's owner and as root, write data nobody syncs, stop, see that the disk was left
# clean, resume, find the data, delete it, and see that the node is left clean.
#
# These are the bugs that were found by hand after an upgrade: `/` at 0700, a stop that lost
# what the guest had not synced, a retained disk deleted by a self-fence, a node that kept a
# disk or a TAP of a sandbox that was gone. This is the check to run after an upgrade, and
# what the nightly lane runs (.github/workflow-drafts/nightly-kvm.yml).
#
# Two ways to run it:
#
#   sudo ASP_E2E_ROOTFS=/path/rootfs.img [ASP_E2E_KERNEL=/opt/sandbox/vmlinux] [ASP_E2E_BIN=build] \
#        scripts/e2e-kvm.sh
#     A throwaway control plane and node-agent on this host (own directories, ports, node id and
#     guest network; nothing of the host's own node is touched), with the host-level checks:
#     the disk is clean after the stop, and nothing of the sandbox is left on the node.
#
#   ASP_E2E_CONTROL_PLANE_URL=https://cp.example ASP_API_KEY=... [ASP_E2E_WORKSPACE=/srv/asp/workspaces/default/e2e] \
#        scripts/e2e-kvm.sh
#     An existing deployment, through its API only (any machine with `asp`): the same life, minus
#     what only the node's host can see. ASP_E2E_WORKSPACE is a directory on the node, under its
#     workspace root and owned by uid 1000, to test the owner; without it there is no workspace.
#     Credentials are the ones `asp` takes (ASP_API_KEY, ASP_ID_TOKEN, asp auth login).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${ASP_E2E_BIN:-$ROOT/build}"
WORK="${ASP_E2E_DIR:-/var/tmp/asp-e2e-kvm}"
NAME="e2e-$$"
# shellcheck source=scripts/smoke-lib.sh
source "$ROOT/scripts/smoke-lib.sh"

EXISTING=0
[[ -n "${ASP_E2E_CONTROL_PLANE_URL:-}" ]] && EXISTING=1

if (( EXISTING )); then
  WORK="$(mktemp -d)"
  command -v "$BIN/asp" >/dev/null 2>&1 || BIN="$(dirname "$(command -v asp)")"
  fail() { echo "FAIL: $*" >&2; exit 1; }
  CP="$ASP_E2E_CONTROL_PLANE_URL"
  # The caller's credentials, a session directory of our own.
  asp() { env HOME="${HOME:-$WORK}" ASP_CONTROL_PLANE_URL="$CP" ASP_SESSION_DIR="$WORK/sessions" "$BIN/asp" "$@" <"${ASP_STDIN:-/dev/null}"; }
  gx() { asp session exec --name "$1" --buffered --cmd "$2" 2>&1; }
  sid() { json_field sandbox_id <"$WORK/sessions/$1.json"; }
  want() { # label pattern answer
    if grep -qE -- "$2" <<<"$3"; then echo "  ok   $1 -> $(head -c 70 <<<"$3" | tr '\n' ' ')"; else fail "$1: wanted /$2/, got: $3"; fi
  }
  trap 'rc=$?; asp session rm --name "$NAME" >/dev/null 2>&1; rm -rf "$WORK"; exit $rc' EXIT
  mkdir -p "$WORK/sessions"
  WORKSPACE="${ASP_E2E_WORKSPACE:-}"
else
  KERNEL="${ASP_E2E_KERNEL:-/opt/sandbox/vmlinux}"
  ROOTFS="${ASP_E2E_ROOTFS:?set ASP_E2E_ROOTFS to the guest rootfs image (or ASP_E2E_CONTROL_PLANE_URL to test a deployment)}"
  CH="${ASP_E2E_CH:-cloud-hypervisor}"
  CP_PORT=18499
  AGENT_PORT=19401
  NODE_ID=e2e-kvm
  GUEST_SUBNET=10.236.0.0/16
  # shellcheck source=scripts/kvm-lib.sh
  source "$ROOT/scripts/kvm-lib.sh"
  # Extra node-agent flags, to try the test itself: --stop-grace=1ms makes every stop a kill,
  # and this script must then fail.
  # shellcheck disable=SC2206
  AGENT_ARGS=(${ASP_E2E_AGENT_ARGS:-})
  kvm_check_requirements
  trap kvm_cleanup EXIT
  kvm_prepare
  kvm_start_cp
  echo "==> start the node"
  kvm_start_agent
  # A workspace owned by uid 1000, the usual owner of a project (the guest has a user for it).
  mkdir -p "$WORK/ws/default/owned"
  chown 1000:1000 "$WORK/ws/default/owned"
  chmod 755 "$WORK/ws/default/owned"
  echo host-file >"$WORK/ws/default/owned/host.txt"
  chown 1000:1000 "$WORK/ws/default/owned/host.txt"
  WORKSPACE="$WORK/ws/default/owned"
fi

# status_field <field>: of the sandbox, from `asp sandbox get --json` (the session file is gone
# once the sandbox is removed, the sandbox is not).
ID=
status_field() {
  asp sandbox get "$ID" --json 2>/dev/null | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get(sys.argv[1], ""))' "$1"
}
wait_status() { # <state> [timeout_s]
  local i
  for ((i = 0; i < ${2:-90} * 2; i++)); do
    [[ "$(status_field state)" == "$1" ]] && return 0
    sleep 0.5
  done
  fail "the sandbox never reached $1 (it is $(status_field state): $(status_field status_detail))"
}

echo "==> start a sandbox with a workspace"
start_args=(--name "$NAME" --timeout 240s)
[[ -n "$WORKSPACE" ]] && start_args+=(--workspace "$WORKSPACE")
asp session start "${start_args[@]}" >/dev/null || fail "the sandbox did not start"
ID=$(sid "$NAME")
want "it runs" '^running$' "$(status_field state)"
want "booted once" '^1$' "$(status_field boot_count)"

echo "==> commands run as the owner of the workspace, and as root when asked"
if [[ -n "$WORKSPACE" ]]; then
  want "the owner of /workspace" '^1000$' "$(gx "$NAME" 'id -u')"
  want "the workspace is shared" 'host-file' "$(gx "$NAME" 'cat /workspace/host.txt')"
fi
want "root, when asked" '^0$' "$(asp session exec --name "$NAME" --buffered --root --cmd 'id -u' 2>&1)"

echo "==> write what nobody syncs"
gx "$NAME" 'sh -c "echo unsynced > /var/tmp/unsynced; dd if=/dev/urandom of=/var/tmp/blob bs=1M count=8 2>/dev/null; sha256sum /var/tmp/blob > /var/tmp/blob.sum"' >/dev/null
[[ -n "$WORKSPACE" ]] && gx "$NAME" 'sh -c "echo from-the-guest > /workspace/guest.txt"' >/dev/null

echo "==> stop"
asp session stop --name "$NAME" >/dev/null || fail "stop"
wait_status stopped 90
if (( ! EXISTING )); then
  disk="$WORK/disks/rootfs-$ID.img"
  [[ -f "$disk" ]] || fail "the stop did not keep the disk ($disk)"
  # A graceful stop is a shutdown: the filesystem is left clean, not to be recovered.
  want "the filesystem was left clean" 'clean' "$(dumpe2fs -h "$disk" 2>/dev/null | grep -i 'Filesystem state')"
  e2fsck -fn "$disk" >/dev/null 2>&1 || fail "e2fsck finds errors in the stopped disk"
  echo "  ok   e2fsck finds nothing wrong"
  want "the disk is root's again" '^0$' "$(owner_uid "$disk")"
  [[ -n "$WORKSPACE" ]] && want "what the guest wrote to the workspace reached the host" 'from-the-guest' "$(cat "$WORKSPACE/guest.txt")"
  ip link show "asp-${ID:0:8}" >/dev/null 2>&1 && fail "the TAP of the stopped sandbox is still there"
  systemctl is-active --quiet "asp-vm-$ID.service" && fail "the unit of the stopped sandbox is still active"
fi

echo "==> resume"
asp session resume --name "$NAME" >/dev/null || fail "resume"
wait_status running 120
want "booted twice" '^2$' "$(status_field boot_count)"
want "the unsynced data is there" 'unsynced' "$(gx "$NAME" 'cat /var/tmp/unsynced')"
want "and the 8 MiB written without a sync is intact" 'blob: OK' "$(gx "$NAME" 'sha256sum -c /var/tmp/blob.sum')"
[[ -n "$WORKSPACE" ]] && want "the workspace is still shared" 'from-the-guest' "$(gx "$NAME" 'cat /workspace/guest.txt')"
want "still the owner" '^1000$|^0$|^[0-9]+$' "$(gx "$NAME" 'id -u')"

echo "==> delete"
asp session rm --name "$NAME" >/dev/null || fail "rm"
wait_status deleted 90
if (( ! EXISTING )); then
  want "no disk" '^gone$' "$(owner_uid "$WORK/disks/rootfs-$ID.img")"
  want "no directory of the VM" '^gone$' "$(owner_uid "$VMDIR/$ID")"
  ip link show "asp-${ID:0:8}" >/dev/null 2>&1 && fail "the TAP of the deleted sandbox is still there"
  systemctl is-active --quiet "asp-vm-$ID.service" && fail "the unit of the deleted sandbox is still active"
  systemctl is-active --quiet "asp-vm-$ID-fs.service" && fail "the virtiofsd unit of the deleted sandbox is still active"
  left=$(find "$WORK/run" "$WORK/disks" -mindepth 1 \( -name "*$ID*" -o -name "*${ID:0:8}*" \) 2>/dev/null | head -3)
  [[ -z "$left" ]] || fail "the node left behind: $left"
  echo "  ok   nothing of the sandbox is left on the node"
fi
echo "OK e2e-kvm"
