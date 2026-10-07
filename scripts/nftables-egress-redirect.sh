#!/usr/bin/env bash
# Guest egress redirect for ASP (nftables). The script itself lives next to the Go
# code that embeds it, so the node-agent binary carries the version that matches
# it and needs no file installed on the host:
#   node-agent/internal/nftredirect/nftables-egress-redirect.sh
# This wrapper keeps the path the docs use for inspecting the ruleset from a
# checkout:  ./scripts/nftables-egress-redirect.sh dry-run --guest-subnet ...
exec "$(cd "$(dirname "$0")" && pwd)/../node-agent/internal/nftredirect/nftables-egress-redirect.sh" "$@"
