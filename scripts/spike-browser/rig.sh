#!/usr/bin/env bash
# The rig of the browser spike (#231) on ncc1701d, the one of the Kubernetes spike (#143) cut to one machine: an Ubuntu
# 24.04 machine (the vendor's cloud image, booted by Cloud Hypervisor with nested KVM) in a network namespace of its own,
# and two services on the host's end of a veth: a forward proxy with an allowlist (the distributions, GitHub, the
# registries and the sites the browser tests load) and a file server for the ASP release. Nothing of the host's network,
# firewall, Docker or ASP's production node is touched.
#
#   sudo rig.sh up                  the network, the proxy, the file server and the machine
#   sudo rig.sh ssh asp ...         a command on the machine
#   sudo rig.sh down [purge]        stop everything; purge also removes the disk
set -euo pipefail
W=/sandbox/spike-browser
NS=aspbrowser
HOST_IP=10.82.0.1; NS_IP=10.82.0.2; GW=10.81.0.1
PROXY_PORT=3129; REL_PORT=8001
PROXY="http://$HOST_IP:$PROXY_PORT"
IMG="$W/img/noble-server-cloudimg-amd64.img"
VCPUS="${SPIKE_VCPUS:-10}"; MEM="${SPIKE_MEM:-20G}"
CH=/usr/local/bin/cloud-hypervisor

ip_of() { case $1 in k3s) echo 10.81.0.11 ;; asp) echo 10.81.0.12 ;; e2b) echo 10.81.0.13 ;; *) echo "unknown machine $1" >&2; exit 2 ;; esac; }
mac_of() { case $1 in k3s) echo 52:54:00:81:00:11 ;; asp) echo 52:54:00:81:00:12 ;; e2b) echo 52:54:00:81:00:13 ;; esac; }
disk_of() { case $1 in k3s) echo 40G ;; asp) echo 40G ;; e2b) echo 48G ;; esac; }
mem_of() { case $1 in e2b) echo "${SPIKE_E2B_MEM:-12G}" ;; *) echo "$MEM" ;; esac; }

vm_ssh() { local name=$1; shift
  ip netns exec "$NS" ssh -i "$W/key" -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR -o ConnectTimeout=10 -o ServerAliveInterval=15 "ubuntu@$(ip_of "$name")" "$@"
}

start_bg() { # pidfile logfile cmd...: a process that outlives this script, its pid recorded
  local pidf=$1 log=$2; shift 2
  setsid nohup "$@" >"$log" 2>&1 </dev/null &
  echo $! >"$pidf"
}

net_up() {
  ip netns add "$NS"
  ip link add isv-h type veth peer name isv-n
  ip link set isv-n netns "$NS"
  ip addr add "$HOST_IP/30" dev isv-h && ip link set isv-h up
  ip netns exec "$NS" ip link set lo up
  ip netns exec "$NS" ip addr add "$NS_IP/30" dev isv-n && ip netns exec "$NS" ip link set isv-n up
  ip netns exec "$NS" ip link add isbr0 type bridge
  ip netns exec "$NS" ip addr add "$GW/24" dev isbr0 && ip netns exec "$NS" ip link set isbr0 up
  for t in istasp; do
    ip netns exec "$NS" ip tuntap add dev $t mode tap
    ip netns exec "$NS" ip link set $t master isbr0 && ip netns exec "$NS" ip link set $t up
  done
  ip netns exec "$NS" sysctl -qw net.ipv4.ip_forward=1
  ip route add 10.81.0.0/24 via "$NS_IP"
}

services_up() {
  cat >"$W/proxy.py" <<'PY'
import socket, sys, threading, urllib.parse, time
HOST, PORT = sys.argv[1], int(sys.argv[2])
ALLOW = ("debian.org", "ubuntu.com", "github.com", "githubusercontent.com", "ghcr.io",
         "docker.io", "docker.com", "quay.io", "registry.k8s.io", "pkg.dev", "googleapis.com",
         "amazonaws.com", "get.helm.sh",
         "example.com", "wikipedia.org", "wikimedia.org", "mozilla.org")
def allowed(h):
    h = (h or "").lower().rstrip(".")
    return any(h == a or h.endswith("." + a) for a in ALLOW)
def log(*a): print(time.strftime("%H:%M:%S"), *a, flush=True)
def pipe(a, b):
    try:
        while True:
            d = a.recv(65536)
            if not d: break
            b.sendall(d)
    except OSError: pass
    finally:
        try: b.shutdown(socket.SHUT_WR)
        except OSError: pass
def handle(c):
    u = None
    try:
        f = c.makefile("rb")
        parts = f.readline().decode("latin1").split()
        if len(parts) != 3: return
        method, target, _ = parts
        headers = []
        while True:
            h = f.readline()
            if h in (b"\r\n", b"\n", b""): break
            headers.append(h)
        if method == "CONNECT":
            host, _, port = target.rpartition(":")
            if not allowed(host):
                c.sendall(b"HTTP/1.1 403 Forbidden\r\n\r\n"); log("deny CONNECT", target); return
            u = socket.create_connection((host, int(port)), timeout=30)
            c.sendall(b"HTTP/1.1 200 Connection established\r\n\r\n"); log("CONNECT", target)
        else:
            p = urllib.parse.urlsplit(target)
            if not allowed(p.hostname):
                c.sendall(b"HTTP/1.1 403 Forbidden\r\n\r\n"); log("deny", method, target); return
            u = socket.create_connection((p.hostname, p.port or 80), timeout=30)
            path = (p.path or "/") + ("?" + p.query if p.query else "")
            req = f"{method} {path} HTTP/1.1\r\n".encode()
            for h in headers:
                if h.split(b":", 1)[0].lower() in (b"proxy-connection", b"connection"): continue
                req += h
            u.sendall(req + b"Connection: close\r\n\r\n"); log(method, p.hostname)
        u.settimeout(None)
        t = threading.Thread(target=pipe, args=(u, c), daemon=True); t.start()
        pipe(c, u); t.join(timeout=5)
    except Exception as e:
        log("error", e)
    finally:
        for s in (u, c):
            try:
                if s: s.close()
            except OSError: pass
srv = socket.socket(); srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind((HOST, PORT)); srv.listen(128)
while True:
    conn, _ = srv.accept()
    threading.Thread(target=handle, args=(conn,), daemon=True).start()
PY
  cat >"$W/relsrv.py" <<'PY'
import functools, http.server, socketserver, sys
class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True
handler = functools.partial(http.server.SimpleHTTPRequestHandler, directory=sys.argv[3])
Server((sys.argv[1], int(sys.argv[2])), handler).serve_forever()
PY
  mkdir -p "$W/release"
  start_bg "$W/proxy.pid" "$W/proxy.log" python3 "$W/proxy.py" "$HOST_IP" "$PROXY_PORT"
  start_bg "$W/relsrv.pid" "$W/relsrv.log" python3 "$W/relsrv.py" "$HOST_IP" "$REL_PORT" "$W/release"
  sleep 1
}

boot_vm() { # name
  local name=$1 d="$W/$1" ip mac loop k r p
  ip=$(ip_of "$name"); mac=$(mac_of "$name")
  mkdir -p "$d/mnt"
  if [[ ! -s "$d/disk.raw" ]]; then
    qemu-img convert -f qcow2 -O raw "$IMG" "$d/disk.raw"
    qemu-img resize -f raw "$d/disk.raw" "$(disk_of "$name")" >/dev/null
  fi
  loop=$(losetup --find --show -P "$d/disk.raw")
  for _ in $(seq 1 50); do [[ -b "${loop}p1" ]] && break; sleep 0.1; done
  for p in $(ls "${loop}"p* 2>/dev/null | sed "s|^${loop}p||" | sort -n); do
    [[ $p == 14 ]] && continue
    mount -o ro "${loop}p$p" "$d/mnt" 2>/dev/null || continue
    k=$(ls -1 "$d"/mnt/boot/vmlinuz-* "$d"/mnt/vmlinuz-* 2>/dev/null | sort -V | tail -1 || true)
    if [[ -n "$k" ]]; then r=$(ls -1 "$(dirname "$k")"/initrd.img-* | sort -V | tail -1); cp "$k" "$d/vmlinuz"; cp "$r" "$d/initrd.img"; umount "$d/mnt"; break; fi
    umount "$d/mnt"
  done
  losetup -d "$loop"
  [[ -s "$d/vmlinuz" ]] || { echo "no kernel in the image" >&2; exit 1; }
  cat >"$d/user-data" <<UD
#cloud-config
hostname: spike-$name
manage_etc_hosts: true
ssh_pwauth: false
disable_root: true
users:
  - name: ubuntu
    shell: /bin/bash
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh_authorized_keys:
      - $(cat "$W/key.pub")
write_files:
  - path: /etc/apt/apt.conf.d/99spike
    content: |
      Acquire::http::Proxy "$PROXY";
      Acquire::https::Proxy "$PROXY";
      DPkg::Lock::Timeout "180";
package_update: false
package_upgrade: false
UD
  printf 'instance-id: spike-%s-1\nlocal-hostname: spike-%s\n' "$name" "$name" >"$d/meta-data"
  cat >"$d/network-config" <<NC
version: 2
ethernets:
  nic0:
    match: {macaddress: "$mac"}
    set-name: eth0
    addresses: [$ip/24]
    routes: [{to: default, via: $GW}]
    nameservers: {addresses: [$GW]}
NC
  dd if=/dev/zero of="$d/seed.img" bs=1M count=4 status=none
  mkfs.vfat -n cidata "$d/seed.img" >/dev/null
  mount -o loop "$d/seed.img" "$d/mnt" && cp "$d/user-data" "$d/meta-data" "$d/network-config" "$d/mnt/" && umount "$d/mnt"
  : >"$d/serial"
  start_bg "$d/ch.pid" "$d/ch.log" ip netns exec "$NS" "$CH" --kernel "$d/vmlinuz" --initramfs "$d/initrd.img" \
    --cmdline "root=/dev/vda1 ro console=ttyS0 net.ifnames=0" \
    --disk "path=$d/disk.raw,image_type=raw" "path=$d/seed.img,image_type=raw,readonly=on" \
    --cpus "boot=$VCPUS,nested=on" --memory "size=$(mem_of "$name")" --net "tap=ist$name,mac=$mac" \
    --serial "file=$d/serial" --console off --api-socket "path=$d/ch.sock"
  for _ in $(seq 1 60); do vm_ssh "$name" true </dev/null 2>/dev/null && break; sleep 3; done
  vm_ssh "$name" "cloud-init status --wait >/dev/null 2>&1; test -c /dev/kvm" </dev/null || { echo "the $name machine did not come up, or has no /dev/kvm" >&2; tail -20 "$d/serial" >&2; exit 1; }
  echo "machine $name up at $ip"
}

kill_pidfile() { # pidfile expected-substring
  local f=$1 want=$2 p
  [[ -s $f ]] || return 0
  p=$(cat "$f")
  if tr '\0' ' ' </proc/"$p"/cmdline 2>/dev/null | grep -q -- "$want"; then kill "$p" 2>/dev/null || true; fi
  rm -f "$f"
}

case "${1:-}" in
  up)
    [[ $EUID -eq 0 ]] || { echo "needs root" >&2; exit 2; }
    [[ -s "$IMG" ]] || { echo "missing $IMG" >&2; exit 2; }
    [[ -s "$W/key" ]] || ssh-keygen -q -t ed25519 -N "" -f "$W/key" -C spike
    ip netns list | grep -qw "$NS" || net_up
    [[ -s "$W/proxy.pid" ]] || services_up
    for m in ${2:-asp}; do [[ -s "$W/$m/ch.pid" ]] && kill -0 "$(cat "$W/$m/ch.pid")" 2>/dev/null || boot_vm "$m"; done
    ;;
  ssh) shift; vm_ssh "$@" ;;
  tproxy) # the way out for ASP's egress proxy in the machine: 93.184.216.34:80/443 -> tproxy-host.py on this end
    for p in 80:8080 443:8443; do
      ip netns exec "$NS" iptables -t nat -C PREROUTING -d 93.184.216.34/32 -p tcp --dport "${p%:*}" -j DNAT --to-destination "$HOST_IP:${p#*:}" 2>/dev/null ||
        ip netns exec "$NS" iptables -t nat -A PREROUTING -d 93.184.216.34/32 -p tcp --dport "${p%:*}" -j DNAT --to-destination "$HOST_IP:${p#*:}"
    done
    [[ -s "$W/tproxy.pid" ]] && kill -0 "$(cat "$W/tproxy.pid")" 2>/dev/null || start_bg "$W/tproxy.pid" "$W/tproxy.log" python3 "$W/tproxy-host.py" "$HOST_IP"
    echo "tproxy up" ;;
  stop) # NAME: power the machine off from inside, keep its disk
    m=$2; vm_ssh "$m" "sudo systemctl poweroff" </dev/null >/dev/null 2>&1 || true
    for _ in $(seq 1 60); do [[ -s "$W/$m/ch.pid" ]] && kill -0 "$(cat "$W/$m/ch.pid")" 2>/dev/null || break; sleep 1; done
    kill_pidfile "$W/$m/ch.pid" cloud-hypervisor; rm -f "$W/$m/ch.sock"; echo "machine $m stopped" ;;
  down)
    for m in k3s asp e2b; do
      [[ -S "$W/$m/ch.sock" ]] && curl -s --max-time 5 --unix-socket "$W/$m/ch.sock" -X PUT http://localhost/api/v1/vmm.shutdown >/dev/null 2>&1 || true
      kill_pidfile "$W/$m/ch.pid" cloud-hypervisor
    done
    kill_pidfile "$W/proxy.pid" proxy.py
    kill_pidfile "$W/relsrv.pid" relsrv.py
    kill_pidfile "$W/tproxy.pid" tproxy-host.py
    ip route del 10.81.0.0/24 via "$NS_IP" 2>/dev/null || true
    ip link del isv-h 2>/dev/null || true
    ip netns del "$NS" 2>/dev/null || true
    if [[ "${2:-}" == purge ]]; then rm -rf "$W/k3s" "$W/asp" "$W/e2b" "$W/release"; fi
    echo "down"
    ;;
  *) echo "usage: $0 up [asp] | ssh NAME cmd... | tproxy | stop NAME | down [purge]" >&2; exit 2 ;;
esac
