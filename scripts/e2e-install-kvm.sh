#!/usr/bin/env bash
# The installer on clean machines, end to end: a release goes onto a clean Ubuntu 24.04 and a clean
# Debian 12 (the vendors' cloud images, booted by Cloud Hypervisor with nested KVM, under real systemd),
# a first session runs on the Ubuntu one, the Debian one joins it as a second node, and both are
# taken away again. It is the acceptance of the one-host and join flows (#124, #128) and what the
# release checklist calls "a clean node installs the package" (docs/how-to/release.md).
#
#   sudo ASP_INSTALL_E2E_DIST=dist ASP_INSTALL_E2E_GUEST=build/guest scripts/e2e-install-kvm.sh
#
# What it takes:
#   ASP_INSTALL_E2E_DIST      the goreleaser output with the linux/amd64 packages (make snapshot; default dist)
#   ASP_INSTALL_E2E_GUEST     the guest kernel and image (scripts/build-guest-image.sh --kernel; default build/guest)
#   ASP_INSTALL_E2E_RELEASE   instead of the two above: a directory laid out as a release (the packages,
#                             SHA256SUMS, install.sh, uninstall.sh, killall.sh and asp-guest_<version>_*)
#   ASP_INSTALL_E2E_VERSION   its version (default: the one in the names of the packages)
#   ASP_INSTALL_E2E_DISTROS   which machines: "ubuntu debian" (default), or "ubuntu" for the one host alone
#   ASP_INSTALL_E2E_IMAGES    where the cloud images are kept (default /var/cache/asp-install-e2e); the missing
#                             ones are downloaded from the vendors and checked against their checksum lists
#   ASP_INSTALL_E2E_KEEP=1    on a failure leave the machines running and say how to reach them
#
# The host needs root, /dev/kvm with nested virtualization on, cloud-hypervisor, qemu-img, losetup, mkfs.vfat,
# ip (iproute2), python3, curl and ssh. The machines live in a network namespace of their own: a bridge, two
# TAPs (iet*, not the node-agent's asp-*), and a veth to the host for two services bound to its end only: a
# web server for the release and a forward proxy that lets them reach the distributions' mirrors and GitHub
# (the installer gets Cloud Hypervisor from the project's release and apt gets nftables). The host's
# firewall, Docker and any node-agent it runs are not touched, and everything is removed at the end.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RELEASE="${ASP_INSTALL_E2E_RELEASE:-}"
DIST="${ASP_INSTALL_E2E_DIST:-$ROOT/dist}"
GUEST="${ASP_INSTALL_E2E_GUEST:-$ROOT/build/guest}"
VERSION="${ASP_INSTALL_E2E_VERSION:-}"
DISTROS="${ASP_INSTALL_E2E_DISTROS:-ubuntu debian}"
IMAGES="${ASP_INSTALL_E2E_IMAGES:-/var/cache/asp-install-e2e}"
WORK="${ASP_INSTALL_E2E_DIR:-/var/tmp/asp-install-e2e}"
CH="${ASP_INSTALL_E2E_CH:-cloud-hypervisor}"
VCPUS="${ASP_INSTALL_E2E_VCPUS:-4}"
MEM="${ASP_INSTALL_E2E_MEM:-4G}"
KEEP="${ASP_INSTALL_E2E_KEEP:-}"

NS=aspinst
HOST_IP=10.78.0.1   # the host's end of the veth: the proxy and the release server listen here
NS_IP=10.78.0.2
GW=10.77.0.1        # the bridge, inside the namespace: the machines' gateway
UBU_IP=10.77.0.11
DEB_IP=10.77.0.12
PROXY_PORT=3128
REL_PORT=8000
REL="http://$HOST_IP:$REL_PORT"
PROXY="http://$HOST_IP:$PROXY_PORT"

UBU_IMG=noble-server-cloudimg-amd64.img
UBU_URL=https://cloud-images.ubuntu.com/noble/current
DEB_IMG=debian-12-genericcloud-amd64.qcow2
DEB_URL=https://cloud.debian.org/images/cloud/bookworm/latest

fail() { echo "FAIL: $*" >&2; exit 1; }
say() { echo "==> $*"; }
want() { # label pattern answer: the line that matched, when it is not an empty one
  if grep -qE -- "$2" <<<"$3"; then
    local hit; hit=$(grep -m1 -E -- "$2" <<<"$3" | sed 's/^ *//' || true)
    echo "  ok   $1 -> $(head -c 80 <<<"${hit:-$3}" | tr '\n' ' ')"
  else
    fail "$1: wanted /$2/, got: $3"
  fi
}
wants_distro() { [[ " $DISTROS " == *" $1 "* ]]; }

# ---- what the host needs
[[ $EUID -eq 0 ]] || { echo "needs root (a network namespace, TAPs, loop devices)" >&2; exit 2; }
for c in "$CH" qemu-img losetup mkfs.vfat ip python3 curl ssh ssh-keygen sha256sum sha512sum; do
  command -v "$c" >/dev/null || { echo "missing command: $c" >&2; exit 2; }
done
[[ -c /dev/kvm ]] || { echo "no /dev/kvm" >&2; exit 2; }
nested=$(cat /sys/module/kvm_intel/parameters/nested /sys/module/kvm_amd/parameters/nested 2>/dev/null | head -1 || true)
[[ "$nested" == Y || "$nested" == 1 ]] || { echo "nested virtualization is off (kvm_intel.nested / kvm_amd.nested): the machines could not run microVMs" >&2; exit 2; }
for d in $DISTROS; do [[ $d == ubuntu || $d == debian ]] || { echo "ASP_INSTALL_E2E_DISTROS: $d is not ubuntu or debian" >&2; exit 2; }; done
wants_distro ubuntu || { echo "the Ubuntu machine is the control plane: ASP_INSTALL_E2E_DISTROS must include ubuntu" >&2; exit 2; }

# ---- cleanup, always
pids=()
vms=()
cleanup() {
  local rc=$?
  set +e
  if [[ -n "$KEEP" && $rc -ne 0 ]]; then
    echo "kept (ASP_INSTALL_E2E_KEEP): ssh with: ip netns exec $NS ssh -i $WORK/key -o StrictHostKeyChecking=no ubuntu@$UBU_IP   (debian@$DEB_IP)" >&2
    echo "logs: $WORK/*.log; teardown: ip link del iev-h; ip netns del $NS; kill ${pids[*]:-}; rm -rf $WORK" >&2
    return
  fi
  for v in "${vms[@]:-}"; do
    [[ -n "$v" && -S "$WORK/$v/ch.sock" ]] && curl -s --max-time 5 --unix-socket "$WORK/$v/ch.sock" -X PUT http://localhost/api/v1/vmm.shutdown >/dev/null 2>&1
  done
  for p in "${pids[@]:-}"; do [[ -n "$p" ]] && kill "$p" 2>/dev/null; done
  sleep 1
  ip link del iev-h 2>/dev/null
  ip netns del "$NS" 2>/dev/null
  if [[ $rc -ne 0 ]]; then
    for v in "${vms[@]:-}"; do echo "--- last of the console of $v:" >&2; tail -n 15 "$WORK/$v.serial" 2>/dev/null >&2; done
  fi
  rm -rf "$WORK"
  return $rc
}
trap cleanup EXIT

rm -rf "$WORK"
mkdir -p "$WORK" "$IMAGES"

# ---- the release directory the machines install from
if [[ -z "$RELEASE" ]]; then
  say "a release directory from $DIST and $GUEST"
  deb=$(ls "$DIST"/asp_*_linux_amd64.deb 2>/dev/null | head -1 || true)
  [[ -n "$deb" ]] || { echo "no asp_*_linux_amd64.deb in $DIST (make snapshot)" >&2; exit 2; }
  : "${VERSION:=$(basename "$deb" | sed -E 's/^asp_(.*)_linux_amd64\.deb$/\1/')}"
  for f in rootfs.img.gz image.json SHA256SUMS vmlinux; do
    [[ -s "$GUEST/$f" ]] || { echo "no $GUEST/$f (scripts/build-guest-image.sh --kernel)" >&2; exit 2; }
  done
  RELEASE="$WORK/release"
  mkdir -p "$RELEASE"
  for f in "$DIST"/*_"$VERSION"_linux_amd64.deb "$DIST"/*_"$VERSION"_linux_amd64.tar.gz "$DIST/SHA256SUMS"; do ln -s "$(cd "$(dirname "$f")" && pwd)/$(basename "$f")" "$RELEASE/"; done
  for f in install.sh uninstall.sh killall.sh; do cp "$ROOT/scripts/$f" "$RELEASE/$f"; done
  for f in rootfs.img.gz image.json SHA256SUMS vmlinux; do ln -s "$(cd "$GUEST" && pwd)/$f" "$RELEASE/asp-guest_${VERSION}_$f"; done
else
  : "${VERSION:=$(ls "$RELEASE"/asp_*_linux_amd64.deb | head -1 | xargs basename | sed -E 's/^asp_(.*)_linux_amd64\.deb$/\1/')}"
fi
[[ -n "$VERSION" ]] || { echo "cannot tell the version: set ASP_INSTALL_E2E_VERSION" >&2; exit 2; }
(cd "$RELEASE" && sha256sum -c SHA256SUMS --ignore-missing >/dev/null) || fail "the release does not match its SHA256SUMS"
say "release $VERSION"

# ---- the cloud images
fetch_image() { # image base-url vendor-sums-file sum-tool
  local img=$1 url=$2 sums=$3 tool=$4
  if [[ ! -s "$IMAGES/$img" ]]; then
    say "downloading $img"
    curl -fsSL -o "$IMAGES/$img.sums" "$url/$sums" || fail "cannot download $url/$sums"
    curl -fSL -o "$IMAGES/$img.part" "$url/$img" || fail "cannot download $url/$img"
    mv "$IMAGES/$img.part" "$IMAGES/$img"
  fi
  [[ -s "$IMAGES/$img.sums" ]] || curl -fsSL -o "$IMAGES/$img.sums" "$url/$sums" || fail "cannot download $url/$sums"
  (cd "$IMAGES" && grep -E " \*?$img\$" "$img.sums" | $tool -c - >/dev/null) || fail "$img does not match the vendor's $sums"
}
fetch_image "$UBU_IMG" "$UBU_URL" SHA256SUMS sha256sum
if wants_distro debian; then fetch_image "$DEB_IMG" "$DEB_URL" SHA512SUMS sha512sum; fi

# ---- the network: a namespace with a bridge and the machines' TAPs, a veth to the host
say "the network"
ip netns add "$NS"
ip link add iev-h type veth peer name iev-n
ip link set iev-n netns "$NS"
ip addr add "$HOST_IP/30" dev iev-h && ip link set iev-h up
ip netns exec "$NS" ip link set lo up
ip netns exec "$NS" ip addr add "$NS_IP/30" dev iev-n && ip netns exec "$NS" ip link set iev-n up
ip netns exec "$NS" ip link add iebr0 type bridge
ip netns exec "$NS" ip addr add "$GW/24" dev iebr0 && ip netns exec "$NS" ip link set iebr0 up
for t in ietu ietd; do
  ip netns exec "$NS" ip tuntap add dev $t mode tap
  ip netns exec "$NS" ip link set $t master iebr0 && ip netns exec "$NS" ip link set $t up
done
ip netns exec "$NS" sysctl -qw net.ipv4.ip_forward=1
ip route add 10.77.0.0/24 via "$NS_IP"

# the release, and a forward proxy for the mirrors and GitHub, both on the host's end of the veth only
cat >"$WORK/proxy.py" <<'PY'
import socket, sys, threading, urllib.parse
HOST, PORT = sys.argv[1], int(sys.argv[2])
ALLOW = ("debian.org", "ubuntu.com", "github.com", "githubusercontent.com")
def allowed(h):
    h = (h or "").lower().rstrip(".")
    return any(h == a or h.endswith("." + a) for a in ALLOW)
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
                c.sendall(b"HTTP/1.1 403 Forbidden\r\n\r\n"); print("deny CONNECT", target, flush=True); return
            u = socket.create_connection((host, int(port)), timeout=30)
            c.sendall(b"HTTP/1.1 200 Connection established\r\n\r\n")
        else:
            p = urllib.parse.urlsplit(target)
            if not allowed(p.hostname):
                c.sendall(b"HTTP/1.1 403 Forbidden\r\n\r\n"); print("deny", method, target, flush=True); return
            u = socket.create_connection((p.hostname, p.port or 80), timeout=30)
            path = (p.path or "/") + ("?" + p.query if p.query else "")
            req = f"{method} {path} HTTP/1.1\r\n".encode()
            for h in headers:
                if h.split(b":", 1)[0].lower() in (b"proxy-connection", b"connection"): continue
                req += h
            u.sendall(req + b"Connection: close\r\n\r\n")
        u.settimeout(None)
        t = threading.Thread(target=pipe, args=(u, c), daemon=True); t.start()
        pipe(c, u); t.join(timeout=5)
    except Exception as e:
        print("error", e, flush=True)
    finally:
        for s in (u, c):
            try:
                if s: s.close()
            except OSError: pass
srv = socket.socket(); srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind((HOST, PORT)); srv.listen(64)
while True:
    conn, _ = srv.accept()
    threading.Thread(target=handle, args=(conn,), daemon=True).start()
PY
python3 "$WORK/proxy.py" "$HOST_IP" "$PROXY_PORT" >"$WORK/proxy.log" 2>&1 &
pids+=($!)
# (not python3 -m http.server: its HTTPServer looks the address up in the DNS before it listens, and on a host
# whose resolver is slow for a private address that is seconds of connections refused)
cat >"$WORK/relsrv.py" <<'PY'
import functools, http.server, socketserver, sys
class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True
handler = functools.partial(http.server.SimpleHTTPRequestHandler, directory=sys.argv[3])
Server((sys.argv[1], int(sys.argv[2])), handler).serve_forever()
PY
python3 "$WORK/relsrv.py" "$HOST_IP" "$REL_PORT" "$RELEASE" >"$WORK/release.log" 2>&1 &
pids+=($!)
for _ in $(seq 1 100); do curl -fsS -o /dev/null "$REL/SHA256SUMS" 2>/dev/null && break; sleep 0.1; done
curl -fsS -o /dev/null "$REL/SHA256SUMS" || fail "the release server did not start: $(cat "$WORK/release.log")"

# ---- the machines
ssh-keygen -q -t ed25519 -N "" -f "$WORK/key" -C asp-install-e2e
vm_ssh() { # name command...  (stdin is the caller's)
  local name=$1 user ip; shift
  if [[ $name == ubu ]]; then user=ubuntu ip=$UBU_IP; else user=debian ip=$DEB_IP; fi
  ip netns exec "$NS" ssh -i "$WORK/key" -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR -o ConnectTimeout=10 -o ServerAliveInterval=15 "$user@$ip" "$@"
}

boot_vm() { # name image user ip mac tap
  local name=$1 img=$2 user=$3 ip=$4 mac=$5 tap=$6 d="$WORK/$1" loop k r p
  mkdir -p "$d/mnt"
  say "booting the $name machine ($img)"
  qemu-img convert -f qcow2 -O raw "$IMAGES/$img" "$d/disk.raw"
  qemu-img resize -f raw "$d/disk.raw" 16G >/dev/null
  # Cloud Hypervisor boots the image's own kernel and initrd directly. Ubuntu's cloud image keeps /boot on a
  # partition of its own, Debian's on the root partition.
  loop=$(losetup --find --show -P "$d/disk.raw")
  for _ in $(seq 1 50); do [[ -b "${loop}p1" ]] && break; sleep 0.1; done
  k=""
  for p in $(ls "${loop}"p* 2>/dev/null | sed "s|^${loop}p||" | sort -n); do
    [[ $p == 14 ]] && continue
    mount -o ro "${loop}p$p" "$d/mnt" 2>/dev/null || continue
    k=$(ls -1 "$d"/mnt/boot/vmlinuz-* "$d"/mnt/vmlinuz-* 2>/dev/null | sort -V | tail -1 || true)
    if [[ -n "$k" ]]; then r=$(ls -1 "$(dirname "$k")"/initrd.img-* | sort -V | tail -1); cp "$k" "$d/vmlinuz"; cp "$r" "$d/initrd.img"; umount "$d/mnt"; break; fi
    umount "$d/mnt"
  done
  losetup -d "$loop"
  [[ -s "$d/vmlinuz" ]] || fail "no kernel in $img"
  # cloud-init: the key, the address (the machines have no DHCP), a proxy for apt, and a patient dpkg lock
  cat >"$d/user-data" <<UD
#cloud-config
hostname: asp-$name
manage_etc_hosts: true
ssh_pwauth: false
disable_root: true
users:
  - name: $user
    shell: /bin/bash
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh_authorized_keys:
      - $(cat "$WORK/key.pub")
write_files:
  - path: /etc/apt/apt.conf.d/99e2e
    content: |
      Acquire::http::Proxy "$PROXY";
      Acquire::https::Proxy "$PROXY";
      DPkg::Lock::Timeout "180";
package_update: false
package_upgrade: false
UD
  printf 'instance-id: asp-%s-1\nlocal-hostname: asp-%s\n' "$name" "$name" >"$d/meta-data"
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
  : >"$WORK/$name.serial"
  ip netns exec "$NS" "$CH" --kernel "$d/vmlinuz" --initramfs "$d/initrd.img" \
    --cmdline "root=/dev/vda1 ro console=ttyS0 net.ifnames=0" \
    --disk "path=$d/disk.raw,image_type=raw" "path=$d/seed.img,image_type=raw,readonly=on" \
    --cpus "boot=$VCPUS,nested=on" --memory "size=$MEM" --net "tap=$tap,mac=$mac" \
    --serial "file=$WORK/$name.serial" --console off --api-socket "path=$d/ch.sock" \
    >"$WORK/$name.ch.log" 2>&1 &
  pids+=($!)
  vms+=("$name")
  for _ in $(seq 1 60); do vm_ssh "$name" true </dev/null 2>/dev/null && break; sleep 3; done
  vm_ssh "$name" "cloud-init status --wait >/dev/null 2>&1; test -c /dev/kvm" </dev/null || fail "the $name machine did not come up, or has no /dev/kvm (nested virtualization)"
}

# the installer, as the documentation gives it, with the release and the proxy of this lab
installer() { # vm role [VAR=value...]
  local vm=$1 role=$2; shift 2
  vm_ssh "$vm" "curl -fsSL $REL/install.sh | sudo env https_proxy=$PROXY http_proxy=$PROXY no_proxy=$HOST_IP INSTALL_ASP_URL=$REL INSTALL_ASP_VERSION=$VERSION INSTALL_ASP_ROLE=$role $* sh" </dev/null 2>&1
}

boot_vm ubu "$UBU_IMG" ubuntu "$UBU_IP" 52:54:00:77:00:11 ietu
wants_distro debian && boot_vm deb "$DEB_IMG" debian "$DEB_IP" 52:54:00:77:00:12 ietd

# ---- Ubuntu: everything on one host, three commands
say "Ubuntu: curl | sh with the role standalone"
out=$(installer ubu standalone) || { echo "$out" >&2; fail "the installer failed"; }
want "the installer says the control plane and the node are up" 'the control plane and the node are up' "$out"
want "Cloud Hypervisor came with it" 'installing Cloud Hypervisor' "$out"
want "the guest image came with it" 'installed .* in /var/lib/asp/images' "$out"
if grep -q "install-asp:" <<<"$out"; then echo "  note: the installer warned:"; grep "install-asp:" <<<"$out" | sed 's/^/    /'; fi
doctor=$(vm_ssh ubu "sudo asp doctor" </dev/null 2>&1 || true)
want "asp doctor finds nothing wrong" '[0-9]+ ok, [0-9]+ warn, 0 fail' "$doctor"
want "the units are active" '^active$' "$(vm_ssh ubu 'systemctl is-active asp-server' </dev/null)"
want "asp session start" 'session started' "$(vm_ssh ubu 'sudo asp session start' </dev/null 2>&1)"
want "asp session exec -- id" 'uid=[0-9]+' "$(vm_ssh ubu 'sudo asp session exec -- id' </dev/null 2>&1)"
want "the guest is the project's kernel" '6\.18\..*-asp|Linux' "$(vm_ssh ubu 'sudo asp session exec -- uname -r' </dev/null 2>&1)"
# a workspace: the guest sees a file of the host as its owner, and what it writes reaches the host
vm_ssh ubu "sudo mkdir -p /srv/asp/workspaces/default/proj && echo from-host | sudo tee /srv/asp/workspaces/default/proj/host.txt >/dev/null && sudo chown -R 1234:1234 /srv/asp/workspaces/default/proj" </dev/null
vm_ssh ubu "sudo asp session start --name ws --workspace /srv/asp/workspaces/default/proj" </dev/null >/dev/null 2>&1 || fail "a session with a workspace did not start"
want "a workspace is shared (as its owner)" 'uid=1234' "$(vm_ssh ubu 'sudo asp session exec --name ws -- sh -c "id; cat /workspace/host.txt; echo from-guest > /workspace/guest.txt"' </dev/null 2>&1)"
want "and what the guest wrote reaches the host" 'from-guest' "$(vm_ssh ubu 'sudo cat /srv/asp/workspaces/default/proj/guest.txt' </dev/null 2>&1)"
vm_ssh ubu "sudo asp session rm --name ws" </dev/null >/dev/null 2>&1 || true

# ---- Debian: joins as a second node
if wants_distro debian; then
  say "Debian: the second host joins"
  vm_ssh ubu "printf 'listen: 0.0.0.0:8443\ntls_san: [$UBU_IP]\n' | sudo tee /etc/asp/standalone.yaml.d/10-site.yaml >/dev/null; sudo chmod 600 /etc/asp/standalone.yaml.d/10-site.yaml; echo '$DEB_IP asp-deb' | sudo tee -a /etc/hosts >/dev/null; sudo systemctl restart asp-server" </dev/null
  for _ in $(seq 1 40); do vm_ssh ubu "sudo asp node list" </dev/null 2>/dev/null | grep -q ' ready ' && break; sleep 2; done
  tok_out=$(vm_ssh ubu "sudo asp node enroll-token --node-id asp-deb" </dev/null 2>&1)
  token=$(grep -m1 '^asp_enroll_' <<<"$tok_out")
  fp=$(grep -o 'INSTALL_ASP_CA_SHA256=[0-9a-f]*' <<<"$tok_out" | head -1 | cut -d= -f2)
  [[ -n "$token" && -n "$fp" ]] || fail "asp node enroll-token printed no token or fingerprint"
  out=$(installer deb agent "INSTALL_ASP_SERVER=https://$UBU_IP:8443 INSTALL_ASP_TOKEN=$token INSTALL_ASP_NODE_ID=asp-deb INSTALL_ASP_CA_SHA256=$fp" | sed "s/$token/<token>/g") || { echo "$out" >&2; fail "the installer failed on the Debian machine"; }
  want "the node registered" 'the node registered' "$out"
  want "the enroll token left the disk" 'the enroll token is out of' "$out"
  want "nftables came with the package (Debian has none)" 'installed asp-node-agent' "$out"
  nodes=""
  for _ in $(seq 1 30); do nodes=$(vm_ssh ubu "sudo asp node list" </dev/null 2>&1); grep -qE 'asp-deb +ready +yes' <<<"$nodes" && break; sleep 2; done
  want "the control plane lists the second node as schedulable" 'asp-deb +ready +yes' "$nodes"
  want "a session placed on it starts" 'session started' "$(vm_ssh ubu 'sudo asp session start --name on-deb --node-id asp-deb' </dev/null 2>&1)"
  want "and runs a command, reached over mutual TLS" 'uid=[0-9]+' "$(vm_ssh ubu 'sudo asp session exec --name on-deb -- id' </dev/null 2>&1)"
  want "on the Debian node" 'node=asp-deb' "$(vm_ssh ubu 'sudo asp session status --name on-deb' </dev/null 2>&1)"
  doctor=$(vm_ssh deb "sudo asp doctor" </dev/null 2>&1 || true)
  want "asp doctor on the Debian node finds nothing wrong" '[0-9]+ ok, [0-9]+ warn, 0 fail' "$doctor"
  vm_ssh ubu "sudo asp session rm --name on-deb" </dev/null >/dev/null 2>&1 || true
fi

# ---- and away again
say "uninstall --purge leaves nothing"
for vm in ubu $(wants_distro debian && echo deb); do
  vm_ssh "$vm" "sudo /usr/local/bin/asp-uninstall.sh --purge --yes" </dev/null >/dev/null 2>&1 || fail "asp-uninstall.sh failed on $vm"
  left=$(vm_ssh "$vm" "dpkg -l 2>/dev/null | awk '/^ii +asp/ {print \$2}' | tr '\n' ' '; ls -d /etc/asp /var/lib/asp /var/lib/asp-control-plane 2>/dev/null | tr '\n' ' '; sudo nft list tables 2>/dev/null | grep asp_egress; systemctl list-units --no-legend 'asp-*' | awk '{print \$1}' | tr '\n' ' '" </dev/null 2>&1)
  want "nothing of ASP is left on $vm" '^[[:space:]]*$' "$left"
done

echo "OK e2e-install-kvm"
