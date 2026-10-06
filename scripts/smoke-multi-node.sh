#!/usr/bin/env bash
# Two dry-run nodes behind one control plane (ADR-0011): spread placement,
# cordon, 503 with reasons when full, uncordon, 409 for an unknown pin, a node
# frozen like a partition (its sandboxes fail as node_lost, and once back it
# stops them because they left its assigned set) and an agent restart (its
# running sandboxes fail as node_agent_restarted). Liveness thresholds are seconds here.
# Each node offers 2 sandbox slots. Honours DATABASE_URL (node ids are unique
# per run so leftover rows do not count as usage).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=smoke-lib.sh
source "$ROOT/scripts/smoke-lib.sh"
WORKDIR="${TMPDIR:-/tmp}/asp-smoke-multi-$$"
mkdir -p "$WORKDIR"
CP=http://127.0.0.1:18090
NODE_A="smoke-a-$$"
NODE_B="smoke-b-$$"
TOKEN="smoke-multi-token"
export ASP_NODE_BOOTSTRAP_TOKEN="$TOKEN"
export ASP_CA_CERT="$WORKDIR/ca.crt"
export ASP_CA_KEY="$WORKDIR/ca.key"
export ASP_OIDC_KEY="$WORKDIR/oidc.pem"
# No mTLS in this dry-run lab: the control plane and both nodes share the
# attestation key, because the control plane only trusts configured keys.
export ASP_ATTEST_KEY="$WORKDIR/attest.pem"
export ASP_AUTO_PROVISION=0
# With DATABASE_URL the control plane runs in production mode; the smoke keeps
# its keys in WORKDIR on purpose.
export ASP_ALLOW_TMP_KEYS=1
export ASP_NODE_STALE_AFTER=3s ASP_NODE_FAILOVER_AFTER=4s ASP_NODE_MONITOR_INTERVAL=1s

cleanup() {
  for pid in "${NA_A:-}" "${NA_B:-}" "${CP_PID:-}"; do
    [[ -n "$pid" ]] && { kill -CONT "$pid"; kill "$pid"; } 2>/dev/null || true
  done
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  echo "--- control plane log" >&2; tail -20 "$WORKDIR/cp.log" >&2 || true
  echo "--- node A log" >&2; tail -10 "$WORKDIR/na-a.log" >&2 || true
  echo "--- node B log" >&2; tail -10 "$WORKDIR/na-b.log" >&2 || true
  exit 1
}

echo "==> build"
(cd "$ROOT/control-plane" && go build -o "$WORKDIR/api" ./cmd/api)
(cd "$ROOT/node-agent" && go build -o "$WORKDIR/node-agent" ./cmd/node-agent)

echo "==> start control-plane (auto_provision=0, policy spread)"
LISTEN_ADDR=127.0.0.1:18090 "$WORKDIR/api" >"$WORKDIR/cp.log" 2>&1 &
CP_PID=$!
for _ in $(seq 1 50); do curl -sf "$CP/healthz" >/dev/null && break; sleep 0.1; done
curl -sf "$CP/healthz" | grep -q ok || fail "control plane did not start"

start_node() { # <node-id> <agent-port> <log>
  ASP_LOCAL_NET_KEY_DIR="$WORKDIR/ln-$1" \
  "$WORKDIR/node-agent" \
    --control-plane-url="$CP" \
    --node-id="$1" \
    --dry-run --reconcile --reconcile-interval=300ms \
    --enroll --bootstrap-token="$TOKEN" \
    --cert-dir="$WORKDIR/certs-$1" \
    --agent-listen="127.0.0.1:$2" \
    --heartbeat-interval=1s \
    --capacity-cpu=4 --capacity-mem-mib=8192 --max-sandboxes=2 \
    --ch-socket-dir="$WORKDIR/ch-$1" \
    >"$3" 2>&1 &
}

echo "==> start two dry-run nodes ($NODE_A, $NODE_B), 2 slots each"
start_node "$NODE_A" 19110 "$WORKDIR/na-a.log"
NA_A=$!
start_node "$NODE_B" 19111 "$WORKDIR/na-b.log"
NA_B=$!
wait_node_schedulable "$CP" "$NODE_A" || fail "node A not schedulable"
wait_node_schedulable "$CP" "$NODE_B" || fail "node B not schedulable"

# create [node_id] → prints "<http_code> <sandbox_id|error body>"
create() {
  local body='{"tenant_id":"smoke","image_ref":"debian:bookworm","cpu_millis":1000,"memory_mib":512'
  [[ -n "${1:-}" ]] && body+=",\"node_id\":\"$1\""
  body+='}'
  local out code
  out=$(curl -sS -w '\n%{http_code}' -X POST "$CP/v1/sandboxes" -H 'Content-Type: application/json' -d "$body")
  code=$(tail -n1 <<<"$out")
  out=$(sed '$d' <<<"$out")
  if [[ "$code" == 201 ]]; then
    echo "$code $(json_field id <<<"$out")"
  else
    echo "$code $out"
  fi
}
node_of() { curl -sf "$CP/v1/sandboxes/$1" | json_field node_id; }
wait_running() { # <sandbox_id>
  for _ in $(seq 1 60); do
    [[ "$(curl -sf "$CP/v1/sandboxes/$1" | json_field state)" == running ]] && return 0
    sleep 0.25
  done
  fail "sandbox $1 never reached running"
}

echo "==> 1. spread: two sandboxes land on different nodes and run"
read -r c1 s1 <<<"$(create)"; [[ "$c1" == 201 ]] || fail "s1: $c1 $s1"
read -r c2 s2 <<<"$(create)"; [[ "$c2" == 201 ]] || fail "s2: $c2 $s2"
n1=$(node_of "$s1"); n2=$(node_of "$s2")
[[ "$n1" != "$n2" ]] || fail "spread put both sandboxes on $n1"
wait_running "$s1"; wait_running "$s2"
echo "    s1 → $n1, s2 → $n2"
for s in "$s1" "$s2"; do
  wait_fresh_attestation "$CP" "$s" || fail "no fresh boot attestation for $s"
done

echo "==> 2. cordon $NODE_A: the next sandbox goes to $NODE_B"
curl -sf -X POST "$CP/v1/nodes/$NODE_A/cordon" | grep -q '"unschedulable_reason":"cordoned"' || fail "cordon"
read -r c3 s3 <<<"$(create)"; [[ "$c3" == 201 ]] || fail "s3: $c3 $s3"
[[ "$(node_of "$s3")" == "$NODE_B" ]] || fail "s3 went to $(node_of "$s3"), want $NODE_B"
wait_running "$s3"

echo "==> 3. nothing fits: 503 with reasons"
out=$(create)
[[ "$out" == 503* ]] || fail "want 503, got $out"
grep -q cordoned <<<"$out" && grep -q max_sandboxes <<<"$out" || fail "503 reasons: $out"
echo "    ${out#503 }"

echo "==> 4. uncordon $NODE_A: room again"
curl -sf -X POST "$CP/v1/nodes/$NODE_A/uncordon" | grep -q '"schedulable":true' || fail "uncordon"
read -r c4 s4 <<<"$(create)"; [[ "$c4" == 201 ]] || fail "s4: $c4 $s4"
[[ "$(node_of "$s4")" == "$NODE_A" ]] || fail "s4 went to $(node_of "$s4"), want $NODE_A"
wait_running "$s4"

echo "==> 5. both nodes full: 503"
out=$(create)
[[ "$out" == 503* ]] || fail "want 503 when full, got $out"

echo "==> 6. a pin to an unknown node: 409"
out=$(create no-such-node)
[[ "$out" == 409* ]] || fail "want 409 for an unknown pin, got $out"

echo "==> 7. destroying a sandbox frees its slot"
curl -sf -X DELETE "$CP/v1/sandboxes/$s4" >/dev/null || fail "destroy s4"
for _ in $(seq 1 60); do
  [[ "$(curl -sf "$CP/v1/sandboxes/$s4" | json_field state)" == stopped ]] && break
  sleep 0.25
done
read -r c5 s5 <<<"$(create)"; [[ "$c5" == 201 ]] || fail "s5 after destroy: $c5 $s5"
[[ "$(node_of "$s5")" == "$NODE_A" ]] || fail "s5 went to $(node_of "$s5"), want $NODE_A"
wait_running "$s5"

field_of() { curl -sf "$CP/v1/sandboxes/$1" | json_field "$2"; }
node_state() {
  curl -sf "$CP/v1/nodes" | python3 -c 'import json,sys; print(next((n["state"] for n in json.load(sys.stdin)["nodes"] if n["id"]==sys.argv[1]), ""))' "$1"
}

echo "==> 8. freeze $NODE_B (SIGSTOP, like a partition): offline, then its sandboxes fail as node_lost"
kill -STOP "$NA_B"
for _ in $(seq 1 80); do
  [[ "$(field_of "$s2" state)" == failed && "$(field_of "$s3" state)" == failed ]] && break
  sleep 0.25
done
for s in "$s2" "$s3"; do
  [[ "$(field_of "$s" state)/$(field_of "$s" stop_reason)" == failed/node_lost ]] || fail "$s: $(field_of "$s" state)/$(field_of "$s" stop_reason)"
done
[[ "$(node_state "$NODE_B")" == offline ]] || fail "$NODE_B state: $(node_state "$NODE_B")"
out=$(create)
[[ "$out" == 503* ]] || fail "with $NODE_B lost and $NODE_A full want 503, got $out"
echo "    ${out#503 }"
echo "    thaw $NODE_B: its sandboxes left its assigned set, so it stops their VMs"
kill -CONT "$NA_B"
for _ in $(seq 1 40); do
  grep self-fencing "$WORKDIR/na-b.log" | grep -q "$s2" && grep self-fencing "$WORKDIR/na-b.log" | grep -q "$s3" && break
  sleep 0.25
done
for s in "$s2" "$s3"; do
  grep self-fencing "$WORKDIR/na-b.log" | grep -q "$s" || fail "$NODE_B did not stop $s after coming back"
done

echo "==> 9. restart the agent of $NODE_A: its running sandboxes fail as node_agent_restarted"
{ kill -9 "$NA_A" && wait "$NA_A"; } 2>/dev/null || true
start_node "$NODE_A" 19110 "$WORKDIR/na-a2.log"
NA_A=$!
for _ in $(seq 1 80); do
  [[ "$(field_of "$s1" stop_reason)" == node_agent_restarted && "$(field_of "$s5" stop_reason)" == node_agent_restarted ]] && break
  sleep 0.25
done
for s in "$s1" "$s5"; do
  [[ "$(field_of "$s" state)/$(field_of "$s" stop_reason)" == failed/node_agent_restarted ]] || fail "$s: $(field_of "$s" state)/$(field_of "$s" stop_reason)"
done
wait_node_schedulable "$CP" "$NODE_A" || fail "$NODE_A not schedulable after restart"
read -r c6 s6 <<<"$(create)"; [[ "$c6" == 201 ]] || fail "s6 after restart: $c6 $s6"
[[ "$(node_of "$s6")" == "$NODE_A" ]] || fail "s6 went to $(node_of "$s6"), want $NODE_A"
wait_running "$s6"

echo "OK smoke-multi-node"
