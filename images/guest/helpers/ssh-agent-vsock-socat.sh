#!/usr/bin/env bash
# Fallback when vsock-ssh-agent-proxy binary is missing: socat unix ← vsock.
# Prefer /usr/local/bin/vsock-ssh-agent-proxy in the guest image.
set -euo pipefail
LISTEN="${ASP_SSH_AUTH_SOCK:-/run/agent-sandbox/ssh-agent.sock}"
CID="${ASP_HOST_CID:-2}"
PORT="${ASP_SSH_AGENT_VSOCK_PORT:-26501}"
mkdir -p "$(dirname "$LISTEN")"
rm -f "$LISTEN"
exec socat "UNIX-LISTEN:${LISTEN},fork,mode=666" "VSOCK-CONNECT:${CID}:${PORT}"
