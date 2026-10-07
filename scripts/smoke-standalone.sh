#!/usr/bin/env bash
# ASP on one host with nothing configured (docs/how-to/single-host.md): asp-server makes the
# database, the keys, the TLS certificate, the administration key and the node token, starts a
# control plane and a node (--profile lab: no VMs, so it needs no KVM and no root), and leaves the
# asp command configured. Real binaries throughout:
#   1. the asp command reaches the control plane with the generated configuration: the
#      self-signed certificate and the key from a file, nothing else set;
#   2. without that configuration it does not (the certificate is not trusted);
#   3. a session starts and runs;
#   4. the files are private, and asp node enroll-token prints the fingerprint of the certificate;
#   5. SIGTERM stops the node, then the control plane, and leaves nothing running;
#   6. started again, the same certificate and key are in use and the sandbox is still there.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=smoke-lib.sh
source "$ROOT/scripts/smoke-lib.sh"
WORKDIR="${TMPDIR:-/tmp}/asp-smoke-standalone-$$"
mkdir -p "$WORKDIR/bin"
DATA="$WORKDIR/data"
LOG="$WORKDIR/server.log"
SERVER_PID=
cleanup() {
  [[ -n "$SERVER_PID" ]] && kill "$SERVER_PID" 2>/dev/null || true
  # asp-server stops its children; give it the time, then take what is left.
  for pid in $(children_from_log); do kill "$pid" 2>/dev/null || true; done
  rm -rf "$WORKDIR"
}
trap cleanup EXIT
fail() {
  echo "FAIL: $*" >&2
  echo "--- asp-server log" >&2; tail -30 "$LOG" >&2 || true
  exit 1
}
children_from_log() { sed -n 's/.*msg=started .*pid=\([0-9]*\).*/\1/p' "$LOG" 2>/dev/null | sort -u; }
mode() { python3 -c 'import os,sys; print(format(os.stat(sys.argv[1]).st_mode & 0o777, "o"))' "$1"; }
alive() { kill -0 "$1" 2>/dev/null; }

echo "==> build"
(cd "$ROOT/control-plane" && go build -o "$WORKDIR/bin/asp-control-plane" ./cmd/api)
(cd "$ROOT/node-agent" && go build -o "$WORKDIR/bin/asp-node-agent" ./cmd/node-agent)
(cd "$ROOT/cli" && go build -o "$WORKDIR/bin/asp" ./cmd/asp && go build -o "$WORKDIR/bin/asp-server" ./cmd/asp-server)
PORT=$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
CLI_CONFIG="$DATA/server/asp.yaml"
export ASP_SESSION_DIR="$WORKDIR/sessions"
asp() { ASP_CONFIG="$CLI_CONFIG" "$WORKDIR/bin/asp" "$@"; }

start_server() {
  "$WORKDIR/bin/asp-server" --profile lab --data-dir "$DATA" --listen "127.0.0.1:$PORT" --cli-config - >>"$LOG" 2>&1 &
  SERVER_PID=$!
}
wait_node() { # the node enrolled and registered: the scheduler sees it
  for _ in $(seq 1 300); do
    if [[ -f "$CLI_CONFIG" ]] && asp node list 2>/dev/null | grep -qE ' ready +yes '; then return 0; fi
    alive "$SERVER_PID" || fail "asp-server exited"
    sleep 0.2
  done
  fail "the node never became schedulable"
}

echo "==> start asp-server (nothing configured)"
start_server
wait_node

echo "==> 1. the asp command reaches it with the configuration asp-server left"
asp node list | grep -q ' ready ' || fail "asp node list"
grep -q "^control_plane_url: https://127.0.0.1:$PORT$" "$CLI_CONFIG" || fail "the CLI configuration names another URL"
grep -q "^api_key_file: $DATA/server/admin-key$" "$CLI_CONFIG" || fail "the CLI configuration does not name the key file"

echo "==> 2. without it the certificate is not trusted"
if ASP_CONFIG=/dev/null ASP_CONTROL_PLANE_URL="https://127.0.0.1:$PORT" "$WORKDIR/bin/asp" node list >/dev/null 2>"$WORKDIR/untrusted.err"; then
  fail "the CLI accepted a certificate it was not told to trust"
fi
grep -qi 'certificate' "$WORKDIR/untrusted.err" || fail "unexpected error without the CA: $(cat "$WORKDIR/untrusted.err")"
if ASP_CONFIG=/dev/null ASP_CA_FILE="$DATA/server/tls.crt" ASP_CONTROL_PLANE_URL="https://127.0.0.1:$PORT" "$WORKDIR/bin/asp" node list >/dev/null 2>&1; then
  fail "the control plane answered a request with no key"
fi

echo "==> 3. a session starts and runs"
asp session start --tenant default >/dev/null 2>&1 || fail "asp session start"
asp session status 2>&1 | grep -q 'state=running' || fail "the session is not running"

echo "==> 4. the files are private, and the join command carries the certificate's fingerprint"
for f in admin-key node-token tls.key asp.db ca.key oidc-key.pem; do
  [[ "$(mode "$DATA/server/$f")" == 600 ]] || fail "$f is not mode 600: $(mode "$DATA/server/$f")"
done
[[ "$(mode "$DATA/agent.token")" == 600 ]] || fail "agent.token is not private"
FP=$(openssl x509 -in "$DATA/server/tls.crt" -outform DER | openssl dgst -sha256 | sed 's/.*= *//')
asp node enroll-token --node-id second-host 2>&1 | grep -q "INSTALL_ASP_CA_SHA256=$FP" || fail "enroll-token does not print the fingerprint of the certificate"
KEY1=$(cat "$DATA/server/admin-key")
PIDS=$(children_from_log)

echo "==> 5. SIGTERM stops the node, then the control plane, and leaves nothing"
kill -TERM "$SERVER_PID"
for _ in $(seq 1 150); do alive "$SERVER_PID" || break; sleep 0.2; done
alive "$SERVER_PID" && fail "asp-server did not stop"
SERVER_PID=
for pid in $PIDS; do alive "$pid" && fail "process $pid is still running"; done
agent_stop=$(grep -n 'msg=stopped .*process=node-agent' "$LOG" | tail -1 | cut -d: -f1)
cp_stop=$(grep -n 'msg=stopped .*process=control-plane' "$LOG" | tail -1 | cut -d: -f1)
[[ -n "$agent_stop" && -n "$cp_stop" && "$agent_stop" -lt "$cp_stop" ]] || fail "the node did not stop before the control plane"

echo "==> 6. started again: same certificate, same key, and the sandbox is still there"
: >"$LOG"
start_server
wait_node
FP2=$(openssl x509 -in "$DATA/server/tls.crt" -outform DER | openssl dgst -sha256 | sed 's/.*= *//')
[[ "$FP" == "$FP2" ]] || fail "the certificate changed across a restart"
[[ "$KEY1" == "$(cat "$DATA/server/admin-key")" ]] || fail "the administration key changed across a restart"
asp session status 2>&1 | grep -qE 'state=(stopped|running)' || fail "the sandbox did not survive the restart: $(asp session status 2>&1 | head -2)"
kill -TERM "$SERVER_PID"
for _ in $(seq 1 150); do alive "$SERVER_PID" || break; sleep 0.2; done
SERVER_PID=

echo "OK smoke-standalone"
