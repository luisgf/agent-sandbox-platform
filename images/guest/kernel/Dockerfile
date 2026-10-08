# syntax=docker/dockerfile:1.7
# The guest kernel: what every sandbox boots. Like the guest image it is built to be the same
# bytes every time (scripts/build-guest-kernel.sh --verify builds it twice and compares): the
# compiler comes from the same dated snapshot of the Debian archive as the image's packages, the
# source is a kernel.org tarball checked against a SHA-256, the configuration is Cloud
# Hypervisor's own for guests (checked against a SHA-256 too) plus the few lines of asp.fragment,
# and nothing records when or where it was built.
#
# Everything a sandbox needs is built in, not a module: virtio (block, net, console, vsock,
# virtio-fs), the KVM PTP clock the guest's chrony follows, cgroup v2, ext4 and devtmpfs. The image
# then needs no /lib/modules, and a kernel that loses one of those fails the build
# (required-options), not a start on a node.
#
#   scripts/build-guest-kernel.sh [--verify] [output-dir]
#
# To move to another kernel: change KERNEL_VERSION and KERNEL_SHA256 (the line of
# https://cdn.kernel.org/pub/linux/kernel/v6.x/sha256sums.asc), BUILD_EPOCH (the date of the
# release) and, if Cloud Hypervisor's defconfig moved, CH_DEFCONFIG_COMMIT and its SHA-256; build; and
# read the diff of kernel.config that the build prints.

# The same Debian image and snapshot as the guest image (images/guest/Dockerfile): bookworm's gcc 12.
ARG DEBIAN_IMAGE=debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587
ARG SNAPSHOT=20261005T000000Z
# The longterm kernel (6.18 is the newest longterm series) and the date it was released: it is the
# timestamp inside the kernel, so that the kernel changes with the recipe and not with every commit.
ARG KERNEL_VERSION=6.18.55
ARG KERNEL_SHA256=f410638061a165c12f42ab871d2f3fcd525515359b5faeee80969cff84524df9
ARG BUILD_EPOCH=1790985600
# Cloud Hypervisor's guest configuration (arch/x86/configs/ch_defconfig of its kernel fork) at the commit
# of the release its integration tests boot (ch-release-v6.16.9-20260508). `make olddefconfig` carries it
# to the longterm kernel above.
ARG CH_DEFCONFIG_COMMIT=cb93a83a34da907359c3cfa3050ff7733a0397b2
ARG CH_DEFCONFIG_SHA256=860e1d00a40337aad6e053a0294c5bd6a56a0692eb50413fa06f341f22da89f5

FROM ${DEBIAN_IMAGE} AS build
ARG SNAPSHOT
ARG KERNEL_VERSION
ARG KERNEL_SHA256
ARG BUILD_EPOCH
ARG CH_DEFCONFIG_COMMIT
ARG CH_DEFCONFIG_SHA256
RUN rm -f /etc/apt/sources.list.d/debian.sources \
    && printf '%s\n' \
         "deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/${SNAPSHOT}/ bookworm bookworm-updates main" \
         "deb [check-valid-until=no] http://snapshot.debian.org/archive/debian-security/${SNAPSHOT}/ bookworm-security main" \
         > /etc/apt/sources.list \
    && apt-get update -qq \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
         build-essential bc bison flex libssl-dev libelf-dev xz-utils curl ca-certificates perl python3 \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN curl -fsSL --retry 3 -o linux.tar.xz "https://cdn.kernel.org/pub/linux/kernel/v${KERNEL_VERSION%%.*}.x/linux-${KERNEL_VERSION}.tar.xz" \
    && echo "${KERNEL_SHA256}  linux.tar.xz" | sha256sum -c - \
    && tar -xf linux.tar.xz && mv "linux-${KERNEL_VERSION}" linux && rm linux.tar.xz
RUN curl -fsSL --retry 3 -o ch_defconfig "https://raw.githubusercontent.com/cloud-hypervisor/linux/${CH_DEFCONFIG_COMMIT}/arch/x86/configs/ch_defconfig" \
    && echo "${CH_DEFCONFIG_SHA256}  ch_defconfig" | sha256sum -c -
COPY images/guest/kernel/asp.fragment images/guest/kernel/required-options /src/
WORKDIR /src/linux
RUN cp /src/ch_defconfig .config \
    && mkdir /tmp/merged \
    && ./scripts/kconfig/merge_config.sh -m -O /tmp/merged .config /src/asp.fragment >/dev/null \
    && cp /tmp/merged/.config .config \
    && make olddefconfig \
    && cp .config /src/kernel.config \
    && while read -r opt; do \
         case "$opt" in ''|'#'*) continue ;; esac; \
         grep -qx "$opt" .config || { echo "the kernel configuration lacks $opt (images/guest/kernel/required-options)" >&2; exit 1; }; \
       done < /src/required-options
# KBUILD_BUILD_*: the banner "Linux version 6.18.55-asp (asp@asp) (gcc …) #1 SMP … <date>" is all the kernel
# says about where and when it was made.
RUN export KBUILD_BUILD_TIMESTAMP="$(date -u -d "@${BUILD_EPOCH}")" KBUILD_BUILD_USER=asp KBUILD_BUILD_HOST=asp KBUILD_BUILD_VERSION=1 \
    && make -j"$(nproc)" vmlinux 2>&1 | tail -n 5 \
    && test -s vmlinux

FROM scratch AS out
COPY --from=build /src/linux/vmlinux /vmlinux
COPY --from=build /src/kernel.config /kernel.config
