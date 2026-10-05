#!/usr/bin/env bash
# Dry-run reconciler: Create stays requested → node claims → FakeVMM start → running → destroy → stopped.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=smoke-lib.sh
source "$ROOT/scripts/smoke-lib.sh"
WORKDIR="${TMPDIR:-/tmp}/asp-smoke-rec-$$"
mkdir -p "$WORKDIR"
CERT_DIR="$WORKDIR/certs"
CP_LOG="$WORKDIR/cp.log"
NA_LOG="$WORKDIR/na.log"
TOKEN="smoke-bootstrap-token"
export ASP_NODE_BOOTSTRAP_TOKEN="$TOKEN"
export ASP_CA_CERT="$WORKDIR/ca.crt"
export ASP_CA_KEY="$WORKDIR/ca.key"
# Real reconciler path: no sync stub provisioner.
export ASP_AUTO_PROVISION=0

cleanup() {
  [[ -n "${NA_PID:-}" ]] && kill "$NA_PID" 2>/dev/null || true
  [[ -n "${CP_PID:-}" ]] && kill "$CP_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

echo "==> build"
(cd "$ROOT/control-plane" && go build -o "$WORKDIR/api" ./cmd/api)
(cd "$ROOT/node-agent" && go build -o "$WORKDIR/node-agent" ./cmd/node-agent)

echo "==> start control-plane (memory, auto_provision=0)"
LISTEN_ADDR=127.0.0.1:18082 ASP_AUTO_PROVISION=0 "$WORKDIR/api" >"$CP_LOG" 2>&1 &
CP_PID=$!
for i in $(seq 1 50); do
  curl -sf http://127.0.0.1:18082/healthz >/dev/null && break
  sleep 0.1
done
curl -sf http://127.0.0.1:18082/healthz | grep -q ok

echo "==> start node-agent (enroll + dry-run + reconcile)"
"$WORKDIR/node-agent" \
  --control-plane-url=http://127.0.0.1:18082 \
  --node-id=smoke-rec-node \
  --dry-run \
  --reconcile \
  --reconcile-interval=500ms \
  --enroll \
  --bootstrap-token="$TOKEN" \
  --cert-dir="$CERT_DIR" \
  --agent-listen=127.0.0.1:19102 \
  --heartbeat-interval=1h \
  >"$NA_LOG" 2>&1 &
NA_PID=$!
for i in $(seq 1 50); do
  curl -sf http://127.0.0.1:19102/healthz >/dev/null && break
  sleep 0.1
done
curl -sf http://127.0.0.1:19102/healthz | grep -q ok
wait_node_schedulable http://127.0.0.1:18082 smoke-rec-node

echo "==> create sandbox (expect requested)"
SB=$(curl -sf -X POST http://127.0.0.1:18082/v1/sandboxes \
  -H 'Content-Type: application/json' \
  -d '{"tenant_id":"smoke","image_ref":"debian:bookworm","cpu_millis":500,"memory_mib":256}')
echo "$SB"
echo "$SB" | grep -q '"state":"requested"'
SID=$(echo "$SB" | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')

echo "==> wait for reconciler → running"
OK=0
for i in $(seq 1 40); do
  GOT=$(curl -sf "http://127.0.0.1:18082/v1/sandboxes/${SID}")
  echo "$GOT" | grep -q '"state":"running"' && { OK=1; break; }
  sleep 0.25
done
[[ "$OK" == "1" ]] || { echo "timeout waiting running"; cat "$NA_LOG"; cat "$CP_LOG"; exit 1; }
echo "$GOT" | grep -q smoke-rec-node

echo "==> destroy → stopping → wait stopped"
curl -sf -X DELETE "http://127.0.0.1:18082/v1/sandboxes/${SID}" | grep -q stopping
OK=0
for i in $(seq 1 40); do
  GOT=$(curl -sf "http://127.0.0.1:18082/v1/sandboxes/${SID}")
  echo "$GOT" | grep -q '"state":"stopped"' && { OK=1; break; }
  sleep 0.25
done
[[ "$OK" == "1" ]] || { echo "timeout waiting stopped"; echo "$GOT"; cat "$NA_LOG"; exit 1; }

echo "==> events present"
curl -sf "http://127.0.0.1:18082/v1/sandboxes/${SID}/events" | grep -q sandbox.claimed

echo "OK smoke-reconcile"
