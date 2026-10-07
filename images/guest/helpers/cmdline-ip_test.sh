#!/bin/sh
# Tests for cmdline-ip.sh: a fake ip(8) and a scratch /etc.   sh cmdline-ip_test.sh
set -u
here=$(cd "$(dirname "$0")" && pwd)
helper="$here/cmdline-ip.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
failures=0

cat >"$tmp/fakeip" <<'FAKE'
#!/bin/sh
echo "ip $*" >>"$ASP_TEST_IPLOG"
FAKE
chmod +x "$tmp/fakeip"

# run <name> <cmdline>: runs the helper on a fresh scratch tree; sets $etc, $log, $rc.
run() {
  case_dir="$tmp/$1"
  mkdir -p "$case_dir/etc"
  etc="$case_dir/etc"
  log="$case_dir/ip.log"
  : >"$log"
  printf '%s\n' "$2" >"$case_dir/cmdline"
  ASP_TEST_IPLOG="$log" ASP_IP_BIN="$tmp/fakeip" ASP_PROC_CMDLINE="$case_dir/cmdline" \
    ASP_ETC="$etc" ASP_PROC_HOSTNAME="$case_dir/kernel-hostname" sh "$helper" >"$case_dir/out" 2>"$case_dir/err"
  rc=$?
}

check() { # <what> <got> <want>
  if [ "$2" != "$3" ]; then
    echo "FAIL: $1"
    echo "  got:  $2"
    echo "  want: $3"
    failures=$((failures + 1))
  fi
}

# --- the node's full form: address, gateway, name and resolver
run full "console=ttyS0 root=/dev/vda reboot=k panic=1 ip=10.233.0.2::10.233.0.1:255.255.255.252:asp-4252d313:eth0:off:10.233.0.1 systemd.setenv=HTTP_PROXY=http://10.233.0.1:8888"
check "full: exit" "$rc" "0"
check "full: ip calls" "$(cat "$log")" "ip link set dev eth0 up
ip addr replace 10.233.0.2/30 dev eth0
ip route replace default via 10.233.0.1 dev eth0"
check "full: kernel hostname" "$(cat "$case_dir/kernel-hostname")" "asp-4252d313"
check "full: /etc/hostname" "$(cat "$etc/hostname")" "asp-4252d313"
check "full: /etc/hosts" "$(cat "$etc/hosts")" "127.0.0.1 localhost
127.0.1.1 asp-4252d313
::1 localhost ip6-localhost ip6-loopback"
check "full: resolv.conf" "$(cat "$etc/resolv.conf")" "nameserver 10.233.0.1
options timeout:2 attempts:2"

# --- two resolvers
run two "ip=10.0.0.2::10.0.0.1:255.255.255.252:g:eth0:off:10.0.0.1:1.1.1.1"
check "two resolvers" "$(cat "$etc/resolv.conf")" "nameserver 10.0.0.1
nameserver 1.1.1.1
options timeout:2 attempts:2"

# --- the form older nodes send: no name, no resolver; nothing else is touched
run old "ip=10.0.0.2::10.0.0.1:255.255.255.252::eth0:off"
check "old: exit" "$rc" "0"
check "old: ip calls" "$(wc -l <"$log" | tr -d ' ')" "3"
for f in hostname hosts resolv.conf; do
  [ -e "$etc/$f" ] && { echo "FAIL: old form wrote $f"; failures=$((failures + 1)); }
done
[ -e "$case_dir/kernel-hostname" ] && { echo "FAIL: old form set the kernel hostname"; failures=$((failures + 1)); }

# --- a name or a resolver that is not one is ignored, the address still applies
run bad "ip=10.0.0.2::10.0.0.1:255.255.255.252:bad_name!:eth0:off:not-an-ip"
check "bad: exit" "$rc" "0"
check "bad: ip calls" "$(wc -l <"$log" | tr -d ' ')" "3"
for f in hostname hosts resolv.conf; do
  [ -e "$etc/$f" ] && { echo "FAIL: bad values wrote $f"; failures=$((failures + 1)); }
done
for bad in "-dash" "a.b" "a b"; do
  run badname "ip=10.0.0.2::10.0.0.1:255.255.255.252:$bad:eth0:off"
  [ -e "$etc/hostname" ] && { echo "FAIL: hostname '$bad' was accepted"; failures=$((failures + 1)); }
done
for bad in "999.1.1.1" "1.2.3" "1.2.3.4.5" "1..2.3" ".1.2.3"; do
  run badip "ip=10.0.0.2::10.0.0.1:255.255.255.252::eth0:off:$bad"
  [ -e "$etc/resolv.conf" ] && { echo "FAIL: resolver '$bad' was accepted"; failures=$((failures + 1)); }
done

# --- no ip= at all, or one that is not static: nothing happens, boot goes on
run none "console=ttyS0 root=/dev/vda"
check "none: exit" "$rc" "0"
check "none: ip calls" "$(wc -c <"$log" | tr -d ' ')" "0"
run dhcp "ip=dhcp"
check "dhcp: exit" "$rc" "0"
check "dhcp: ip calls" "$(wc -c <"$log" | tr -d ' ')" "0"

if [ "$failures" -ne 0 ]; then
  echo "$failures failure(s)"
  exit 1
fi
echo "ok cmdline-ip"
