#!/usr/bin/env bash
# Lab control-plane with Keycloak IdP required (realm asp @ auth.luisgf.es).
# Env: ~/.secrets/asp-idp.env (ASP_IDP_*). Does not touch Keycloak.
# Prefer systemd unit asp-control-plane.service on ncc1701d (see scripts/systemd/).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEMO="${ASP_CP_DEMO_DIR:-/tmp/asp-idp-lab}"
CP_PORT="${ASP_CP_PORT:-18112}"
API_BIN="${ASP_API_BIN:-$ROOT/build/api}"
IDP_ENV="${ASP_IDP_ENV:-$HOME/.secrets/asp-idp.env}"

if [[ ! -x "$API_BIN" ]]; then
  echo "missing api binary: $API_BIN (build control-plane first)" >&2
  exit 1
fi
if [[ ! -f "$IDP_ENV" ]]; then
  echo "missing IdP env: $IDP_ENV" >&2
  exit 1
fi

# Stop previous lab instance in this DEMO dir
if [[ -f "$DEMO/cp.pid" ]]; then
  kill "$(cat "$DEMO/cp.pid")" 2>/dev/null || true
  sleep 0.3
fi
pkill -f "$DEMO/api" 2>/dev/null || true
rm -rf "$DEMO"
mkdir -p "$DEMO"
cp "$API_BIN" "$DEMO/api"

set -a
# shellcheck disable=SC1090
. "$IDP_ENV"
set +a

export LISTEN_ADDR="127.0.0.1:${CP_PORT}"
export ASP_OIDC_ISSUER="${ASP_OIDC_ISSUER:-http://127.0.0.1:${CP_PORT}}"
export ASP_AUTH_REQUIRE="${ASP_AUTH_REQUIRE:-0}"
# reuse host oidc/attest keys if present (lab)
[[ -f /tmp/asp-oidc-key.pem ]] && export ASP_OIDC_KEY_PATH=/tmp/asp-oidc-key.pem
[[ -f /tmp/asp-attest-key.pem ]] && export ASP_ATTEST_KEY_PATH=/tmp/asp-attest-key.pem

"$DEMO/api" >"$DEMO/cp.log" 2>&1 &
echo $! >"$DEMO/cp.pid"

ok=0
for _ in $(seq 1 50); do
  if curl -sf "http://127.0.0.1:${CP_PORT}/healthz" >/dev/null; then ok=1; break; fi
  sleep 0.1
done
if [[ $ok -ne 1 ]]; then
  echo "CP_FAIL port=${CP_PORT}" >&2
  tail -40 "$DEMO/cp.log" >&2 || true
  exit 1
fi
# Confirm IdP required from log
if grep -q "idp_required=true\|idp enabled\|IdP" "$DEMO/cp.log" 2>/dev/null || grep -qi "issuer" "$DEMO/cp.log"; then
  grep -E "idp|IdP|issuer|listening" "$DEMO/cp.log" | head -20 || true
fi
echo "CP_OK url=http://127.0.0.1:${CP_PORT} pid=$(cat "$DEMO/cp.pid") env=$IDP_ENV demo=$DEMO"
