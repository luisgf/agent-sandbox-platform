#!/usr/bin/env bash
# Dry-run smoke: tenant egress allowlist + OIDC JWKS mint + SSH agent bridge (no root).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=smoke-lib.sh
source "$ROOT/scripts/smoke-lib.sh"
WORKDIR="${TMPDIR:-/tmp}/asp-smoke-id-$$"
mkdir -p "$WORKDIR"
# The node-agent creates this file; the control plane reads it to call the agent.
export ASP_AGENT_TOKEN_FILE="$WORKDIR/agent.token"
SOCK="$WORKDIR/pod-daemon.sock"
CERT_DIR="$WORKDIR/certs"
SSH_BRIDGE="$WORKDIR/ssh-agent.sock"
ID_SOCK="$WORKDIR/identity.sock"
OIDC_KEY="$WORKDIR/oidc.pem"
CP_LOG="$WORKDIR/cp.log"
PD_LOG="$WORKDIR/pd.log"
NA_LOG="$WORKDIR/na.log"
TOKEN="smoke-bootstrap-token"
export ASP_NODE_BOOTSTRAP_TOKEN="$TOKEN"
export ASP_CA_CERT="$WORKDIR/ca.crt"
export ASP_CA_KEY="$WORKDIR/ca.key"
export ASP_OIDC_KEY="$OIDC_KEY"
export ASP_OIDC_ISSUER="http://127.0.0.1:18081"
export ASP_EGRESS_DENY_DEFAULT=1
export ASP_AUTO_PROVISION=0
# Do not set ASP_EGRESS_DEFAULT_ALLOW — empty rules deny in this smoke.

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

echo "==> start control-plane (memory + deny-default egress, auto_provision=0)"
LISTEN_ADDR=127.0.0.1:18081 ASP_AUTO_PROVISION=0 "$WORKDIR/api" >"$CP_LOG" 2>&1 &
CP_PID=$!
for i in $(seq 1 50); do
  curl -sf http://127.0.0.1:18081/healthz >/dev/null && break
  sleep 0.1
done
curl -sf http://127.0.0.1:18081/healthz | grep -q ok

echo "==> OIDC discovery + JWKS"
curl -sf http://127.0.0.1:18081/.well-known/openid-configuration | grep -q jwks_uri
JWKS=$(curl -sf http://127.0.0.1:18081/oidc/jwks.json)
echo "$JWKS" | grep -q '"kty":"RSA"'

echo "==> put tenant egress allowlist"
curl -sf -X PUT http://127.0.0.1:18081/v1/tenants/smoke/egress \
  -H 'Content-Type: application/json' \
  -d '{"rules":[{"host_pattern":"*.github.com","enabled":true},{"host_pattern":"registry.npmjs.org"}]}' \
  | grep -q deny-default

echo "==> CP egress check helper"
ALLOW=$(curl -sf -X POST http://127.0.0.1:18081/v1/tenants/smoke/egress/check \
  -H 'Content-Type: application/json' \
  -d '{"host":"api.github.com"}')
echo "$ALLOW" | grep -q '"allowed":true'
DENY=$(curl -sf -X POST http://127.0.0.1:18081/v1/tenants/smoke/egress/check \
  -H 'Content-Type: application/json' \
  -d '{"host":"evil.example"}')
echo "$DENY" | grep -q '"allowed":false'

echo "==> start node-agent (enroll + reconcile + egress-enforce + ssh bridge + identity)"
"$WORKDIR/node-agent" \
  --control-plane-url=http://127.0.0.1:18081 \
  --node-id=smoke-id-node \
  --dry-run \
  --reconcile \
  --reconcile-interval=500ms \
  --enroll \
  --bootstrap-token="$TOKEN" \
  --cert-dir="$CERT_DIR" \
  --agent-listen=127.0.0.1:19101 \
  --pod-daemon-sock="$SOCK" \
  --egress-enforce \
  --ssh-agent-bridge="$SSH_BRIDGE" \
  --identity-listen="$ID_SOCK" \
  --insecure-identity-sandbox-header \
  --heartbeat-interval=1h \
  >"$NA_LOG" 2>&1 &
NA_PID=$!
for i in $(seq 1 50); do
  curl -sf http://127.0.0.1:19101/healthz >/dev/null && break
  sleep 0.1
done
curl -sf http://127.0.0.1:19101/healthz | grep -q ok
wait_node_schedulable http://127.0.0.1:18081 smoke-id-node
[[ -S "$SSH_BRIDGE" ]] || { echo "ssh bridge missing"; cat "$NA_LOG"; exit 1; }
[[ -S "$ID_SOCK" ]] || { echo "identity sock missing"; cat "$NA_LOG"; exit 1; }

echo "==> node egress-check (enforce → 403 on deny)"
POLICY='{"tenant_id":"smoke","mode":"deny-default","rules":[{"host_pattern":"*.github.com","enabled":true}]}'
AGENT_AUTH="Authorization: Bearer $(cat "$ASP_AGENT_TOKEN_FILE")"
curl -sf -X POST http://127.0.0.1:19101/v1/internal/egress-check \
  -H "$AGENT_AUTH" -H 'Content-Type: application/json' \
  -d "{\"host\":\"api.github.com\",\"egress_allowlist\":$POLICY}" | grep -q '"allowed":true'
CODE=$(curl -s -o /tmp/asp-eg-deny.json -w '%{http_code}' -X POST http://127.0.0.1:19101/v1/internal/egress-check \
  -H "$AGENT_AUTH" -H 'Content-Type: application/json' \
  -d "{\"host\":\"evil.example\",\"egress_allowlist\":$POLICY}")
[[ "$CODE" == "403" ]] || { echo "want 403 got $CODE"; cat /tmp/asp-eg-deny.json; exit 1; }

echo "==> create sandbox + wait running + mint OIDC token"
SB=$(curl -sf -X POST http://127.0.0.1:18081/v1/sandboxes \
  -H 'Content-Type: application/json' \
  -d '{"tenant_id":"smoke","image_ref":"debian:bookworm","cpu_millis":500,"memory_mib":256,"node_id":"smoke-id-node"}')
SID=$(echo "$SB" | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')
OK=0
for i in $(seq 1 40); do
  GOT=$(curl -sf "http://127.0.0.1:18081/v1/sandboxes/${SID}")
  echo "$GOT" | grep -q '"state":"running"' && { OK=1; break; }
  sleep 0.25
done
[[ "$OK" == "1" ]] || { echo "timeout waiting running"; cat "$NA_LOG"; exit 1; }
MINT=$(curl -sf -X POST http://127.0.0.1:18081/v1/internal/oidc/token \
  -H 'Content-Type: application/json' \
  -d "{\"sandbox_id\":\"$SID\",\"aud\":\"https://api.example.com\",\"nonce\":\"smoke\"}")
echo "$MINT" | grep -q access_token
echo "$MINT" | grep -q '"tenant_id":"smoke"'

echo "==> identity proxy (guest-facing) forwards mint"
# --identity-listen has no sandbox binding (a guest's own vsock 26502 does): the
# header names the sandbox only because of --insecure-identity-sandbox-header (lab).
TOK=$(curl -sf --unix-socket "$ID_SOCK" \
  -X POST http://localhost/v1/tokens/oidc \
  -H "Content-Type: application/json" \
  -H "X-ASP-Sandbox-ID: $SID" \
  -d '{"aud":"https://api.example.com"}')
echo "$TOK" | grep -q access_token

echo "==> exec still works; egress_allowlist attached by CP"
OUT=$(curl -sf -X POST "http://127.0.0.1:18081/v1/sandboxes/${SID}/exec" \
  -H 'Content-Type: application/json' \
  -d '{"cmd":["echo","hello"]}')
echo "$OUT" | grep -q hello

echo "OK smoke-identity-egress"
