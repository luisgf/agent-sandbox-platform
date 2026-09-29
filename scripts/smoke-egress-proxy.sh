#!/usr/bin/env bash
# Optional dry-run: HTTP forward proxy allow/deny (no KVM / no CP).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORKDIR="${TMPDIR:-/tmp}/asp-smoke-proxy-$$"
mkdir -p "$WORKDIR"
cleanup() {
  [[ -n "${PROXY_PID:-}" ]] && kill "$PROXY_PID" 2>/dev/null || true
  [[ -n "${UP_PID:-}" ]] && kill "$UP_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

echo "==> upstream on :18099"
python3 - <<'PY' &
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(b"upstream-ok")
    def log_message(self, *a): pass
HTTPServer(("127.0.0.1", 18099), H).serve_forever()
PY
UP_PID=$!

echo "==> proxy smoke binary"
(cd "$ROOT/node-agent" && go build -o "$WORKDIR/proxy" ./cmd/egress-proxy-smoke)
"$WORKDIR/proxy" -listen 127.0.0.1:18888 -allow 127.0.0.1 >"$WORKDIR/proxy.log" 2>&1 &
PROXY_PID=$!
for i in $(seq 1 50); do
  if curl -sf -o /dev/null -x http://127.0.0.1:18888 http://127.0.0.1:18099/ 2>/dev/null; then break; fi
  sleep 0.1
done

echo "==> allow"
curl -sf -x http://127.0.0.1:18888 http://127.0.0.1:18099/ | grep -q upstream-ok

echo "==> deny"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -x http://127.0.0.1:18888 http://evil.example/ || true)
[[ "$CODE" == "403" ]] || { echo "want 403 got $CODE"; cat "$WORKDIR/proxy.log"; exit 1; }

echo "OK smoke-egress-proxy"
