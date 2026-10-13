#!/bin/bash
# In the asp machine of the browser spike (#231), as root, after rig-setup.sh: the desktop variant against the plain
# image, with ASP installed by its one-host installer. Each "RESULT key value" line goes to the ADR.
#   tests.sh plain       cold start of the plain image (the installed one)
#   tests.sh switch      install the desktop image set and make it current
#   tests.sh desktop     everything about the desktop image
#   tests.sh headless    the same browser with and without a screen
#   tests.sh density N M N desktop sandboxes of M MiB at once
set -uo pipefail
V=0.0.0-SNAPSHOT-bb9ad4a
W=/srv/asp/workspaces/default/browser
WIKI=https://en.wikipedia.org/wiki/Virtual_machine
ms() { local t=${EPOCHREALTIME/./}; echo $(( t / 1000 )); }
res() { echo "RESULT $*"; }
A() { asp "$@" </dev/null; }
stats() { local name=$1; shift; printf '%s\n' "$@" | sort -n | awk -v n="$name" '{a[NR]=$1} END {printf "RESULT %s_ms min=%d median=%d max=%d n=%d\n", n, a[1], a[int((NR+1)/2)], a[NR], NR}'; }
x() { local n=$1; shift; A session exec --name "$n" --buffered --exec-timeout 150s -- bash -c "$*" 2>&1; }
xr() { local n=$1; shift; A session exec --name "$n" --root --buffered --exec-timeout 150s -- bash -c "$*" 2>&1; }
sid() { python3 -c 'import json,sys,os; print(json.load(open(os.path.expanduser("~/.cache/asp/sessions/%s.json" % sys.argv[1])))["sandbox_id"])' "$1" 2>/dev/null; }
vm_mib() { local m; m=$(systemctl show -p MemoryCurrent --value "asp-vm-$(sid "$1").service" 2>/dev/null); [[ $m =~ ^[0-9]+$ ]] && echo $(( m / 1048576 )) || echo "?"; }
start() { A session start --name "$1" --memory-mib "$2" --workspace "$W" >/dev/null 2>&1; }
wait_exec() { # name [seconds, default 60]
  local end=$(( $(ms) + ${2:-60} * 1000 ))
  while (( $(ms) < end )); do A session exec --name "$1" -- true >/dev/null 2>&1 && return 0; sleep 0.1; done; return 1; }
cdp() { local n=$1; shift; x "$n" "python3 /workspace/cdp.py $*"; }
rmall() { for n in "$@"; do A session rm --name "$n" >/dev/null 2>&1; done; }
# The audit lines of the egress proxy for one sandbox since a time: the hosts it denied, counted.
denied_hosts() { # sandbox_id since
  journalctl -u asp-server --since "$2" --no-pager -o cat 2>/dev/null | grep -o '{.*egress_audit.*}\|egress_audit.*' | python3 -c '
import sys, json, re, collections
sid = sys.argv[1]; c = collections.Counter()
for l in sys.stdin:
    if sid not in l: continue
    l = l.replace("\\\"", "\"")   # the journal line carries the audit JSON with its quotes escaped
    m = re.search(r"\"host\":\"([^\"]+)\"", l)
    e = re.search(r"\"event\":\"([^\"]+)\"", l)
    pt = re.search(r"\"port\":(\d+)", l)
    if m and e and e.group(1) == "deny": c[m.group(1) + (":" + pt.group(1) if pt else "")] += 1
print(" ".join(f"{h} x{n}" for h, n in c.most_common()) or "none")' "$1"
}

mkdir -p "$W" && chown 1000:1000 "$W"
install -o 1000 -g 1000 -m 0755 /root/cdp.py "$W/cdp.py"

case "${1:-}" in
plain)
  echo "== plain image: cold start, 512 MiB (5)"
  t=(); for i in 1 2 3 4 5; do s=$(ms); start p$i 512; wait_exec p$i; t+=($(( $(ms) - s ))); [[ $i == 1 ]] && { sleep 20; res plain_vm_mib_idle "$(vm_mib p1)"; }; rmall p$i; done
  stats plain_cold "${t[@]}"
  start plat 512; wait_exec plat
  t=(); for i in $(seq 1 20); do s=$(ms); A session exec --name plat -- true >/dev/null 2>&1; t+=($(( $(ms) - s ))); done
  stats plain_exec_latency "${t[@]}"; rmall plat
  ;;
switch)
  s=$(ms); asp image pull --version "$V-desktop" --base-url http://10.82.0.1:8001 2>&1 | tail -2; res desktop_image_pull_ms $(( $(ms) - s ))
  res current_image "$(readlink /opt/sandbox/rootfs.img)"
  ;;
desktop)
  echo "== 1. cold start of the desktop image, 2048 MiB: to a command, and to the browser answering on DevTools (5)"
  t1=(); t2=()
  for i in 1 2 3 4 5; do
    s=$(ms); start d$i 2048; wait_exec d$i; t1+=($(( $(ms) - s )))
    r=$(cdp d$i wait --timeout 90); t2+=($(( $(ms) - s )))
    [[ $i == 1 ]] && { res desktop_cdp_wait "$r"; res desktop_browser_version "$(cdp d1 version | python3 -c 'import json,sys; print(json.load(sys.stdin).get("Browser"))' 2>&1)"; }
    rmall d$i
  done
  stats desktop_cold_to_exec "${t1[@]}"; stats desktop_cold_to_browser "${t2[@]}"

  echo "== 2. how much memory: start, load a Wikipedia article, look"
  for m in 512 768 1024 1536 2048; do
    n=mem$m; start $n $m; wait_exec $n || { res mem_${m} "no exec"; rmall $n; continue; }
    r=$(cdp $n wait --timeout 90); g=$(cdp $n goto "$WIKI" --timeout 60 | tail -1)
    sleep 5
    res mem_${m} "browser=[$r] page=$g guest_used_mib=$(x $n "free -m | awk '/^Mem:/{print \$3}'") vm_mib=$(vm_mib $n) oom=$(xr $n 'dmesg 2>/dev/null | grep -ci "out of memory\|oom-kill"')"
    [[ $m == 2048 ]] && res mem_top "$(x $n "ps -eo rss,comm --sort=-rss | head -12 | awk 'NR>1{printf \"%s=%dMiB \", \$2, \$1/1024}'")"
    rmall $n
  done

  echo "== 3. page loads at 1536 MiB, three of each, and a screenshot"
  start pg 1536; wait_exec pg; cdp pg wait --timeout 90 >/dev/null
  for u in https://example.com/ "$WIKI"; do
    for i in 1 2 3; do res load "$(cdp pg goto "$u" --timeout 60 | tail -1)"; done
  done
  cdp pg goto "$WIKI" --screenshot /workspace/wiki.png >/dev/null; res screenshot "$(ls -la $W/wiki.png 2>&1)"
  t=(); for i in $(seq 1 20); do s=$(ms); A session exec --name pg -- true >/dev/null 2>&1; t+=($(( $(ms) - s ))); done
  stats desktop_exec_latency "${t[@]}"

  echo "== 4. egress from the browser and from the guest"
  res egress_browser_denied "$(cdp pg goto https://www.google.com/ --timeout 30 | tail -1)"
  res egress_browser_denied_http "$(cdp pg goto http://neverssl.com/ --timeout 30 | tail -1)"
  res egress_dns_allowed "$(x pg 'getent hosts en.wikipedia.org || echo NXDOMAIN')"
  res egress_dns_denied "$(x pg 'getent hosts www.google.com || echo NXDOMAIN')"
  res egress_tls_around_proxy "$(x pg 'python3 -c "
import socket, ssl
try:
    s = socket.create_connection((\"1.1.1.1\", 443), 5)
    ssl.create_default_context().wrap_socket(s, server_hostname=\"one.one.one.one\"); print(\"TLS OK: LEAK\")
except Exception as e: print(\"failed:\", type(e).__name__, str(e)[:80])"')"
  res egress_udp_443 "$(x pg 'python3 -c "
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(3)
s.sendto(b\"\\xc0\" + b\"\\x00\" * 1199, (\"1.1.1.1\", 443))
try: s.recvfrom(2048); print(\"reply: LEAK\")
except Exception as e: print(\"no reply:\", type(e).__name__)"')"
  res egress_dns_8888 "$(x pg 'python3 -c "
import socket, struct
q = struct.pack(\">HHHHHH\", 0x1234, 0x0100, 1, 0, 0, 0) + b\"\".join(bytes([len(p)]) + p.encode() for p in \"www.google.com\".split(\".\")) + b\"\\x00\\x00\\x01\\x00\\x01\"
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(3); s.sendto(q, (\"8.8.8.8\", 53))
try:
    d, _ = s.recvfrom(512); print(\"rcode\", d[3] & 15, \"answers\", struct.unpack(\">H\", d[6:8])[0])
except Exception as e: print(\"no reply:\", type(e).__name__)"')"
  res egress_other_port "$(x pg 'timeout 4 bash -c "exec 3<>/dev/tcp/1.1.1.1/22" 2>/dev/null && echo CONNECTED || echo blocked')"
  rmall pg

  echo "== 5. what the browser tries to reach on its own: 120 s idle, with the policy and without"
  since=$(date '+%Y-%m-%d %H:%M:%S'); start bg 1536; wait_exec bg; cdp bg wait --timeout 90 >/dev/null; sleep 120
  res background_with_policy "$(denied_hosts "$(sid bg)" "$since")"
  xr bg 'rm -f /etc/chromium/policies/managed/asp.json; rm -rf /home/sandbox/.config/chromium; systemctl restart asp-desktop-browser' >/dev/null
  sleep 3; since=$(date '+%Y-%m-%d %H:%M:%S'); cdp bg wait --timeout 90 >/dev/null; sleep 120
  res background_without_policy "$(denied_hosts "$(sid bg)" "$since")"
  rmall bg

  echo "== 6. DevTools and noVNC from outside the guest, through vsock (the node's side of a port forward)"
  start vs 1536; wait_exec vs; cdp vs wait --timeout 90 >/dev/null
  vsock=$(journalctl -u asp-server --no-pager -o cat | grep 'sandbox running' | grep "$(sid vs)" | grep -o 'vsock_path=[^ ]*' | tail -1 | cut -d= -f2)
  res vsock_path "$vsock"
  systemd-run --quiet --unit spike-bridge-cdp python3 /root/vsock_bridge.py --listen 127.0.0.1:19222 --socket "$vsock" --port 9222
  systemd-run --quiet --unit spike-bridge-vnc python3 /root/vsock_bridge.py --listen 0.0.0.0:16080 --socket "$vsock" --port 6080
  sleep 1
  res cdp_through_vsock "$(python3 /root/cdp.py --endpoint http://127.0.0.1:19222 goto https://example.com/ --screenshot /root/bridge.png --timeout 30 | tail -1)"
  res novnc_through_vsock "$(curl -s -o /dev/null -w '%{http_code} %{size_download}B' http://127.0.0.1:16080/vnc.html)"
  echo "left running: session vs, bridges spike-bridge-cdp (127.0.0.1:19222) and spike-bridge-vnc (0.0.0.0:16080)"

  echo "== 7. stop and resume: the profile is on the disk, the open pages are not"
  start sr 1536; wait_exec sr; cdp sr wait --timeout 90 >/dev/null; cdp sr goto https://example.com/ >/dev/null
  s=$(ms); A session stop --name sr >/dev/null 2>&1; res desktop_stop_ms $(( $(ms) - s ))
  s=$(ms); A session resume --name sr >/dev/null 2>&1; wait_exec sr; cdp sr wait --timeout 90 >/dev/null; res desktop_resume_to_browser_ms $(( $(ms) - s ))
  res after_resume_history "$(x sr 'ls /home/sandbox/.config/chromium/Default/History 2>&1')"
  rmall sr
  ;;
background)
  echo "== what the browser tries to reach on its own: 120 s idle, with the policy and without"
  since=$(date '+%Y-%m-%d %H:%M:%S'); start bg 1536; wait_exec bg; cdp bg wait --timeout 90 >/dev/null; sleep 120
  res background_with_policy "$(denied_hosts "$(sid bg)" "$since")"
  xr bg 'rm -f /etc/chromium/policies/managed/asp.json; rm -rf /home/sandbox/.config/chromium; systemctl restart asp-desktop-browser' >/dev/null
  sleep 3; since=$(date '+%Y-%m-%d %H:%M:%S'); cdp bg wait --timeout 90 >/dev/null; sleep 120
  res background_without_policy "$(denied_hosts "$(sid bg)" "$since")"
  rmall bg
  ;;
background-flags)
  echo "== the same 120 s idle, with the policy and Playwright's switches against background traffic"
  F="--disable-background-networking --disable-component-update --disable-sync --disable-domain-reliability --no-pings --disable-features=OptimizationHints,OptimizationGuideModelDownloading,MediaRouter,Translate,AutofillServerCommunication,CertificateTransparencyComponentUpdater"
  start bf 1536; wait_exec bf; cdp bf wait --timeout 90 >/dev/null
  xr bf "echo 'ASP_DESKTOP_EXTRA_FLAGS=$F' >> /etc/default/asp-desktop; rm -rf /home/sandbox/.config/chromium; systemctl restart asp-desktop-browser" >/dev/null
  sleep 3; since=$(date '+%Y-%m-%d %H:%M:%S'); cdp bf wait --timeout 90 >/dev/null; sleep 120
  res background_with_policy_and_flags "$(denied_hosts "$(sid bf)" "$since")"
  res still_loads "$(cdp bf goto https://example.com/ --timeout 30 | tail -1)"
  rmall bf
  ;;
headless)
  echo "== the same browser without a screen: --headless=new, the display, window manager and noVNC stopped (1536 MiB)"
  for mode in headed headless; do
    n=hl$mode; start $n 1536; wait_exec $n; cdp $n wait --timeout 90 >/dev/null
    if [[ $mode == headless ]]; then
      xr $n 'systemctl stop asp-desktop.target asp-desktop-browser asp-desktop-display asp-desktop-wm asp-desktop-novnc; sync; echo 3 > /proc/sys/vm/drop_caches
        p="${HTTPS_PROXY:-$(tr "\0" "\n" < /proc/1/environ | sed -n "s/^HTTPS_PROXY=//p")}"
        systemd-run --quiet --unit hl-chromium --uid=sandbox --setenv=HOME=/home/sandbox /usr/bin/chromium --headless=new --no-first-run \
          --user-data-dir=/home/sandbox/.config/chromium-headless --remote-debugging-address=127.0.0.1 --remote-debugging-port=9222 \
          --window-size=1280,800 --proxy-server="$p" about:blank' >/dev/null
      cdp $n wait --timeout 60 >/dev/null
    else
      xr $n 'sync; echo 3 > /proc/sys/vm/drop_caches' >/dev/null
    fi
    g=$(cdp $n goto "$WIKI" --timeout 60 | tail -1); sleep 5
    res $mode "page=$g guest_used_mib=$(x $n "free -m | awk '/^Mem:/{print \$3}'") processes=$(x $n "ps -eo rss,comm --sort=-rss | awk 'NR>1 && \$1>10000{s+=\$1; n++} END{printf \"%d over 10 MiB, %d MiB RSS\", n, s/1024}'")"
    rmall $n
  done
  ;;
density)
  N=${2:-8}; M=${3:-1536}
  echo "== density: $N desktop sandboxes of $M MiB at once, each loads the article"
  rmall $(seq -f "n%g" 1 "$N")
  # A deleted sandbox holds its place in the scheduler until the node has torn it down: start from an empty node.
  for _ in $(seq 1 180); do [[ $(systemctl list-units --no-legend --plain 'asp-vm-*' | wc -l) == 0 ]] && break; sleep 1; done
  sleep 5
  used0=$(free -m | awk '/^Mem:/{print $3}')
  s=$(ms)
  for i in $(seq 1 "$N"); do (
    A session start --timeout 900s --name n$i --memory-mib "$M" --workspace "$W" >/tmp/start-n$i.log 2>&1 || { echo "  n$i start failed after $(( $(ms) - s )) ms: $(tail -2 /tmp/start-n$i.log | tr '\n' ' ')"; exit 1; }
    wait_exec n$i 900 || { echo "  n$i: no command after $(( $(ms) - s )) ms"; exit 1; }
    echo "  n$i command at $(( $(ms) - s )) ms"
    cdp n$i wait --timeout 300 >/dev/null && echo "  n$i browser ready at $(( $(ms) - s )) ms"
  ) & done; wait
  res density_all_browsers_ms $(( $(ms) - s ))
  ok=0; for i in $(seq 1 "$N"); do r=$(cdp n$i goto "$WIKI" --timeout 90 | tail -1); [[ $r == *'"title": "Virtual machine'* || $r == *'"title":"Virtual machine'* ]] && ok=$((ok+1)) || echo "  n$i: $r"; done
  res density_pages_loaded "$ok/$N"
  sleep 10
  tot=0; for i in $(seq 1 "$N"); do v=$(vm_mib n$i); [[ $v =~ ^[0-9]+$ ]] && tot=$((tot+v)); done
  res density_vm_mib_total "$tot (per sandbox $((tot / N)))"
  res density_node_used_mib "$(( $(free -m | awk '/^Mem:/{print $3}') - used0 ))"
  res density_disk_per_sandbox "$(du -m /var/lib/asp/disks/rootfs-*.img 2>/dev/null | awk '{s+=$1; n++} END{printf "%d MiB allocated each, %d sandboxes", (n ? s/n : 0), n}')"
  rmall $(seq -f "n%g" 1 "$N")
  for _ in $(seq 1 180); do [[ $(systemctl list-units --no-legend --plain 'asp-vm-*' | wc -l) == 0 ]] && break; sleep 1; done
  ;;
*) echo "usage: $0 plain|switch|desktop|background|background-flags|headless|density [N M]" >&2; exit 2 ;;
esac
