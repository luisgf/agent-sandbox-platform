#!/usr/bin/env bash
# Smoke the asp CLI against a short-lived dry-run CP + node-agent + pod-daemon.
# Mirrors smoke-enroll-exec lifecycle via `asp sandbox run`.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=smoke-lib.sh
source "$ROOT/scripts/smoke-lib.sh"
WORKDIR="${TMPDIR:-/tmp}/asp-cli-smoke-$$"
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
TOKEN="smoke-cli-bootstrap"
export ASP_NODE_BOOTSTRAP_TOKEN="$TOKEN"
export ASP_CA_CERT="$WORKDIR/ca.crt"
export ASP_CA_KEY="$WORKDIR/ca.key"
export ASP_AUTO_PROVISION=0
CP_URL="http://127.0.0.1:18081"

cleanup() {
  [[ -n "${NA_PID:-}" ]] && kill "$NA_PID" 2>/dev/null || true
  [[ -n "${CP_PID:-}" ]] && kill "$CP_PID" 2>/dev/null || true
  [[ -n "${PD_PID:-}" ]] && kill "$PD_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

echo "==> build asp + stack"
(cd "$ROOT/cli" && go build -o "$WORKDIR/asp" ./cmd/asp)
(cd "$ROOT/control-plane" && go build -o "$WORKDIR/api" ./cmd/api)
(cd "$ROOT/node-agent" && go build -o "$WORKDIR/node-agent" ./cmd/node-agent)
(cd "$ROOT/pod-daemon" && cargo build -q)
PD_BIN="$ROOT/pod-daemon/target/debug/pod-daemon"

echo "==> start pod-daemon"
"$PD_BIN" --listen unix --unix-socket "$SOCK" >"$PD_LOG" 2>&1 &
PD_PID=$!
for i in $(seq 1 50); do
  [[ -S "$SOCK" ]] && break
  sleep 0.1
done
[[ -S "$SOCK" ]] || { echo "pod-daemon sock missing"; cat "$PD_LOG"; exit 1; }

echo "==> start control-plane on 18081"
ASP_LISTEN_ADDR=127.0.0.1:18081 ASP_AUTO_PROVISION=0 "$WORKDIR/api" >"$CP_LOG" 2>&1 &
CP_PID=$!
for i in $(seq 1 50); do
  curl -sf "$CP_URL/healthz" >/dev/null && break
  sleep 0.1
done
curl -sf "$CP_URL/healthz" | grep -q ok

echo "==> start node-agent (dry-run + reconcile)"
"$WORKDIR/node-agent" \
  --control-plane-url="$CP_URL" \
  --node-id=cli-smoke-node \
  --dry-run \
  --reconcile \
  --reconcile-interval=500ms \
  --enroll \
  --bootstrap-token="$TOKEN" \
  --cert-dir="$CERT_DIR" \
  --agent-listen=127.0.0.1:19101 \
  --pod-daemon-sock="$SOCK" \
  --heartbeat-interval=1h \
  >"$NA_LOG" 2>&1 &
NA_PID=$!
for i in $(seq 1 50); do
  curl -sf http://127.0.0.1:19101/healthz >/dev/null && break
  sleep 0.1
done
curl -sf http://127.0.0.1:19101/healthz | grep -q ok
wait_node_schedulable "$CP_URL" cli-smoke-node

ASP="$WORKDIR/asp"
export ASP_CONTROL_PLANE_URL="$CP_URL"

echo "==> asp sandbox run"
OUT=$("$ASP" sandbox run --node-id=cli-smoke-node --tenant=smoke --timeout=30s --cmd 'echo hello-cli' 2>"$WORKDIR/asp.err")
echo "$OUT" | grep -q hello-cli
grep -q 'created sandbox' "$WORKDIR/asp.err"
grep -q 'destroyed sandbox' "$WORKDIR/asp.err"

echo "==> asp sandbox run --keep + delete"
KEEP_ERR=$("$ASP" sandbox run --node-id=cli-smoke-node --tenant=smoke --timeout=30s --keep --cmd 'echo keep-me' 2>&1 >/dev/null) || true
echo "$KEEP_ERR" | grep -q 'keeping sandbox'
SID=$(echo "$KEEP_ERR" | sed -n 's/.*created sandbox \([^ ]*\).*/\1/p' | head -1)
[[ -n "$SID" ]] || { echo "no sandbox id in: $KEEP_ERR"; exit 1; }
"$ASP" sandbox get "$SID" --json | grep -q '"state"'

echo "==> stop keeps the sandbox, start resumes it, delete ends it (ADR-0012)"
wait_state "$CP_URL" "$SID" running
"$ASP" sandbox stop "$SID" --json | grep -q stopping
wait_state "$CP_URL" "$SID" stopped
"$ASP" sandbox start "$SID" --json | grep -q requested
wait_state "$CP_URL" "$SID" running
[[ "$("$ASP" sandbox get "$SID" --json | json_field boot_count)" == 2 ]] || { echo "boot_count not 2 after a resume"; exit 1; }
"$ASP" sandbox delete "$SID" --json | grep -q deleting
wait_state "$CP_URL" "$SID" deleted
"$ASP" sandbox list --tenant=smoke | { ! grep -q "$SID"; }
"$ASP" sandbox list --tenant=smoke --all | grep -q "$SID"

echo "==> asp sandbox list"
"$ASP" sandbox list --tenant=smoke >/dev/null

echo "OK smoke-asp-cli"
