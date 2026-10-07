# shellcheck shell=bash
# A throwaway control plane and node-agent on this KVM host, for the scripts that boot real
# VMs (smoke-vmm-user-kvm.sh, e2e-kvm.sh). Source it after smoke-lib.sh, with these set:
#
#   ROOT                 the repository
#   BIN                  directory with the linux binaries api, node-agent and asp
#   KERNEL, ROOTFS      the guest kernel and base image
#   CH                   the cloud-hypervisor binary (name or path)
#   WORK                 a directory of its own, removed at the end
#   CP_PORT, AGENT_PORT  loopback ports
#   NODE_ID              the node's id; GUEST_SUBNET its guest network (a /16 no one else uses)
#
# Nothing it starts is bound to the real node-agent of the host: it has its own directories,
# ports, node id and guest subnet, and it stops only the units of the sandboxes it started.
# Then: set the EXIT trap to kvm_cleanup, call kvm_prepare, kvm_start_cp, kvm_start_agent.

CP="http://127.0.0.1:$CP_PORT"
VMDIR="$WORK/run-vm" # --vm-run-dir default: --ch-socket-dir with -vm appended
UID_BASE=1879048192  # --vm-uid-base default (0x70000000)
pids=()
AGENT_PID=
AGENT_LOG="$WORK/agent.log"
AGENT_ARGS=()        # extra node-agent flags every start gets

fail() { echo "FAIL: $*" >&2; exit 1; }

# asp runs the CLI against the control plane. Its stdin is /dev/null unless ASP_STDIN names a
# file: a buffered exec reads stdin until EOF and hangs on an open terminal.
asp() {
  env HOME="$WORK/home" ASP_CONTROL_PLANE_URL="$CP" ASP_SESSION_DIR="$WORK/sessions" \
    ASP_REQUIRE_TOKEN= ASP_IDP_REQUIRED= ASP_API_KEY= ASP_ID_TOKEN= "$BIN/asp" "$@" <"${ASP_STDIN:-/dev/null}"
}
gx() { asp session exec --name "$1" --buffered --cmd "$2" 2>&1; } # run a command in a guest
sid() { json_field sandbox_id <"$WORK/sessions/$1.json"; }
want() { # label pattern answer
  if grep -qE -- "$2" <<<"$3"; then
    echo "  ok   $1 -> $(head -c 70 <<<"$3" | tr '\n' ' ')"
  else
    fail "$1: wanted /$2/, got: $3"
  fi
}
vmm_pid() { systemctl show -p MainPID --value "asp-vm-$1.service"; }
status_of() { grep -E "^$2:" "/proc/$1/status" | cut -f2- | tr -s '\t' ' '; }
owner_uid() { stat -c %u "$1" 2>/dev/null || echo gone; }

# kvm_check_requirements: root, KVM and what the scripts run.
kvm_check_requirements() {
  [[ $EUID -eq 0 ]] || { echo "needs root (systemd units, TAPs, chown)" >&2; exit 2; }
  local f c
  for f in "$BIN/api" "$BIN/node-agent" "$BIN/asp" "$KERNEL" "$ROOTFS" /dev/kvm; do
    [[ -e "$f" ]] || { echo "missing: $f" >&2; exit 2; }
  done
  for c in ip python3 "$CH" curl setpriv systemd-run systemctl virtiofsd; do
    command -v "$c" >/dev/null || { echo "missing command: $c" >&2; exit 2; }
  done
}

# kvm_prepare: an empty $WORK with a workspace root (and one project in it, 777).
kvm_prepare() {
  rm -rf "$WORK"
  mkdir -p "$WORK"/{run,disks,keys,home,sessions,hv} "$WORK/ws/default/proj"
  chmod 755 "$WORK" "$WORK/ws" "$WORK/ws/default"
  chmod 777 "$WORK/ws/default/proj"
  echo host-file >"$WORK/ws/default/proj/host.txt"
}

# kvm_start_cp: a control plane that asks for no credential, with its keys in $WORK.
kvm_start_cp() {
  export ASP_ATTEST_KEY="$WORK/keys/attest.pem"
  env ASP_LISTEN_ADDR="127.0.0.1:$CP_PORT" ASP_INSECURE_OPEN_API=1 ASP_AUTO_PROVISION=0 \
    ASP_AGENT_TOKEN_FILE="$WORK/agent.token" ASP_WORKSPACE_ROOTS="$WORK/ws" \
    ASP_CA_CERT="$WORK/keys/ca.crt" ASP_CA_KEY="$WORK/keys/ca.key" ASP_OIDC_KEY="$WORK/keys/oidc.pem" \
    "$BIN/api" >"$WORK/cp.log" 2>&1 &
  pids+=($!)
  local _
  for _ in $(seq 1 50); do curl -fsS "$CP/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
  curl -fsS "$CP/healthz" >/dev/null || fail "the control plane did not start: $(tail -5 "$WORK/cp.log")"
}

# kvm_start_agent [flags]: the node-agent, with its VMs confined and unprivileged.
kvm_start_agent() {
  "$BIN/node-agent" --control-plane-url="$CP" --node-id="$NODE_ID" \
    --ch-binary="$(command -v "$CH")" --ch-socket-dir="$WORK/run" --disk-dir="$WORK/disks" \
    --guest-kernel="$KERNEL" --guest-rootfs="$ROOTFS" --guest-ready-timeout=120s \
    --agent-listen="127.0.0.1:$AGENT_PORT" --agent-token-file="$WORK/agent.token" \
    --reconcile --reconcile-interval=2s --tap-auto --host-vsock --host-vsock-dir="$WORK/hv" \
    --workspace-root="$WORK/ws" --guest-subnet="$GUEST_SUBNET" --reap-leftovers=on --stop-grace=10s \
    --vm-confine=on --vm-unprivileged=on "${AGENT_ARGS[@]}" "$@" >>"$AGENT_LOG" 2>&1 &
  AGENT_PID=$!
  # The node is schedulable from the heartbeat of the agent before this one: wait for
  # this one to have adopted what it finds and registered.
  local _
  for _ in $(seq 1 300); do
    grep -q "reconciler started" "$AGENT_LOG" 2>/dev/null && break
    kill -0 "$AGENT_PID" 2>/dev/null || fail "the node-agent exited: $(tail -5 "$AGENT_LOG")"
    sleep 0.2
  done
  grep -q "reconciler started" "$AGENT_LOG" || fail "the node never registered: $(tail -5 "$AGENT_LOG")"
  wait_node_schedulable "$CP" "$NODE_ID" 60 || fail "the node is not schedulable: $(tail -5 "$AGENT_LOG")"
}

# The VMs outlive the agent, so killing it is how a restart is simulated.
kvm_kill_agent() {
  [[ -n "$AGENT_PID" ]] && kill "$AGENT_PID" 2>/dev/null || true
  [[ -n "$AGENT_PID" ]] && wait "$AGENT_PID" 2>/dev/null || true
  AGENT_PID=
}

# kvm_cleanup: the EXIT trap. On a failure it prints the agent's log; with ASP_SMOKE_KEEP or
# ASP_E2E_KEEP set it then leaves everything in place to look at.
kvm_cleanup() {
  local rc=$?
  set +e
  if [[ $rc -ne 0 ]]; then
    echo "--- node-agent log (tail)" >&2
    tail -40 "$AGENT_LOG" 2>/dev/null >&2
  fi
  if [[ ( -n "${ASP_SMOKE_KEEP:-}" || -n "${ASP_E2E_KEEP:-}" ) && $rc -ne 0 ]]; then
    echo "KEEP: leaving the node, the control plane and $WORK in place (agent pid ${AGENT_PID:-none}, control plane pid ${pids[*]:-none})" >&2
    exit $rc
  fi
  kvm_kill_agent
  # Only the sandboxes this script started: the host's slice is shared.
  local f id p
  for f in "$WORK"/run/state/*.json; do
    [[ -e "$f" ]] || continue
    id=$(basename "$f" .json)
    systemctl stop "asp-vm-$id.service" "asp-vm-$id-fs.service" 2>/dev/null
  done
  "$BIN/node-agent" --reap-only --ch-socket-dir="$WORK/run" --disk-dir="$WORK/disks" >/dev/null 2>&1
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null; done
  rm -rf "$WORK"
  exit $rc
}
