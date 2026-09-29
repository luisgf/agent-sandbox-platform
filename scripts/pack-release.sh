#!/usr/bin/env bash
# Pack a shippable source tree excluding build artifacts.
# Output: /workspace/agent-sandbox-platform-release.tar.gz (or ASP_RELEASE_TGZ)
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${ASP_RELEASE_TGZ:-/workspace/agent-sandbox-platform-release.tar.gz}"
STAGE="$(mktemp -d)"
NAME="agent-sandbox-platform"
trap 'rm -rf "$STAGE"' EXIT

mkdir -p "$STAGE/$NAME"
# Copy tree excluding heavy/ephemeral paths
if command -v rsync >/dev/null 2>&1; then
  rsync -a \
    --exclude '.git/' \
    --exclude '**/target/' \
    --exclude 'pod-daemon/target/' \
    --exclude '**/node_modules/' \
    --exclude 'build/' \
    --exclude '*.tar.gz' \
    --exclude '.DS_Store' \
    "$ROOT/" "$STAGE/$NAME/"
else
  tar -C "$ROOT" \
    --exclude='.git' \
    --exclude='target' \
    --exclude='pod-daemon/target' \
    --exclude='node_modules' \
    --exclude='build' \
    --exclude='*.tar.gz' \
    -cf - . | tar -C "$STAGE/$NAME" -xf -
fi

# Drop any nested target dirs that slipped through
find "$STAGE/$NAME" -type d -name target -prune -exec rm -rf {} + 2>/dev/null || true
find "$STAGE/$NAME" -type d -name .git -prune -exec rm -rf {} + 2>/dev/null || true

tar -C "$STAGE" -czf "$OUT" "$NAME"
echo "packed: $OUT ($(du -h "$OUT" | awk '{print $1}'))"
