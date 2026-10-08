#!/usr/bin/env bash
# The node's egress rules (table asp_egress, scripts/nftables-egress-redirect.sh) in nftables for
# real, without a VM: who reaches the proxy and the DNS sink, what a guest can and cannot send,
# and what the rest of the network cannot do. KVM is not needed, only root, `ip`, `nft` and python3,
# so it runs in CI; scripts/smoke-egress-kvm.sh is the same thing with a real guest.
#
# Three network namespaces, none of them the host's:
#
#   [guest] vg0 10.66.0.2/30 --- asp-t0 10.66.0.1/30 [srv: the node] eth-n 10.99.0.1/24 fd99::1/64 --- vn0 10.99.0.2/24 fd99::2/64 [net]
#
# The node's interface to the guest is called asp-t0, like a guest TAP (the rules match asp-*). srv has the
# four ports a node has: the proxy (8888) and the DNS sink (5353, TCP and UDP), which are bound to every address
# like the node-agent binds them, and another service of the host (9999). net plays the network the node is on
# and the Internet: it is where an attacker comes from, and where the origin of 80, 443, 53 and 22222 is.
#
# Usage:  sudo scripts/smoke-egress-nft.sh
# ASP_NFT_SMOKE_SCRIPT names another copy of the rules to try (the checks that guard the proxy fail on an old one).
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RULES="${ASP_NFT_SMOKE_SCRIPT:-$ROOT/scripts/nftables-egress-redirect.sh}"
GST=aspnft-guest SRV=aspnft-srv NET=aspnft-net
[[ $EUID -eq 0 ]] || { echo "needs root (network namespaces, nftables)" >&2; exit 2; }
for c in ip nft python3 bash; do command -v "$c" >/dev/null || { echo "missing command: $c" >&2; exit 2; }; done
[[ -r "$RULES" ]] || { echo "missing: $RULES" >&2; exit 2; }

WORK="$(mktemp -d)"
pids=()
cleanup() {
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null; done
  ip netns del "$GST" 2>/dev/null; ip netns del "$SRV" 2>/dev/null; ip netns del "$NET" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT
ip netns del "$GST" 2>/dev/null; ip netns del "$SRV" 2>/dev/null; ip netns del "$NET" 2>/dev/null

cat >"$WORK/serve.py" <<'PYEOF'
# serve.py proto:port:name ...   each port answers its name, over IPv4 and IPv6
import socket, sys, threading
def tcp(port, name):
    s = socket.socket(socket.AF_INET6, socket.SOCK_STREAM); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 0); s.bind(("::", port)); s.listen(16)
    while True:
        c, _ = s.accept(); c.sendall(name.encode()); c.close()
def udp(port, name):
    s = socket.socket(socket.AF_INET6, socket.SOCK_DGRAM); s.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 0); s.bind(("::", port))
    while True:
        _, a = s.recvfrom(100); s.sendto(name.encode(), a)
for spec in sys.argv[1:]:
    proto, port, name = spec.split(":")
    threading.Thread(target=tcp if proto == "tcp" else udp, args=(int(port), name), daemon=True).start()
threading.Event().wait()
PYEOF
cat >"$WORK/probe.py" <<'PYEOF'
# probe.py proto host port [source]   prints what answered, or "closed"
import socket, sys
proto, host, port = sys.argv[1], sys.argv[2], int(sys.argv[3])
src = sys.argv[4] if len(sys.argv) > 4 else None
try:
    s = socket.socket(socket.AF_INET6 if ":" in host else socket.AF_INET, socket.SOCK_STREAM if proto == "tcp" else socket.SOCK_DGRAM)
    s.settimeout(1.5)
    if src:
        s.bind((src, 0))
    if proto == "tcp":
        s.connect((host, port)); print(s.recv(32).decode() or "closed")
    else:
        s.sendto(b"x", (host, port)); print(s.recvfrom(32)[0].decode())
except Exception:
    print("closed")
PYEOF

in_g() { ip netns exec "$GST" "$@"; }
in_s() { ip netns exec "$SRV" "$@"; }
in_n() { ip netns exec "$NET" "$@"; }
probe() { # <netns> <proto> <host> <port> [source]
  local ns=$1; shift
  ip netns exec "$ns" python3 "$WORK/probe.py" "$@"
}

ip netns add "$GST"; ip netns add "$SRV"; ip netns add "$NET"
ip link add asp-t0 type veth peer name vg0; ip link set asp-t0 netns "$SRV"; ip link set vg0 netns "$GST"
ip link add eth-n type veth peer name vn0;  ip link set eth-n netns "$SRV";  ip link set vn0 netns "$NET"
for ns in "$GST" "$SRV" "$NET"; do ip netns exec "$ns" ip link set lo up; done
in_s ip addr add 10.66.0.1/30 dev asp-t0; in_s ip link set asp-t0 up
in_g ip addr add 10.66.0.2/30 dev vg0; in_g ip link set vg0 up; in_g ip route add default via 10.66.0.1
in_s ip addr add 10.99.0.1/24 dev eth-n; in_s ip addr add fd99::1/64 dev eth-n nodad; in_s ip link set eth-n up
in_n ip addr add 10.99.0.2/24 dev vn0; in_n ip addr add fd99::2/64 dev vn0 nodad; in_n ip link set vn0 up
in_n ip route add 10.66.0.0/30 via 10.99.0.1
in_s sysctl -qw net.ipv4.ip_forward=1
# a source address the guest was never given, to try to borrow
in_g ip addr add 10.66.7.7/32 dev vg0

# `ip netns exec` directly, not through the in_* functions: $! has to be the server, not a subshell
# around it, or the trap kills the subshell and the server outlives the script.
ip netns exec "$SRV" python3 "$WORK/serve.py" tcp:8888:proxy tcp:5353:sink udp:5353:sink tcp:9999:host udp:9999:host >/dev/null 2>&1 & pids+=($!)
ip netns exec "$NET" python3 "$WORK/serve.py" tcp:80:origin tcp:443:origin udp:53:origin tcp:53:origin tcp:22222:origin >/dev/null 2>&1 & pids+=($!)
ip netns exec "$GST" python3 "$WORK/serve.py" tcp:7777:guest >/dev/null 2>&1 & pids+=($!)
sleep 1

# The rules as the node-agent applies them: the script's own output, loaded into srv.
"$RULES" dry-run --guest-subnet 10.66.0.0/16 --proxy-ip 10.66.0.1 --proxy-port 8888 --dns-sink-port 5353 --http-ports 80,443 \
  --mode enforce >"$WORK/rules.nft" || { echo "the rules did not render" >&2; exit 1; }
in_s nft -f "$WORK/rules.nft" || { echo "the rules did not load" >&2; exit 1; }

bad=0
want() { # <label> <expected> <got>
  if [[ "$2" == "$3" ]]; then printf '  ok   %-64s %s\n' "$1" "$3"; else printf '  FAIL %-64s wanted %s, got %s\n' "$1" "$2" "$3"; bad=1; fi
}

echo "==> a guest: its web and DNS traffic goes to the node's proxy and sink, whatever it dials"
want "tcp 80 to the origin lands on the proxy" proxy "$(probe "$GST" tcp 10.99.0.2 80)"
want "tcp 443 to the origin lands on the proxy" proxy "$(probe "$GST" tcp 10.99.0.2 443)"
want "udp 53 to a resolver lands on the sink" sink "$(probe "$GST" udp 10.99.0.2 53)"
want "tcp 53 to a resolver lands on the sink" sink "$(probe "$GST" tcp 10.99.0.2 53)"
echo "==> a guest: nothing else"
want "another port of the origin: dropped" closed "$(probe "$GST" tcp 10.99.0.2 22222)"
want "a service of the host that is not the proxy: dropped" closed "$(probe "$GST" tcp 10.66.0.1 9999)"
want "udp to a service of the host: dropped" closed "$(probe "$GST" udp 10.66.0.1 9999)"
want "the proxy itself, directly" proxy "$(probe "$GST" tcp 10.66.0.1 8888)"
want "the sink itself, udp" sink "$(probe "$GST" udp 10.66.0.1 5353)"
want "an address outside the guest subnet: dropped" closed "$(probe "$GST" tcp 10.66.0.1 8888 192.0.2.7)"
want "an address of the subnet that is not routed to this TAP: dropped" closed "$(probe "$GST" tcp 10.66.0.1 8888 10.66.7.7)"
echo "==> nothing outside opens a connection to a guest"
want "the network to a service in the guest: dropped" closed "$(probe "$NET" tcp 10.66.0.2 7777)"
echo "==> the node itself"
want "loopback to the proxy" proxy "$(in_s python3 "$WORK/probe.py" tcp 127.0.0.1 8888)"
want "loopback to the sink, udp" sink "$(in_s python3 "$WORK/probe.py" udp 127.0.0.1 5353)"
echo "==> the network the node is on: the proxy and the sink are not for it, over IPv4 or IPv6"
want "proxy, tcp" closed "$(probe "$NET" tcp 10.99.0.1 8888)"
want "sink, tcp" closed "$(probe "$NET" tcp 10.99.0.1 5353)"
want "sink, udp" closed "$(probe "$NET" udp 10.99.0.1 5353)"
want "proxy, tcp, IPv6" closed "$(probe "$NET" tcp fd99::1 8888)"
want "sink, tcp, IPv6" closed "$(probe "$NET" tcp fd99::1 5353)"
want "sink, udp, IPv6" closed "$(probe "$NET" udp fd99::1 5353)"
echo "==> and the host's own services are the host's business, not the table's"
want "another port, tcp" host "$(probe "$NET" tcp 10.99.0.1 9999)"
want "another port, udp" host "$(probe "$NET" udp 10.99.0.1 9999)"
want "another port, tcp, IPv6" host "$(probe "$NET" tcp fd99::1 9999)"

echo
if (( bad )); then echo "FAIL smoke-egress-nft"; exit 1; fi
echo "OK smoke-egress-nft"
