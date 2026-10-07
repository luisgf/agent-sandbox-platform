#!/usr/bin/env bash
# End-to-end dry-run: pod-daemon (unix) → node-agent exec proxy → control-plane exec.
# Uses ASP_AUTO_PROVISION=0 + --reconcile so Create stays requested until FakeVMM start.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=smoke-lib.sh
source "$ROOT/scripts/smoke-lib.sh"
WORKDIR="${TMPDIR:-/tmp}/asp-smoke-$$"
mkdir -p "$WORKDIR"
# The node-agent creates this file; the control plane reads it to call the agent.
export ASP_AGENT_TOKEN_FILE="$WORKDIR/agent.token"
# Dry-run smoke: no API keys and no IdP, so the control plane is opened on purpose.
export ASP_INSECURE_OPEN_API=1
SOCK="$WORKDIR/pod-daemon.sock"
CERT_DIR="$WORKDIR/certs"
CP_LOG="$WORKDIR/cp.log"
PD_LOG="$WORKDIR/pd.log"
NA_LOG="$WORKDIR/na.log"
TOKEN="smoke-bootstrap-token"
export ASP_NODE_BOOTSTRAP_TOKEN="$TOKEN"
export ASP_CA_CERT="$WORKDIR/ca.crt"
export ASP_CA_KEY="$WORKDIR/ca.key"
export ASP_AUTO_PROVISION=0

cleanup() {
  [[ -n "${NA_PID:-}" ]] && kill "$NA_PID" 2>/dev/null || true
  [[ -n "${CP_PID:-}" ]] && kill "$CP_PID" 2>/dev/null || true
  [[ -n "${PD_PID:-}" ]] && kill "$PD_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

echo "==> build"
(cd "$ROOT/control-plane" && go build -o "$WORKDIR/api" ./cmd/api)
(cd "$ROOT/node-agent" && go build -o "$WORKDIR/node-agent" ./cmd/node-agent)
(cd "$ROOT/pod-daemon" && cargo build -q)
PD_BIN="$ROOT/pod-daemon/target/debug/pod-daemon"

echo "==> start pod-daemon on $SOCK"
"$PD_BIN" --listen unix --unix-socket "$SOCK" >"$PD_LOG" 2>&1 &
PD_PID=$!
for i in $(seq 1 50); do
  [[ -S "$SOCK" ]] && break
  sleep 0.1
done
[[ -S "$SOCK" ]] || { echo "pod-daemon sock missing"; cat "$PD_LOG"; exit 1; }

echo "==> start control-plane (memory, auto_provision=0)"
LISTEN_ADDR=127.0.0.1:18080 ASP_AUTO_PROVISION=0 "$WORKDIR/api" >"$CP_LOG" 2>&1 &
CP_PID=$!
for i in $(seq 1 50); do
  curl -sf http://127.0.0.1:18080/healthz >/dev/null && break
  sleep 0.1
done
curl -sf http://127.0.0.1:18080/healthz | grep -q ok

echo "==> start node-agent (enroll + dry-run + reconcile + pod sock)"
"$WORKDIR/node-agent" \
  --control-plane-url=http://127.0.0.1:18080 \
  --node-id=smoke-node \
  --dry-run \
  --reconcile \
  --reconcile-interval=500ms \
  --enroll \
  --bootstrap-token="$TOKEN" \
  --cert-dir="$CERT_DIR" \
  --agent-listen=127.0.0.1:19100 \
  --pod-daemon-sock="$SOCK" \
  --heartbeat-interval=1h \
  >"$NA_LOG" 2>&1 &
NA_PID=$!
for i in $(seq 1 50); do
  curl -sf http://127.0.0.1:19100/healthz >/dev/null && break
  sleep 0.1
done
curl -sf http://127.0.0.1:19100/healthz | grep -q ok
wait_node_schedulable http://127.0.0.1:18080 smoke-node
[[ -f "$CERT_DIR/client.crt" ]] || { echo "certs not written"; cat "$NA_LOG"; exit 1; }

echo "==> create sandbox pinned to smoke-node (requested until reconcile)"
SB=$(curl -sf -X POST http://127.0.0.1:18080/v1/sandboxes \
  -H 'Content-Type: application/json' \
  -d '{"tenant_id":"smoke","image_ref":"debian:bookworm","cpu_millis":500,"memory_mib":256,"node_id":"smoke-node"}')
echo "$SB" | grep -q smoke-node
echo "$SB" | grep -q '"state":"requested"'
SID=$(echo "$SB" | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')

echo "==> wait for reconciler → running"
OK=0
for i in $(seq 1 40); do
  GOT=$(curl -sf "http://127.0.0.1:18080/v1/sandboxes/${SID}")
  echo "$GOT" | grep -q '"state":"running"' && { OK=1; break; }
  sleep 0.25
done
[[ "$OK" == "1" ]] || { echo "timeout waiting running"; cat "$NA_LOG"; exit 1; }

echo "==> exec echo hello"
OUT=$(curl -sf -X POST "http://127.0.0.1:18080/v1/sandboxes/${SID}/exec" \
  -H 'Content-Type: application/json' \
  -d '{"cmd":["echo","hello"]}')
echo "$OUT"
echo "$OUT" | grep -q '"exit_code":0'
echo "$OUT" | grep -q hello

echo "==> list nodes (enrolled)"
curl -sf http://127.0.0.1:18080/v1/nodes | grep -q smoke-node
curl -sf http://127.0.0.1:18080/v1/nodes | grep -q cert_fingerprint

echo "OK smoke-enroll-exec"
