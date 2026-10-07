# shellcheck shell=bash
# Helpers shared by the smoke scripts. Source after ROOT is set.

# A smoke runs with the settings it passes and with nothing else: not the /etc/asp or the
# ~/.config/asp of the host it happens to run on, which the node-agent, the control plane and
# the CLI read (/dev/null is "no file").
export ASP_CONFIG=/dev/null

# wait_node_schedulable <cp_url> <node_id> [timeout_s]
# Creates are refused (503) until a node can take sandboxes, and the agent's
# /healthz answers before it registers, so wait for the scheduler's view.
wait_node_schedulable() {
  local cp="$1" node="$2" timeout="${3:-20}" i
  for ((i = 0; i < timeout * 10; i++)); do
    if curl -fsS "$cp/v1/nodes" 2>/dev/null | python3 -c '
import json, sys
node = sys.argv[1]
nodes = json.load(sys.stdin).get("nodes", [])
sys.exit(0 if any(n["id"] == node and n.get("schedulable") for n in nodes) else 1)
' "$node"; then
      return 0
    fi
    sleep 0.1
  done
  echo "node $node never became schedulable:" >&2
  curl -sS "$cp/v1/nodes" >&2 || true
  return 1
}

# wait_state <cp_url> <sandbox_id> <state> [timeout_s]
# Waits until the control plane reports the sandbox in <state>: the node agent
# acts on a poll, so a stop, a resume or a delete takes a moment.
wait_state() {
  local cp="$1" sb="$2" want="$3" timeout="${4:-15}" i got=""
  for ((i = 0; i < timeout * 4; i++)); do
    got=$(curl -fsS "$cp/v1/sandboxes/$sb" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("state",""))' 2>/dev/null || true)
    [[ "$got" == "$want" ]] && return 0
    sleep 0.25
  done
  echo "sandbox $sb never reached $want (last state: ${got:-none})" >&2
  return 1
}

# json_field <field> : read one top-level field from JSON on stdin.
json_field() {
  python3 -c 'import json,sys; v=json.load(sys.stdin).get(sys.argv[1]); print("" if v is None else v)' "$1"
}

# wait_fresh_attestation <cp_url> <sandbox_id> [timeout_s]
# The node posts a signed boot attestation right after reporting running; the
# control plane stores it only if a trusted key signed it.
wait_fresh_attestation() {
  local cp="$1" sb="$2" timeout="${3:-10}" i
  for ((i = 0; i < timeout * 10; i++)); do
    if curl -fsS "$cp/v1/sandboxes/$sb/attestation" 2>/dev/null | grep -q '"fresh":true'; then
      return 0
    fi
    sleep 0.1
  done
  echo "sandbox $sb has no fresh attestation:" >&2
  curl -sS "$cp/v1/sandboxes/$sb/attestation" >&2 || true
  return 1
}
