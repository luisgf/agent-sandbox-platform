#!/usr/bin/env bash
# Build the guest image a node boots: rootfs.img (ext4), its compressed copy, SHA256SUMS and
# image.json, the same bytes every time. Needs docker (BuildKit), not root: nothing is
# mounted; the filesystem is laid out inside a container.
#
#   scripts/build-guest-image.sh [output-dir]          # default build/guest
#   scripts/build-guest-image.sh --verify [output-dir] # build twice from scratch, fail if they differ
#
# Environment:
#   SOURCE_DATE_EPOCH       the build date, in seconds (default: the time of the HEAD commit)
#   ASP_ROOTFS_SIZE_MB      size of the filesystem (default 512)
#   ASP_GUEST_VERSION       the version image.json says (default: git describe, or "dev")
#   ASP_GUEST_KERNEL_FILE   a kernel to publish with the image, as vmlinux
#   ASP_GUEST_MODULES       a lib/modules/<kernel release> directory to put in the image (a kernel
#                           built with its drivers as modules, e.g. the host's Ubuntu kernel)
#
# What makes it reproducible is in images/guest/Dockerfile (pinned bases, a package snapshot,
# locked crates and modules) and images/guest/pack-rootfs.sh (the ext4 layout).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# BuildKit (the buildx plugin) can write the filesystem of the image straight to a tar. A docker
# without the plugin has only the classic builder, which keeps the image in the daemon: the
# filesystem is then exported from a container that is created and never started. The packer
# removes what docker adds to the second kind (docker-injected files), so both make the same image.
LOAD=()
if docker buildx version >/dev/null 2>&1; then
  export DOCKER_BUILDKIT=1
  LOAD=(--load)   # a container-driver builder keeps its images unless asked to load them
else
  unset DOCKER_BUILDKIT
fi

verify=0
if [[ "${1:-}" == "--verify" ]]; then verify=1; shift; fi
OUT="${1:-$ROOT/build/guest}"
SIZE_MB="${ASP_ROOTFS_SIZE_MB:-512}"
EPOCH="${SOURCE_DATE_EPOCH:-$(git -C "$ROOT" log -1 --format=%ct 2>/dev/null || true)}"
[[ -n "$EPOCH" ]] || { echo "SOURCE_DATE_EPOCH is not set and this is not a git checkout" >&2; exit 2; }
VERSION="${ASP_GUEST_VERSION:-$(git -C "$ROOT" describe --tags --always 2>/dev/null || echo dev)}"
COMMIT="$(git -C "$ROOT" rev-parse HEAD 2>/dev/null || echo unknown)"
DOCKERFILE="$ROOT/images/guest/Dockerfile"
TAG="asp-guest-build:$EPOCH"
PACKER="asp-guest-packer:$EPOCH"

sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }
size() { wc -c <"$1" | tr -d ' '; }
arg() { sed -n "s/^ARG $1=//p" "$DOCKERFILE" | head -n1; }

# build <output dir> [extra docker build flags]: everything, into the directory.
build() {
  local out=$1; shift
  rm -rf "$out"; mkdir -p "$out"
  local flags=(--build-arg "SOURCE_DATE_EPOCH=$EPOCH" -f "$DOCKERFILE")

  echo "==> the filesystem of the guest (SOURCE_DATE_EPOCH=$EPOCH)"
  if (( ${#LOAD[@]} )); then
    docker build "$@" "${flags[@]}" --output "type=tar,dest=$out/rootfs.tar" "$ROOT"
    docker build "${flags[@]}" -t "$TAG" "${LOAD[@]}" "$ROOT" >/dev/null
  else
    docker build "$@" "${flags[@]}" -t "$TAG" "$ROOT" >/dev/null
    local cid; cid="$(docker create "$TAG")"
    docker export "$cid" >"$out/rootfs.tar"
    docker rm -f "$cid" >/dev/null
  fi

  if [[ -n "${ASP_GUEST_MODULES:-}" ]]; then
    [[ -d "$ASP_GUEST_MODULES" ]] || { echo "ASP_GUEST_MODULES is not a directory: $ASP_GUEST_MODULES" >&2; exit 1; }
    # In usr/lib/modules, not lib/modules: /lib is a symlink to usr/lib in the image, and a
    # tar member "lib" of another kind would replace it and take /sbin/init with it.
    local stage; stage="$(mktemp -d)"
    mkdir -p "$stage/usr/lib/modules"
    cp -a "$ASP_GUEST_MODULES" "$stage/usr/lib/modules/$(basename "$ASP_GUEST_MODULES")"
    tar -rf "$out/rootfs.tar" --numeric-owner --owner=0 --group=0 -C "$stage" "usr/lib/modules/$(basename "$ASP_GUEST_MODULES")"
    rm -rf "$stage"
  fi

  echo "==> the ext4 image (${SIZE_MB} MiB)"
  docker build "$@" "${flags[@]}" --target packer -t "$PACKER" "${LOAD[@]}" "$ROOT" >/dev/null
  docker run --rm --tmpfs /work:exec,mode=0755,size=3g -v "$out":/out "$PACKER" \
    /out/rootfs.tar /out/rootfs.img "$SIZE_MB" "$EPOCH"
  rm -f "$out/rootfs.tar"

  if [[ -n "${ASP_GUEST_KERNEL_FILE:-}" ]]; then
    cp "$ASP_GUEST_KERNEL_FILE" "$out/vmlinux"
  fi

  echo "==> image.json and SHA256SUMS"
  local pkgs
  pkgs="$(docker run --rm --user 0 --entrypoint dpkg-query "$TAG" -W -f '${Package} ${Version}\n' | LC_ALL=C sort \
    | awk 'BEGIN{first=1} {printf "%s    {\"name\": \"%s\", \"version\": \"%s\"}", (first?"":",\n"), $1, $2; first=0} END{print ""}')"
  {
    echo '{'
    echo '  "schema": 1,'
    echo "  \"version\": \"$VERSION\","
    echo "  \"commit\": \"$COMMIT\","
    echo "  \"source_date_epoch\": $EPOCH,"
    echo "  \"base\": {\"image\": \"$(arg DEBIAN_IMAGE)\", \"snapshot\": \"$(arg SNAPSHOT)\"},"
    echo "  \"rootfs\": {\"file\": \"rootfs.img\", \"sha256\": \"$(sha256 "$out/rootfs.img")\", \"size\": $(size "$out/rootfs.img"),"
    echo "    \"gzip\": {\"file\": \"rootfs.img.gz\", \"sha256\": \"$(sha256 "$out/rootfs.img.gz")\", \"size\": $(size "$out/rootfs.img.gz")}},"
    if [[ -f "$out/vmlinux" ]]; then
      echo "  \"kernel\": {\"file\": \"vmlinux\", \"sha256\": \"$(sha256 "$out/vmlinux")\", \"size\": $(size "$out/vmlinux")},"
    fi
    echo '  "packages": ['
    echo "$pkgs"
    echo '  ]'
    echo '}'
  } >"$out/image.json"
  (
    cd "$out"
    files=(rootfs.img rootfs.img.gz image.json)
    [[ -f vmlinux ]] && files+=(vmlinux)
    for f in "${files[@]}"; do echo "$(sha256 "$f")  $f"; done
  ) >"$out/SHA256SUMS"
}

if (( verify )); then
  A="$OUT.a"; B="$OUT.b"
  build "$A"
  build "$B" --no-cache
  if cmp -s "$A/SHA256SUMS" "$B/SHA256SUMS"; then
    echo "OK reproducible: two builds from scratch have the same SHA256SUMS"
    cat "$A/SHA256SUMS"
    rm -rf "$OUT"; mv "$A" "$OUT"; rm -rf "$B"
    exit 0
  fi
  echo "FAIL not reproducible: the builds differ" >&2
  diff "$A/SHA256SUMS" "$B/SHA256SUMS" >&2 || true
  echo "kept $A and $B to compare" >&2
  exit 1
fi

build "$OUT"
echo "OK: $OUT"
cat "$OUT/SHA256SUMS"
