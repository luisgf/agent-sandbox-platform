#!/bin/bash
# In the asp machine of the browser spike, as root: delete every sandbox that is not deleted (the orphans of an
# interrupted run, which have no session file), then wait until no VM unit is left.
set -uo pipefail
CPU=$(sed -n 's/^control_plane_url: *//p' /etc/asp/asp.yaml | head -1)
KEYF=$(sed -n 's/^api_key_file: *//p' /etc/asp/asp.yaml | head -1)
CAF=$(sed -n 's/^ca_file: *//p' /etc/asp/asp.yaml | head -1)
api() { curl -s ${CAF:+--cacert $CAF} -H "Authorization: Bearer $(cat "$KEYF")" "$@"; }
for id in $(api "$CPU/v1/sandboxes" | python3 -c 'import json,sys; d=json.load(sys.stdin); d=d.get("sandboxes", d) if isinstance(d, dict) else d; print(" ".join(s["id"] for s in d if s["state"] not in ("deleted", "deleting")))'); do
  printf '%s %s\n' "$id" "$(api -o /dev/null -w '%{http_code}' -X DELETE "$CPU/v1/sandboxes/$id")"
done
for _ in $(seq 1 120); do [[ $(systemctl list-units --no-legend --plain 'asp-vm-*' | wc -l) == 0 ]] && break; sleep 1; done
rm -f /root/.cache/asp/sessions/*.json
echo "VM units left: $(systemctl list-units --no-legend --plain 'asp-vm-*' | wc -l)"
