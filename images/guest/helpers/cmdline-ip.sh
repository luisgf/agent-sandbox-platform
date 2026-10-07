#!/bin/sh
# Apply the kernel ip= cmdline. The lab kernel is built without CONFIG_IP_PNP,
# so the kernel ignores ip= and eth0 stays down. This runs before pod-daemon.
#
# ip=<client>:<server>:<gw>:<netmask>:<hostname>:<device>:<autoconf>:<dns0>:<dns1>
# Besides the address and the default route it sets what the node put in the
# optional fields: the hostname (/etc/hostname, /etc/hosts and the kernel's) and the
# resolver(s) (/etc/resolv.conf). A field left empty is left alone.
# No ip= token (or a non-static form): exit 0. Boot must continue.
#
# Tests:
#   ASP_PROC_CMDLINE  file to read instead of /proc/cmdline
#   ASP_IP_BIN        ip(8) binary (default ip)
#   ASP_ETC           directory to write hostname, hosts and resolv.conf in (default /etc)
#   ASP_PROC_HOSTNAME file that sets the kernel's hostname (default /proc/sys/kernel/hostname)
set -u

cmdline_file="${ASP_PROC_CMDLINE:-/proc/cmdline}"
ip_bin="${ASP_IP_BIN:-ip}"
etc_dir="${ASP_ETC:-/etc}"
proc_hostname="${ASP_PROC_HOSTNAME:-/proc/sys/kernel/hostname}"

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
name=${5:-}
dev=${6:-}
dns0=${8:-}
dns1=${9:-}

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

# From here on a failure is reported but does not fail the boot: the guest has its
# address, and pod-daemon answers on vsock whatever it is called.

# A hostname is letters, digits and dashes, not starting with a dash.
if [ -n "$name" ]; then
  case "$name" in
    -*|*[!A-Za-z0-9-]*)
      echo "cmdline-ip: ignoring hostname '$name'" >&2
      ;;
    *)
      printf '%s\n' "$name" >"$proc_hostname" 2>/dev/null || echo "cmdline-ip: cannot set the kernel hostname" >&2
      printf '%s\n' "$name" >"$etc_dir/hostname" 2>/dev/null || echo "cmdline-ip: cannot write $etc_dir/hostname" >&2
      # Without its own name in /etc/hosts, sudo and friends stall on a lookup.
      printf '127.0.0.1 localhost\n127.0.1.1 %s\n::1 localhost ip6-localhost ip6-loopback\n' "$name" >"$etc_dir/hosts" 2>/dev/null ||
        echo "cmdline-ip: cannot write $etc_dir/hosts" >&2
      ;;
  esac
fi

# The resolver is the node's DNS sink (or whatever it named): an IPv4 address.
valid_ipv4() {
  case "$1" in
    ""|*[!0-9.]*|.*|*.|*..*) return 1 ;;
  esac
  oifs=$IFS
  IFS=.
  # shellcheck disable=SC2086
  set -- $1
  IFS=$oifs
  [ $# -eq 4 ] || return 1
  for oct in "$1" "$2" "$3" "$4"; do
    [ "$oct" -le 255 ] 2>/dev/null || return 1
  done
}

if [ -n "$dns0" ]; then
  if valid_ipv4 "$dns0"; then
    {
      printf 'nameserver %s\n' "$dns0"
      if [ -n "$dns1" ] && valid_ipv4 "$dns1"; then
        printf 'nameserver %s\n' "$dns1"
      fi
      # A resolver that does not answer must not stall every lookup for the glibc default.
      printf 'options timeout:2 attempts:2\n'
    } >"$etc_dir/resolv.conf" 2>/dev/null || echo "cmdline-ip: cannot write $etc_dir/resolv.conf" >&2
  else
    echo "cmdline-ip: ignoring resolver '$dns0'" >&2
  fi
fi
exit 0
