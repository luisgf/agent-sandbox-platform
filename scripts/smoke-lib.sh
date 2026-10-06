# shellcheck shell=bash
# Helpers shared by the smoke scripts. Source after ROOT is set.

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

# json_field <field> : read one top-level field from JSON on stdin.
json_field() {
  python3 -c 'import json,sys; v=json.load(sys.stdin).get(sys.argv[1]); print("" if v is None else v)' "$1"
}
