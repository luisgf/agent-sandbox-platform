#!/bin/bash
# In the asp machine of the browser spike (#231), as root, after the installer: give ASP's egress proxy a way out.
# The machine has no route to the Internet, only the lab's allowlist proxy (10.82.0.1:3129). The sites the tests load
# resolve (/etc/hosts) to 93.184.216.34; the rig's netns DNATs that address's ports 80/443 to tproxy-host.py on the host
# end of the veth, which hands them to the lab proxy by Host header and TLS SNI. ASP's own proxy, the one under test,
# decides first; then the lab's. (The address cannot live on this machine: ASP's proxy refuses to dial the node itself.)
set -euo pipefail
FAKE=93.184.216.34
SITES="example.com www.example.com en.wikipedia.org upload.wikimedia.org meta.wikimedia.org www.wikipedia.org www.mozilla.org"
ip addr del "$FAKE/32" dev lo 2>/dev/null || true
systemctl stop spike-tproxy 2>/dev/null || true
sed -i '/# browser-spike$/d' /etc/hosts
for s in $SITES; do echo "$FAKE $s # browser-spike" >> /etc/hosts; done
curl -s -o /dev/null -w "through the rig's tproxy, example.com: %{http_code}\n" --max-time 10 https://example.com/ || true

# The tenant's allowlist: the test sites, nothing else.
CPU=$(sed -n 's/^control_plane_url: *//p' /etc/asp/asp.yaml | head -1)
CAF=$(sed -n 's/^ca_file: *//p' /etc/asp/asp.yaml | head -1)
KEYF=$(sed -n 's/^api_key_file: *//p' /etc/asp/asp.yaml | head -1)
curl -sS ${CAF:+--cacert $CAF} -o /dev/null -w 'egress policy PUT: %{http_code}\n' -X PUT \
  -H "Authorization: Bearer $(cat "$KEYF")" -H 'Content-Type: application/json' \
  -d '{"rules":[{"host_pattern":"example.com","enabled":true},{"host_pattern":"*.example.com","enabled":true},{"host_pattern":"*.wikipedia.org","enabled":true},{"host_pattern":"*.wikimedia.org","enabled":true}]}' \
  "$CPU/v1/tenants/default/egress"
