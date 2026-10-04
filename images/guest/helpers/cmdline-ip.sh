#!/bin/sh
# Apply the kernel ip= cmdline. The lab kernel is built without CONFIG_IP_PNP,
# so the kernel ignores ip= and eth0 stays down. This runs before pod-daemon.
#
# ip=<client>:<server>:<gw>:<netmask>:<hostname>:<device>:<autoconf>
# No ip= token (or a non-static form): exit 0. Boot must continue.
#
# Tests:
#   ASP_PROC_CMDLINE  file to read instead of /proc/cmdline
#   ASP_IP_BIN        ip(8) binary (default ip)
set -u

cmdline_file="${ASP_PROC_CMDLINE:-/proc/cmdline}"
ip_bin="${ASP_IP_BIN:-ip}"

if [ ! -r "$cmdline_file" ]; then
  echo "cmdline-ip: cannot read $cmdline_file; skipping" >&2
  exit 0
fi

cmdline=$(cat "$cmdline_file")
iparg=""
for tok in $cmdline; do
  case "$tok" in
    ip=*)
      iparg=${tok#ip=}
      break
      ;;
  esac
done

if [ -z "$iparg" ]; then
  exit 0
fi

case "$iparg" in
  *:*) ;;
  *) exit 0 ;;
esac

oldifs=$IFS
IFS=:
# shellcheck disable=SC2086
set -- $iparg
IFS=$oldifs

client=${1:-}
gw=${3:-}
mask=${4:-}
dev=${6:-}

if [ -z "$client" ] || [ -z "$dev" ] || [ -z "$mask" ]; then
  echo "cmdline-ip: incomplete ip=$iparg; skipping" >&2
  exit 0
fi

prefix_from_mask() {
  m=$1
  oifs=$IFS
  IFS=.
  # shellcheck disable=SC2086
  set -- $m
  IFS=$oifs
  if [ $# -ne 4 ]; then
    return 1
  fi
  bits=0
  expect_zero=0
  for oct in "$1" "$2" "$3" "$4"; do
    case "$oct" in
      ""|*[!0-9]*) return 1 ;;
    esac
    if [ "$oct" -gt 255 ]; then
      return 1
    fi
    n=$oct
    bit=128
    while [ "$bit" -gt 0 ]; do
      if [ $((n / bit)) -ge 1 ]; then
        if [ "$expect_zero" -eq 1 ]; then
          return 1
        fi
        bits=$((bits + 1))
        n=$((n - bit))
      else
        expect_zero=1
      fi
      bit=$((bit / 2))
    done
  done
  printf "%s\n" "$bits"
}

prefix=$(prefix_from_mask "$mask") || {
  echo "cmdline-ip: bad netmask $mask" >&2
  exit 1
}

if ! "$ip_bin" link set dev "$dev" up; then
  echo "cmdline-ip: link up $dev failed" >&2
  exit 1
fi
if ! "$ip_bin" addr replace "$client/$prefix" dev "$dev"; then
  echo "cmdline-ip: address $client/$prefix dev $dev failed" >&2
  exit 1
fi
if [ -n "$gw" ]; then
  if ! "$ip_bin" route replace default via "$gw" dev "$dev"; then
    echo "cmdline-ip: default via $gw dev $dev failed" >&2
    exit 1
  fi
fi
exit 0
