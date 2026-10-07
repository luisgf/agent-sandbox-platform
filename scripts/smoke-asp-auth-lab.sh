#!/usr/bin/env bash
# Smoke asp auth + list against lab CP (IdP required). No token printed.
# Intended on ncc1701d (or with tunnel to 18112 + local secrets file).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ASP_BIN="${ASP_BIN:-$ROOT/build/asp}"
CP_URL="${ASP_CONTROL_PLANE_URL:-http://127.0.0.1:18112}"
SECRETS="${ASP_IDP_SECRETS_FILE:-$HOME/.secrets/asp-keycloak-lab.txt}"
CACHE="${ASP_IDP_TOKEN_CACHE:-$(mktemp -t asp-auth-cache.XXXXXX.json)}"
cleanup() { rm -f "$CACHE" 2>/dev/null || true; }
trap cleanup EXIT

if [[ ! -x "$ASP_BIN" ]]; then
  echo "==> build asp"
  mkdir -p "$ROOT/build"
  (cd "$ROOT/cli" && go build -o "$ASP_BIN" ./cmd/asp)
fi
if [[ ! -f "$SECRETS" ]]; then
  echo "missing secrets file: $SECRETS" >&2
  exit 1
fi

echo "==> healthz"
curl -fsS "$CP_URL/healthz" | grep -qi ok

echo "==> 401 without token"
code=$(curl -sS -o /dev/null -w '%{http_code}' "$CP_URL/v1/sandboxes?tenant_id=default" || true)
[[ "$code" == "401" ]] || { echo "expected 401 got $code"; exit 1; }

echo "==> asp auth login"
export ASP_CONTROL_PLANE_URL="$CP_URL"
export ASP_REQUIRE_TOKEN=1
export ASP_IDP_SECRETS_FILE="$SECRETS"
export ASP_IDP_TOKEN_CACHE="$CACHE"
"$ASP_BIN" auth login >/dev/null
"$ASP_BIN" auth status | grep -q 'cache_valid=true'

echo "==> asp sandbox list (Bearer from cache)"
# Clear env token so cache path is exercised
unset ASP_ID_TOKEN ASP_IDP_ACCESS_TOKEN || true
"$ASP_BIN" sandbox list --tenant=default --json >/dev/null

echo "OK smoke-asp-auth-lab"
