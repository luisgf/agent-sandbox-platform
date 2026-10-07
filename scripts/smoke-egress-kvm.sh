#!/usr/bin/env bash
# Egress enforcement on a real KVM host: a guest cannot reach the network except
# through the node's egress proxy, with or without HTTP_PROXY.
#
# Needs root, /dev/kvm, cloud-hypervisor, nftables, python3, and a guest kernel and
# rootfs (the guest needs bash and iproute2: the images/guest one has them).
#
# It touches nothing on the host's own network: the control plane, the node-agent and
# the sandbox's TAP live in a network namespace of their own (asp-egress-smoke), and
# the "internet" is a second namespace behind a veth pair:
#
#   guest 10.233.x.2 -- TAP -- [ns A: node-agent, nft asp_egress, proxy :8888, DNS sink]
#                                      | 10.99.0.1 --veth-- 10.99.0.2 [ns B: origin]
#
# ns B serves a web server on :80 (allowed.test) and a plain TCP service on :22222.
# Phase 1 runs the node as shipped (the proxy is set; nothing else): the redirect and
# enforce mode must be on, and the guest must reach allowed.test only through the
# proxy, get 403 for anything else, and be unable to reach :22222 at all. Phase 2
# turns the redirect off (--egress-nft-redirect=false) and shows the same probes
# bypass the proxy, so the test can tell the difference.
#
# Usage:
#   sudo ASP_SMOKE_ROOTFS=/path/rootfs.img [ASP_SMOKE_KERNEL=/opt/sandbox/vmlinux] \
#        [ASP_SMOKE_BIN=build] ./scripts/smoke-egress-kvm.sh
# ASP_SMOKE_BIN holds the api, node-agent and asp binaries (linux).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${ASP_SMOKE_BIN:-$ROOT/build}"
KERNEL="${ASP_SMOKE_KERNEL:-/opt/sandbox/vmlinux}"
ROOTFS="${ASP_SMOKE_ROOTFS:?set ASP_SMOKE_ROOTFS to the guest rootfs image}"
CH="${ASP_SMOKE_CH:-cloud-hypervisor}"
WORK="${ASP_SMOKE_DIR:-/var/tmp/asp-smoke-egress}"
NS=asp-egress-smoke
NSB=asp-egress-origin
CP_PORT=18299
CP="http://127.0.0.1:$CP_PORT"

[[ $EUID -eq 0 ]] || { echo "needs root (network namespaces, TAPs, nftables)" >&2; exit 2; }
for f in "$BIN/api" "$BIN/node-agent" "$BIN/asp" "$KERNEL" "$ROOTFS" /dev/kvm; do
  [[ -e "$f" ]] || { echo "missing: $f" >&2; exit 2; }
done
for c in ip nft python3 "$CH" curl bash; do
  command -v "$c" >/dev/null || { echo "missing command: $c" >&2; exit 2; }
done
# shellcheck source=scripts/smoke-lib.sh
source "$ROOT/scripts/smoke-lib.sh"

pids=()
fail() { echo "FAIL: $*" >&2; exit 1; }
in_a() { ip netns exec "$NS" "$@"; }
in_b() { ip netns exec "$NSB" "$@"; }
# The control plane is in namespace A: the helpers of smoke-lib.sh call curl.
curl() { ip netns exec "$NS" curl "$@"; }
# asp runs the CLI against the control plane. Its stdin is /dev/null unless ASP_STDIN
# names a file: a buffered exec reads stdin until EOF and hangs on an open terminal.
asp() {
  in_a env HOME="$WORK/home" ASP_CP_URL="$CP" ASP_SESSION_DIR="$WORK/sessions" \
    ASP_IDP_REQUIRED= ASP_API_KEY= ASP_ID_TOKEN= "$BIN/asp" "$@" <"${ASP_STDIN:-/dev/null}"
}

stop_agent() {
  [[ -n "${AGENT_PID:-}" ]] && kill "$AGENT_PID" 2>/dev/null || true
  [[ -n "${AGENT_PID:-}" ]] && wait "$AGENT_PID" 2>/dev/null || true
  AGENT_PID=
  # whatever the agent left (cloud-hypervisor, TAPs, sockets)
  in_a "$BIN/node-agent" --reap-only --ch-socket-dir="$WORK/run" >/dev/null 2>&1 || true
}

cleanup() {
  local rc=$?
  set +e
  if [[ $rc -ne 0 ]]; then
    echo "--- node-agent log (tail)" >&2
    tail -40 "${AGENT_LOG:-/dev/null}" 2>/dev/null >&2
    echo "--- nft ruleset in the node's namespace" >&2
    ip netns exec "$NS" nft list ruleset 2>&1 | head -60 >&2
  fi
  if [[ -n "${ASP_SMOKE_KEEP:-}" && $rc -ne 0 ]]; then
    echo "ASP_SMOKE_KEEP: leaving $NS, $NSB and $WORK in place (stop the agent by hand; ip netns del to clean up)" >&2
    exit $rc
  fi
  stop_agent
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null; done
  ip netns del "$NS" 2>/dev/null
  ip netns del "$NSB" 2>/dev/null
  rm -rf "/etc/netns/$NS" "$WORK"
  exit $rc
}
trap cleanup EXIT

# --- the two namespaces and the veth between them
ip netns del "$NS" 2>/dev/null || true
ip netns del "$NSB" 2>/dev/null || true
rm -rf "$WORK"
mkdir -p "$WORK"/{run,disks,ws,keys,home,sessions} "/etc/netns/$NS"
printf '127.0.0.1 localhost\n10.99.0.2 allowed.test\n' >"/etc/netns/$NS/hosts"
ip netns add "$NS"
ip netns add "$NSB"
ip link add vA type veth peer name vB
ip link set vA netns "$NS"
ip link set vB netns "$NSB"
ip -n "$NS" addr add 10.99.0.1/30 dev vA
ip -n "$NS" link set vA up
ip -n "$NS" link set lo up
ip -n "$NSB" addr add 10.99.0.2/30 dev vB
ip -n "$NSB" link set vB up
ip -n "$NSB" link set lo up
# Guest packets that the node would forward (it must not) have somewhere to go.
ip -n "$NS" route add default via 10.99.0.2
ip -n "$NSB" route add 10.233.0.0/16 via 10.99.0.1
in_a sysctl -qw net.ipv4.ip_forward=1
in_b sysctl -qw net.ipv4.ip_forward=0

# --- the origin: a web server on :80 and a plain TCP service on :22222
cat >"$WORK/origin.py" <<'PY'
import socket, threading
from http.server import BaseHTTPRequestHandler, HTTPServer

class H(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(b"origin-ok")
    def log_message(self, *a): pass

def raw():
    s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("10.99.0.2", 22222)); s.listen(8)
    while True:
        c, _ = s.accept(); c.sendall(b"bypass-open\n"); c.close()

threading.Thread(target=raw, daemon=True).start()
HTTPServer(("10.99.0.2", 80), H).serve_forever()
PY
# (Background jobs call ip directly: $! must be the process itself, not a subshell.)
ip netns exec "$NSB" python3 "$WORK/origin.py" &
pids+=($!)

# --- the control plane
export ASP_ATTEST_KEY="$WORK/keys/attest.pem"
ip netns exec "$NS" env LISTEN_ADDR="127.0.0.1:$CP_PORT" ASP_INSECURE_OPEN_API=1 ASP_AUTO_PROVISION=0 \
  ASP_AGENT_TOKEN_FILE="$WORK/agent.token" ASP_WORKSPACE_ROOTS="$WORK/ws" \
  ASP_CA_CERT="$WORK/keys/ca.crt" ASP_CA_KEY="$WORK/keys/ca.key" ASP_OIDC_KEY="$WORK/keys/oidc.pem" \
  ASP_EGRESS_DENY_DEFAULT=1 "$BIN/api" >"$WORK/cp.log" 2>&1 &
pids+=($!)
for _ in $(seq 1 50); do in_a curl -fsS "$CP/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
in_a curl -fsS "$CP/healthz" >/dev/null || fail "the control plane did not start: $(tail -5 "$WORK/cp.log")"

# allowed.test is the only name the tenant may reach
in_a curl -fsS -X PUT -H 'Content-Type: application/json' \
  -d '{"rules":[{"host_pattern":"allowed.test","enabled":true}]}' \
  "$CP/v1/tenants/default/egress" >/dev/null || fail "could not set the tenant egress policy"

AGENT_LOG="$WORK/agent.log"
start_agent() { # flags...
  ip netns exec "$NS" "$BIN/node-agent" --control-plane-url="$CP" --node-id=smoke-egress \
    --ch-binary="$(command -v "$CH")" --ch-socket-dir="$WORK/run" --disk-dir="$WORK/disks" \
    --guest-kernel="$KERNEL" --guest-rootfs="$ROOTFS" --guest-ready-timeout=120s \
    --agent-listen=127.0.0.1:19101 --agent-token-file="$WORK/agent.token" \
    --reconcile --reconcile-interval=2s --tap-auto --vm-confine=off --reap-leftovers=off \
    --workspace-root="$WORK/ws" --guest-subnet=10.233.0.0/16 \
    --egress-enforce --egress-proxy-listen=:8888 --egress-dns-sink=:5353 \
    --egress-allow-cidr=10.99.0.0/30 "$@" >>"$AGENT_LOG" 2>&1 &
  AGENT_PID=$!
}

# --- what the guest runs: one HTTP/1.0 GET over /dev/tcp (the guest has no curl)
cat >"$WORK/probe.sh" <<'SH'
#!/bin/bash
# probe.sh DIALHOST PORT HOSTHEADER [abs]  -> the status line and body, or ERR <why>
dial=$1; port=$2; hosth=$3; abs=${4:-}
exec 3<>"/dev/tcp/$dial/$port" 2>/dev/null || { echo "ERR connect"; exit 3; }
if [ -n "$abs" ]; then
  printf 'GET http://%s/ HTTP/1.0\r\nHost: %s\r\n\r\n' "$hosth" "$hosth" >&3
else
  printf 'GET / HTTP/1.0\r\nHost: %s\r\n\r\n' "$hosth" >&3
fi
timeout 5 cat <&3 | tr -d '\r' | awk 'NR==1{print; next} /^$/{b=1; next} b{print}'
SH

probe() { # dial port host [abs] -> the guest's answer, "ERR timeout" when it hangs
  local out
  out=$(asp session exec --name egress --buffered --cmd "timeout 10 bash /tmp/probe.sh $*" 2>&1) || true
  [[ -n "$out" ]] && echo "$out" || echo "ERR timeout"
}

want() { # label pattern answer
  if grep -qE "$2" <<<"$3"; then
    echo "  ok   $1 -> $(head -c 60 <<<"$3" | tr '\n' ' ')"
  else
    fail "$1: wanted /$2/, got: $3"
  fi
}

run_phase() { # name redirect-flags...
  local name=$1; shift
  echo "==> phase: $name (node-agent $*)"
  : >"$AGENT_LOG"
  start_agent "$@"
  wait_node_schedulable "$CP" smoke-egress 60 || fail "the node never registered"
  local row
  row=$(asp node list | grep smoke-egress || true)
  echo "  node: $row"

  asp session start --name egress --timeout 180s >/dev/null || fail "the sandbox did not start"
  ASP_STDIN="$WORK/probe.sh" asp session exec --name egress --buffered --cmd "tee /tmp/probe.sh" >/dev/null || fail "could not upload the probe"
  local gw
  gw=$(asp session exec --name egress --buffered --cmd "ip -o route show default" | awk '{print $3}')
  [[ -n "$gw" ]] || fail "the guest has no default route"
  echo "  guest gateway: $gw"

  case "$name" in
    enforced)
      grep -q "enforced" <<<"$row" || fail "asp node list does not show egress enforced: $row"
      want "via the proxy, an allowed host" '200' "$(probe "$gw" 8888 allowed.test abs)"
      want "via the proxy, the body" 'origin-ok' "$(probe "$gw" 8888 allowed.test abs)"
      want "via the proxy, a denied host" '403' "$(probe "$gw" 8888 denied.test abs)"
      want "no proxy, port 80 to anywhere: redirected, denied" '403' "$(probe 192.0.2.10 80 denied.test)"
      want "no proxy, port 80: redirected, allowed" 'origin-ok' "$(probe 10.99.0.2 80 allowed.test)"
      want "no proxy, a service on another port: unreachable" '^ERR' "$(probe 10.99.0.2 22222 x)"
      ;;
    open)
      grep -qE "[[:space:]]off[[:space:]]" <<<"$row" || fail "asp node list should show egress off: $row"
      want "via the proxy, an allowed host" 'origin-ok' "$(probe "$gw" 8888 allowed.test abs)"
      want "no proxy, port 80 to anywhere is NOT redirected" '^ERR' "$(probe 192.0.2.10 80 denied.test)"
      want "no proxy, a service on another port is reachable" 'bypass-open' "$(probe 10.99.0.2 22222 x)"
      ;;
  esac
  local sb
  sb=$(json_field sandbox_id <"$WORK/sessions/egress.json")
  asp session rm --name egress >/dev/null || true
  wait_state "$CP" "$sb" deleted 60 || fail "the sandbox was not deleted"
  stop_agent
  # The rules outlive the agent that applied them; the next phase starts clean.
  in_a nft delete table ip asp_egress 2>/dev/null || true
  in_a nft delete table ip6 asp_egress 2>/dev/null || true
}

run_phase enforced
run_phase open --egress-nft-redirect=false
echo "OK smoke-egress-kvm"
