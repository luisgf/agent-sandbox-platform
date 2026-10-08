#!/usr/bin/env bash
# Build the kernel the guest boots: vmlinux and the configuration it was built with, the same
# bytes every time. Needs docker, not root. The recipe (source, compiler, configuration) is
# images/guest/kernel/Dockerfile.
#
#   scripts/build-guest-kernel.sh [output-dir]          # default build/kernel: vmlinux, kernel.config, kernel.env
#   scripts/build-guest-kernel.sh --verify [output-dir] # build twice from scratch, fail if they differ
#
# build-guest-image.sh --kernel runs this and publishes the kernel with the image.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DOCKERFILE="$ROOT/images/guest/kernel/Dockerfile"

verify=0
if [[ "${1:-}" == "--verify" ]]; then verify=1; shift; fi
OUT="${1:-$ROOT/build/kernel}"

sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }
arg() { sed -n "s/^ARG $1=//p" "$DOCKERFILE" | head -n1; }

# BuildKit can write the files of the last stage to a directory; the classic builder keeps an
# image in the daemon and the files are copied out of a container that is created and never started.
buildkit=0
if docker buildx version >/dev/null 2>&1; then
  export DOCKER_BUILDKIT=1
  buildkit=1
else
  unset DOCKER_BUILDKIT
fi

# build <output dir> [extra docker build flags]
build() {
  local out=$1; shift
  rm -rf "$out"; mkdir -p "$out"
  echo "==> the kernel $(arg KERNEL_VERSION) (a few minutes; Cloud Hypervisor's configuration as the base)"
  if (( buildkit )); then
    docker build "$@" -f "$DOCKERFILE" --target out --output "type=local,dest=$out" "$ROOT"
  else
    local tag="asp-guest-kernel:$(arg BUILD_EPOCH)"
    docker build "$@" -f "$DOCKERFILE" --target out -t "$tag" "$ROOT"
    local cid; cid="$(docker create "$tag" /vmlinux)"
    docker cp "$cid:/vmlinux" "$out/vmlinux"
    docker cp "$cid:/kernel.config" "$out/kernel.config"
    docker rm -f "$cid" >/dev/null
  fi
  [[ -s "$out/vmlinux" && -s "$out/kernel.config" ]] || { echo "the build left no vmlinux or kernel.config in $out" >&2; exit 1; }
  # What image.json says about the kernel.
  {
    echo "KERNEL_VERSION=$(arg KERNEL_VERSION)"
    echo "KERNEL_SHA256=$(arg KERNEL_SHA256)"
    echo "CONFIG_BASE_COMMIT=$(arg CH_DEFCONFIG_COMMIT)"
    echo "CONFIG_SHA256=$(sha256 "$out/kernel.config")"
    echo "VMLINUX_SHA256=$(sha256 "$out/vmlinux")"
  } >"$out/kernel.env"
}

if (( verify )); then
  A="$OUT.a"; B="$OUT.b"
  build "$A"
  build "$B" --no-cache
  if cmp -s "$A/kernel.env" "$B/kernel.env"; then
    echo "OK reproducible: two builds from scratch give the same vmlinux and configuration"
    cat "$A/kernel.env"
    rm -rf "$OUT"; mv "$A" "$OUT"; rm -rf "$B"
    exit 0
  fi
  echo "FAIL not reproducible: the builds differ" >&2
  diff "$A/kernel.env" "$B/kernel.env" >&2 || true
  echo "kept $A and $B to compare" >&2
  exit 1
fi

build "$OUT"
echo "OK: $OUT"
cat "$OUT/kernel.env"
