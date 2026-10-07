#!/usr/bin/env bash
# Fase 2e: complete guest egress redirect (HTTP + DNS anti-bypass).
#
# Por qué: HTTP_PROXY is voluntary — a compromised guest can bypass the allowlist
# by dialing the internet directly or resolving via public DNS to raw IPs.
#
# Qué ganamos: force guest TCP HTTP(S) through the node-agent forward proxy and
# pin/block guest DNS so allowlist cannot be skipped via direct resolvers.
# Everything else a guest sends is dropped (deny-by-default):
#   - forward: no other port leaves the node, and no guest reaches another
#     guest. Only a local-net session's own tunnel (wg-asp-*) is forwarded.
#   - input: a guest reaches only the proxy and the DNS sink on the host,
#     never node-agent, sshd or any other host service.
#   - anti-spoof: a guest may only use the /30 routed to its own TAP, so the
#     proxy's per-sandbox policy (keyed by source IP) cannot be borrowed.
# Guest TAPs are matched by name (asp-*); wg-asp-* does not match.
#
# Modes:
#   soft    — SoftFail: missing root/nft/CAP_NET_ADMIN → warn + exit 0 (apply)
#   enforce — fail hard (non-zero) when rules cannot be applied
#
# Usage:
#   ./scripts/nftables-egress-redirect.sh dry-run \
#     --guest-subnet 10.200.0.0/16 --proxy-port 8888 --dns-sink-port 5353
#   sudo ./scripts/nftables-egress-redirect.sh apply --mode enforce ...
#   sudo ./scripts/nftables-egress-redirect.sh flush
#   ./scripts/nftables-egress-redirect.sh dry-run --mode soft ...
set -euo pipefail

ACTION="${1:-}"
shift || true

GUEST_SUBNET="${ASP_GUEST_SUBNET:-10.200.0.0/16}"
PROXY_IP="${ASP_EGRESS_PROXY_IP:-10.200.0.1}"
PROXY_PORT="${ASP_EGRESS_PROXY_PORT:-8888}"
DNS_SINK_IP="${ASP_EGRESS_DNS_SINK_IP:-}"
DNS_SINK_PORT="${ASP_EGRESS_DNS_SINK_PORT:-5353}"
HTTP_PORTS="${ASP_NFT_HTTP_PORTS:-80,443}"
TABLE="${ASP_NFT_TABLE:-asp_egress}"
MODE="${ASP_NFT_EGRESS_MODE:-soft}"   # soft | enforce
DNS_ACTION="${ASP_NFT_DNS_ACTION:-redirect}"  # redirect | drop

while [[ $# -gt 0 ]]; do
  case "$1" in
    --guest-subnet) GUEST_SUBNET="$2"; shift 2 ;;
    --proxy-ip) PROXY_IP="$2"; shift 2 ;;
    --proxy-port) PROXY_PORT="$2"; shift 2 ;;
    --dns-sink-ip) DNS_SINK_IP="$2"; shift 2 ;;
    --dns-sink-port) DNS_SINK_PORT="$2"; shift 2 ;;
    --http-ports) HTTP_PORTS="$2"; shift 2 ;;
    --table) TABLE="$2"; shift 2 ;;
    --mode) MODE="$2"; shift 2 ;;
    --dns-action) DNS_ACTION="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

MODE="$(echo "$MODE" | tr '[:upper:]' '[:lower:]')"
DNS_ACTION="$(echo "$DNS_ACTION" | tr '[:upper:]' '[:lower:]')"
case "$MODE" in soft|enforce) ;; *)
  echo "nftables-egress-redirect: --mode must be soft|enforce (got: $MODE)" >&2
  exit 2
esac
case "$DNS_ACTION" in redirect|drop) ;; *)
  echo "nftables-egress-redirect: --dns-action must be redirect|drop (got: $DNS_ACTION)" >&2
  exit 2
esac

# Default DNS sink to proxy host IP (TAP gateway) when unset.
if [[ -z "$DNS_SINK_IP" ]]; then
  DNS_SINK_IP="$PROXY_IP"
fi

softfail() {
  local msg="$1"
  if [[ "$MODE" == "soft" ]]; then
    echo "nftables-egress-redirect: SoftFail: $msg" >&2
    exit 0
  fi
  echo "nftables-egress-redirect: Enforce: $msg" >&2
  exit 1
}

render_dns_nat() {
  if [[ "$DNS_ACTION" == "redirect" ]]; then
    cat <<NFT
    # Redirect guest DNS to host DNS sink (node-agent --egress-dns-sink).
    ip saddr ${GUEST_SUBNET} udp dport 53 redirect to :${DNS_SINK_PORT}
    ip saddr ${GUEST_SUBNET} tcp dport 53 redirect to :${DNS_SINK_PORT}
NFT
  fi
}

render_dns_filter() {
  if [[ "$DNS_ACTION" == "drop" ]]; then
    cat <<NFT
    # Drop guest DNS to public resolvers (anti-bypass); guests must use host DNS sink / TAP.
    ip saddr ${GUEST_SUBNET} udp dport 53 drop comment "asp_egress_dns_drop"
    ip saddr ${GUEST_SUBNET} tcp dport 53 drop comment "asp_egress_dns_drop"
NFT
  fi
}

render_input_dns() {
  if [[ "$DNS_ACTION" == "redirect" ]]; then
    cat <<NFT
    iifname "asp-*" udp dport ${DNS_SINK_PORT} accept comment "asp_guest_dns_sink"
    iifname "asp-*" tcp dport ${DNS_SINK_PORT} accept comment "asp_guest_dns_sink"
NFT
  fi
}

render() {
  cat <<NFT
# agent-sandbox-platform — guest egress, deny-by-default (table ${TABLE})
# mode=${MODE} dns_action=${DNS_ACTION} http_ports={ ${HTTP_PORTS} }
table ip ${TABLE} {
  chain antispoof {
    type filter hook prerouting priority raw; policy accept;
    # A guest may only send from the /30 routed back through its own TAP.
    iifname "asp-*" ip saddr != ${GUEST_SUBNET} drop comment "asp_guest_antispoof"
    iifname "asp-*" fib saddr . iif oif missing drop comment "asp_guest_antispoof"
  }
  chain prerouting {
    type nat hook prerouting priority dstnat; policy accept;
    # Local-net sessions are not named here. node-agent inserts, at the head
    # of this chain, "iifname <that TAP> ... return" for TCP 80/443 and DNS,
    # and deletes those rules on session clear. Re-applying this file removes
    # them; the running agent puts them back. Other guests keep the redirect.
    # Force guest HTTP(S) through host egress forward proxy.
    ip saddr ${GUEST_SUBNET} tcp dport { ${HTTP_PORTS} } redirect to :${PROXY_PORT}
$(render_dns_nat)
  }
  chain input {
    type filter hook input priority filter; policy accept;
    # Guest → host: only the proxy and the DNS sink (after the redirect above).
    iifname "asp-*" ct state established,related accept
    iifname "asp-*" tcp dport ${PROXY_PORT} accept comment "asp_guest_proxy"
$(render_input_dns)
    iifname "asp-*" drop comment "asp_guest_to_host_drop"
  }
  chain forward {
    type filter hook forward priority filter; policy accept;
$(render_dns_filter)
    # Local-net session: policy routing already sends this TAP only to its
    # own wg-asp device (or a blackhole).
    iifname "asp-*" oifname "wg-asp-*" accept comment "asp_local_net"
    oifname "asp-*" ct state established,related accept
    # Anything else from a guest (other ports, other guests) is dropped.
    iifname "asp-*" drop comment "asp_guest_forward_drop"
    ip saddr ${GUEST_SUBNET} drop comment "asp_guest_forward_drop"
    # Nothing outside opens a connection to a guest.
    oifname "asp-*" drop comment "asp_guest_inbound_drop"
  }
}
table ip6 ${TABLE} {
  # Guests get no IPv6 address from ASP; still drop link-local access to host
  # services. Only a local-net session's own tunnel is forwarded.
  chain input {
    type filter hook input priority filter; policy accept;
    iifname "asp-*" drop comment "asp_guest_to_host_drop"
  }
  chain forward {
    type filter hook forward priority filter; policy accept;
    iifname "asp-*" oifname "wg-asp-*" accept comment "asp_local_net"
    oifname "asp-*" ct state established,related accept
    iifname "asp-*" drop comment "asp_guest_forward_drop"
    oifname "asp-*" drop comment "asp_guest_inbound_drop"
  }
}
NFT
}

need_root_nft() {
  if [[ "$(id -u)" -ne 0 ]]; then
    softfail "need root / CAP_NET_ADMIN to ${1}"
  fi
  if ! command -v nft >/dev/null 2>&1; then
    softfail "nft binary not found (install nftables)"
  fi
}

case "${ACTION}" in
  dry-run|print)
    render
    ;;
  apply)
    need_root_nft apply
    # Idempotent: replace table atomically
    nft delete table ip "${TABLE}" 2>/dev/null || true
    nft delete table ip6 "${TABLE}" 2>/dev/null || true
    if ! render | nft -f -; then
      softfail "nft -f failed while applying tables ip/ip6 ${TABLE}"
    fi
    echo "applied table ip ${TABLE}: subnet=${GUEST_SUBNET} http={${HTTP_PORTS}}->:${PROXY_PORT} dns=${DNS_ACTION}/:${DNS_SINK_PORT} mode=${MODE}"
    echo "note: proxy must listen on ${PROXY_PORT}; DNS sink on ${DNS_SINK_IP}:${DNS_SINK_PORT} when dns_action=redirect"
    echo "Enforce requirements: root, nftables, TAP iface carrying ${GUEST_SUBNET}"
    ;;
  flush|remove|delete)
    need_root_nft flush
    nft delete table ip "${TABLE}" 2>/dev/null || true
    nft delete table ip6 "${TABLE}" 2>/dev/null || true
    echo "flushed tables ip/ip6 ${TABLE} (if present)"
    ;;
  *)
    echo "usage: $0 {dry-run|apply|flush} [--guest-subnet CIDR] [--proxy-ip IP] [--proxy-port PORT]" >&2
    echo "         [--dns-sink-ip IP] [--dns-sink-port PORT] [--http-ports 80,443,8080]" >&2
    echo "         [--table NAME] [--mode soft|enforce] [--dns-action redirect|drop]" >&2
    exit 2
    ;;
esac
