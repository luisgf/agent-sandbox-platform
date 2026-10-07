#!/usr/bin/env bash
# Bare-metal prove on ncc1701d: CH hybrid guest→host SSH agent type=12.
set -uo pipefail
export PATH=/usr/local/go/bin:${HOME}/src/bots/build:${PATH}
DEMO=/tmp/asp-hybrid-prove

kill $(cat "$DEMO/cp.pid" 2>/dev/null) 2>/dev/null || true
sudo kill $(cat "$DEMO/na.pid" 2>/dev/null) 2>/dev/null || true
pkill -f "$DEMO/api" 2>/dev/null || true
sudo pkill -f "$DEMO/node-agent" 2>/dev/null || true
sudo pkill -f cloud-hypervisor 2>/dev/null || true
sleep 1

rm -rf "$DEMO"
mkdir -p "$DEMO/certs"
export ASP_AGENT_TOKEN_FILE="$DEMO/agent.token"   # the agent creates it; curl below sends it
cp ~/src/bots/build/node-agent ~/src/bots/build/api ~/src/bots/build/asp "$DEMO/"
cp /tmp/asp-oidc-key.pem /tmp/asp-attest-key.pem "$DEMO/"

sudo rm -f /run/asp/ch-*.sock* /run/asp/vsock-*.sock* /run/asp/ssh-agent-*.sock 2>/dev/null || true
sudo mkdir -p /run/asp && sudo chown ubuntu:ubuntu /run/asp
for t in $(ip -o link show 2>/dev/null | awk -F': ' '{print $2}' | grep '^asp-' || true); do
  sudo ip link del "$t" 2>/dev/null || true
done

CP_PORT=18111
NA_PORT=19211
EG_PORT=18911
NODE_ID=hybrid-prove

ASP_LISTEN_ADDR=127.0.0.1:$CP_PORT \
ASP_OIDC_ISSUER=http://127.0.0.1:$CP_PORT \
ASP_OIDC_KEY=$DEMO/asp-oidc-key.pem \
ASP_ATTEST_KEY=$DEMO/asp-attest-key.pem \
ASP_INSECURE_OPEN_API=1 \
ASP_NODE_BOOTSTRAP_TOKEN=dev-bootstrap \
  "$DEMO/api" >"$DEMO/cp.log" 2>&1 &
echo $! >"$DEMO/cp.pid"
ok=0
for _ in $(seq 1 40); do
  if curl -sf "http://127.0.0.1:$CP_PORT/healthz" >/dev/null; then ok=1; break; fi
  sleep 0.2
done
if [[ $ok -ne 1 ]]; then echo CP_FAIL; cat "$DEMO/cp.log"; exit 1; fi
echo CP_OK

# Same attestation key as the control plane: it trusts no other key here.
# The demo keeps its certificates under /tmp: allow it on later runs too.
sudo env ASP_ATTEST_KEY="$DEMO/asp-attest-key.pem" ASP_ALLOW_TMP_KEYS=1 "$DEMO/node-agent" \
    --control-plane-url "http://127.0.0.1:$CP_PORT" \
    --node-id "$NODE_ID" \
    --enroll \
    --bootstrap-token dev-bootstrap \
    --cert-dir "$DEMO/certs" \
    --agent-listen "127.0.0.1:$NA_PORT" \
    --reconcile \
    --reconcile-interval 1s \
    --tap-auto \
    --host-vsock \
    --ssh-agent-bridge "$DEMO/ssh-agent.sock" \
    --egress-proxy-listen "127.0.0.1:$EG_PORT" \
    --ch-socket-dir /run/asp \
    --ch-binary /usr/local/bin/cloud-hypervisor \
  >"$DEMO/na.log" 2>&1 &
echo $! >"$DEMO/na.pid"
ok=0
for _ in $(seq 1 50); do
  if grep -q 'registered with control plane' "$DEMO/na.log" 2>/dev/null; then ok=1; break; fi
  sleep 0.2
done
grep -E 'hybrid|host-vsock|ERROR|registered|guest' "$DEMO/na.log" | head -40 || true
if [[ $ok -ne 1 ]]; then echo NA_FAIL; tail -50 "$DEMO/na.log"; exit 1; fi
echo NA_OK

curl -sf -X POST "http://127.0.0.1:$CP_PORT/v1/sandboxes" \
  -H 'Content-Type: application/json' \
  -d '{"tenant_id":"tenant-demo","image_ref":"local","cpu_millis":1000,"memory_mib":512,"vmm_profile":"cloud-hypervisor"}' \
  | tee "$DEMO/create.json"
echo
SID=$(python3 -c 'import json; print(json.load(open("/tmp/asp-hybrid-prove/create.json"))["id"])')
echo "sandbox=$SID"
echo "$SID" >"$DEMO/sandbox.id"

ok=0
for _ in $(seq 1 90); do
  if [[ -S /run/asp/vsock-${SID}.sock_26501 ]]; then
    echo HYBRID_LISTENER_OK
    ls -la /run/asp/vsock-${SID}.sock*
    ok=1
    break
  fi
  sleep 0.5
done
grep -E 'hybrid guest|sandbox running|failed|ERROR' "$DEMO/na.log" | tail -30 || true
if [[ $ok -ne 1 ]]; then echo HYBRID_FAIL; ls -la /run/asp/; exit 1; fi

ok=0
for _ in $(seq 1 90); do
  resp=$(curl -sf -X POST "http://127.0.0.1:$NA_PORT/v1/internal/exec" \
    -H "Authorization: Bearer $(cat "$ASP_AGENT_TOKEN_FILE" 2>/dev/null)" -H 'Content-Type: application/json' \
    -d "{\"sandbox_id\":\"$SID\",\"cmd\":[\"/bin/true\"],\"timeout_secs\":5}" || true)
  if echo "$resp" | grep -q 'exit_code":0'; then ok=1; echo guest_true_ok; break; fi
  sleep 1
done
if [[ $ok -ne 1 ]]; then echo GUEST_NOT_READY; echo "$resp"; fi


# Start proxy then REQUEST_IDENTITIES (guest has perl; python3 optional).
curl -sS -X POST "http://127.0.0.1:$NA_PORT/v1/internal/exec" \
  -H "Authorization: Bearer $(cat "$ASP_AGENT_TOKEN_FILE")" -H 'Content-Type: application/json' \
  -d "{\"sandbox_id\":\"$SID\",\"cmd\":[\"sh\",\"-c\",\"mkdir -p /run/agent-sandbox; rm -f /run/agent-sandbox/ssh-agent.sock; vsock-ssh-agent-proxy -listen /run/agent-sandbox/ssh-agent.sock -cid 2 -port 26501 >/tmp/proxy.log 2>&1 & sleep 1; ls -l /run/agent-sandbox/ssh-agent.sock\"],\"timeout_secs\":15}"
echo

# Write perl probe into guest via a tiny argv (no nested python).
curl -sS -X POST "http://127.0.0.1:$NA_PORT/v1/internal/exec" \
  -H "Authorization: Bearer $(cat "$ASP_AGENT_TOKEN_FILE")" -H 'Content-Type: application/json' \
  --data-binary @- <<JSON | tee "$DEMO/ssh-proof.json"
{"sandbox_id":"$SID","timeout_secs":20,"cmd":["perl","-e","use IO::Socket::UNIX; my \$s=IO::Socket::UNIX->new(Type=>SOCK_STREAM,Peer=>\"/run/agent-sandbox/ssh-agent.sock\") or die \"dial: \$!\"; my \$p=pack(\"C\",11); print \$s pack(\"N\",length(\$p)),\$p; my \$h; read(\$s,\$h,4)==4 or die \"hdr\"; my \$n=unpack(\"N\",\$h); my \$b; read(\$s,\$b,\$n)==\$n or die \"body\"; my \$t=unpack(\"C\",substr(\$b,0,1)); print \"GUEST_SSH_AGENT_OK type=\$t\\n\"; exit(\$t==12?0:1);"]}
JSON
echo
grep -q 'GUEST_SSH_AGENT_OK type=12' "$DEMO/ssh-proof.json" || { echo PROVE_SSH_FAIL; exit 1; }

echo PROVE_DONE
