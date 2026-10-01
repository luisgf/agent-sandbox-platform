#!/usr/bin/env bash
# Demo / prove guest→host SSH agent over CH hybrid vsock ({muxer}_26501).
# Usage (on bare-metal node with running sandbox + hybrid-aware node-agent):
#   SANDBOX_ID=... ./scripts/demo-hybrid-guest-ssh-agent.sh
# Or unit-level without KVM:
#   ./scripts/demo-hybrid-guest-ssh-agent.sh --unit
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if [[ "${1:-}" == "--unit" ]]; then
  echo "== unit: hostvsock hybrid acceptor =="
  (cd "$ROOT/node-agent" && go test ./internal/hostvsock/ -run 'Hybrid' -v -count=1)
  exit 0
fi

VSOCK_DIR="${ASP_VSOCK_DIR:-/run/asp}"
SID="${SANDBOX_ID:-}"
if [[ -z "$SID" ]]; then
  # pick newest muxer if unique
  mapfile -t socks < <(ls -1t "$VSOCK_DIR"/vsock-*.sock 2>/dev/null | grep -v '_[0-9]' || true)
  if [[ ${#socks[@]} -eq 1 ]]; then
    SID="$(basename "${socks[0]}" .sock | sed 's/^vsock-//')"
  fi
fi
if [[ -z "$SID" ]]; then
  echo "Set SANDBOX_ID=... (or leave exactly one vsock-*.sock in $VSOCK_DIR)" >&2
  exit 2
fi

MUX="$VSOCK_DIR/vsock-${SID}.sock"
HYB="${MUX}_26501"
echo "sandbox=$SID muxer=$MUX hybrid_ssh=$HYB"

if [[ ! -S "$HYB" ]]; then
  echo "FAIL: missing hybrid listener $HYB — is node-agent built with AttachSandbox + --host-vsock --reconcile?" >&2
  ls -la "$VSOCK_DIR"/vsock-"$SID"* 2>/dev/null || true
  exit 1
fi
echo "OK: hybrid SSH listener present"

# Prefer asp exec if available; else print guest commands for manual run.
EXEC_HELPER="${ASP_EXEC:-}"
GUEST_SCRIPT='set -e
mkdir -p /run/agent-sandbox
if ! pgrep -f vsock-ssh-agent-proxy >/dev/null 2>&1; then
  vsock-ssh-agent-proxy -listen /run/agent-sandbox/ssh-agent.sock -cid 2 -port 26501 &
  sleep 0.3
fi
perl -e "
  use IO::Socket::UNIX;
  my \$s = IO::Socket::UNIX->new(Type => SOCK_STREAM, Peer => \"/run/agent-sandbox/ssh-agent.sock\") or die \"dial: \$!\";
  my \$payload = pack(\"C\", 11);
  print \$s pack(\"N\", length(\$payload)), \$payload;
  my \$hdr; read(\$s, \$hdr, 4) == 4 or die \"hdr\";
  my \$len = unpack(\"N\", \$hdr);
  my \$body; read(\$s, \$body, \$len) == \$len or die \"body\";
  my \$type = unpack(\"C\", substr(\$body, 0, 1));
  print \"GUEST_SSH_AGENT_OK type=\$type\\n\";
  exit(\$type == 12 ? 0 : 1);
"'

if [[ -n "$EXEC_HELPER" ]]; then
  echo "== exec via \$ASP_EXEC =="
  # shellcheck disable=SC2086
  $EXEC_HELPER "$SID" -- bash -lc "$GUEST_SCRIPT"
else
  echo "Hybrid listener OK. Run inside guest (asp exec / pod-daemon):"
  echo "$GUEST_SCRIPT"
  echo
  echo "Tip: ASP_EXEC='asp exec' SANDBOX_ID=$SID $0"
fi
