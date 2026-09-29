#!/usr/bin/env sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
input="$repo_root/docs/diagram.mmd"
output="$repo_root/docs/diagram.svg"

if command -v mmdc >/dev/null 2>&1; then
  mmdc -i "$input" -o "$output"
  printf 'Generated %s\n' "$output"
else
  printf '%s\n' 'Mermaid CLI is not installed.'
  printf 'Run: npx --yes @mermaid-js/mermaid-cli -i docs/diagram.mmd -o docs/diagram.svg\n'
  exit 1
fi
