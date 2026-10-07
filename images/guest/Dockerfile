# syntax=docker/dockerfile:1.7
# The guest image: what every sandbox boots. It is built to be the same bytes every time
# (scripts/build-guest-image.sh builds it twice in CI and compares): every base image is
# pinned by digest, every package comes from one dated snapshot of the Debian archive,
# every crate and Go module is checked against its lock file, and nothing records when
# or where it was built. To move to newer packages, change SNAPSHOT (and the digests
# with it), build, and look at the diff of the package list in image.json.
#
# Build from the repo root:
#   docker build -t agent-sandbox-guest -f images/guest/Dockerfile .
# The rootfs.img the nodes boot is made by scripts/build-guest-image.sh.

# The Debian image and the snapshot of the archive it was made from: the official
# image says which one in /etc/apt/sources.list.d/debian.sources. A newer image gets a
# newer date here.
ARG DEBIAN_IMAGE=debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587
ARG SNAPSHOT=20261005T000000Z
# The image of the packer stage, below: trixie, made from the same snapshot.
ARG PACKER_IMAGE=debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f
# The build date: the time of the commit, set by the build script. It becomes the
# timestamp of every file and the "last password change" of the users.
ARG SOURCE_DATE_EPOCH=0

FROM rust:1-bookworm@sha256:114c7a4425406451c2866b6aafe69fe29b1b298832db1277d411ac73c82d04d6 AS build-rust
WORKDIR /src/pod-daemon
COPY pod-daemon/Cargo.toml pod-daemon/Cargo.lock ./
COPY pod-daemon/src ./src
# --locked: the versions and checksums of Cargo.lock, nothing else. The paths that end up
# in the binary (panic messages, debug info) are not those of this machine.
ENV CARGO_INCREMENTAL=0 \
    RUSTFLAGS="--remap-path-prefix=/src/pod-daemon=/build/pod-daemon --remap-path-prefix=/usr/local/cargo=/cargo --remap-path-prefix=/usr/local/rustup=/rustup"
RUN cargo build --release --locked

FROM golang:1.27-bookworm@sha256:8d48e12ec56735e9358640898b9d9b9fcca110612ed8a5567438c0a1baa24e66 AS build-go
WORKDIR /src/vsock-ssh-agent-proxy
COPY images/guest/cmd/vsock-ssh-agent-proxy/go.mod images/guest/cmd/vsock-ssh-agent-proxy/go.sum ./
COPY images/guest/cmd/vsock-ssh-agent-proxy/*.go ./
# -mod=readonly: go.sum decides, and a build that would change go.mod or go.sum fails
# (it used to run go mod tidy here, which fetched whatever was newest).
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -buildvcs=false -ldflags="-s -w -buildid=" -o /out/vsock-ssh-agent-proxy .

# The tool that lays out the ext4 filesystem. It is Debian trixie's, from the same day's snapshot:
# e2fsprogs 1.47.1 and later give every file the build date when SOURCE_DATE_EPOCH is set
# (bookworm's 1.47.0 copies the time each file was extracted, and no two images are the same).
# It only makes the filesystem; the guest keeps bookworm.
FROM ${PACKER_IMAGE} AS packer
ARG SNAPSHOT
RUN rm -f /etc/apt/sources.list.d/debian.sources \
    && printf '%s\n' \
         "deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/${SNAPSHOT}/ trixie main" \
         > /etc/apt/sources.list \
    && apt-get update -qq \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends e2fsprogs gzip tar \
    && rm -rf /var/lib/apt/lists/*
COPY images/guest/pack-rootfs.sh /usr/local/bin/pack-rootfs
ENTRYPOINT ["/usr/local/bin/pack-rootfs"]

FROM ${DEBIAN_IMAGE}
ARG SNAPSHOT
ARG SOURCE_DATE_EPOCH
# CH boots the kernel with root=/dev/vda and /sbin/init. docker export does not
# keep ENTRYPOINT/CMD, and bookworm-slim ships neither systemd nor /sbin/init.
# systemd-sysv is the package that links /sbin/init -> /lib/systemd/systemd.
# The packages come from the snapshot (the archive as it was that day), not from
# whatever the mirror has today.
RUN rm -f /etc/apt/sources.list.d/debian.sources \
    && printf '%s\n' \
         "deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/${SNAPSHOT}/ bookworm bookworm-updates main" \
         "deb [check-valid-until=no] http://snapshot.debian.org/archive/debian-security/${SNAPSHOT}/ bookworm-security main" \
         > /etc/apt/sources.list \
    && apt-get update -qq \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
         ca-certificates chrony iproute2 kmod systemd systemd-sysv \
    && rm -rf /var/lib/apt/lists/* \
    && install -d -o root -g root -m 0755 /var/lib/sandboxd \
    && useradd --system --uid 10001 --home-dir /var/lib/sandboxd --shell /usr/sbin/nologin sandboxd \
    && chown sandboxd:sandboxd /var/lib/sandboxd \
    # uid 1000 is the usual owner of a workspace: give it a name and a home, so its \
    # files and its shell do not show a bare number ("I have no name!"). \
    && useradd --uid 1000 --create-home --shell /bin/bash sandbox \
    && install -d -o sandboxd -g sandboxd -m 0755 /run/agent-sandbox \
    && install -d -m 0755 /etc/systemd/system \
    && test -x /lib/systemd/systemd \
    && test -e /sbin/init
COPY --from=build-rust /src/pod-daemon/target/release/pod-daemon /usr/local/bin/pod-daemon
COPY --from=build-go /out/vsock-ssh-agent-proxy /usr/local/bin/vsock-ssh-agent-proxy
COPY images/guest/systemd/pod-daemon.service /etc/systemd/system/pod-daemon.service
COPY images/guest/systemd/ssh-agent-vsock.service /etc/systemd/system/ssh-agent-vsock.service
COPY images/guest/systemd/workspace-virtiofs.service /etc/systemd/system/workspace-virtiofs.service
COPY images/guest/systemd/cmdline-ip.service /etc/systemd/system/cmdline-ip.service
COPY images/guest/modules-load.d/vsock.conf /etc/modules-load.d/vsock.conf
COPY images/guest/modules-load.d/ptp_kvm.conf /etc/modules-load.d/ptp_kvm.conf
COPY images/guest/chrony/chrony.conf /etc/chrony/chrony.conf
COPY images/guest/openrc/pod-daemon /etc/init.d/pod-daemon.openrc-example
COPY images/guest/openrc/ssh-agent-vsock /etc/init.d/ssh-agent-vsock.openrc-example
COPY images/guest/openrc/workspace-virtiofs /etc/init.d/workspace-virtiofs.openrc-example
COPY images/guest/helpers/ssh-agent-vsock-socat.sh /usr/local/share/asp/ssh-agent-vsock-socat.sh
COPY images/guest/helpers/mount-virtiofs-workspace.sh /usr/local/share/asp/mount-virtiofs-workspace.sh
COPY images/guest/helpers/cmdline-ip.sh /usr/local/share/asp/cmdline-ip.sh
RUN chmod 0755 /usr/local/share/asp/mount-virtiofs-workspace.sh /usr/local/share/asp/ssh-agent-vsock-socat.sh /usr/local/share/asp/cmdline-ip.sh \
    && mkdir -p /etc/systemd/system/multi-user.target.wants \
    && ln -sfn /etc/systemd/system/pod-daemon.service /etc/systemd/system/multi-user.target.wants/pod-daemon.service \
    && ln -sfn /etc/systemd/system/ssh-agent-vsock.service /etc/systemd/system/multi-user.target.wants/ssh-agent-vsock.service \
    && ln -sfn /etc/systemd/system/workspace-virtiofs.service /etc/systemd/system/multi-user.target.wants/workspace-virtiofs.service \
    && ln -sfn /etc/systemd/system/cmdline-ip.service /etc/systemd/system/multi-user.target.wants/cmdline-ip.service
# Nothing in the image says when it was built: the logs and caches of the build go, the
# backups of the account files (which have dates of their own) go, and the day each
# account's password last changed is the build date.
RUN rm -rf /var/log/* /var/cache/ldconfig/aux-cache /var/cache/debconf/*-old /var/lib/dpkg/*-old \
         /var/cache/apt/* /tmp/* /etc/passwd- /etc/group- /etc/shadow- /etc/gshadow- /etc/subuid- /etc/subgid- \
    && sed -i -E "s/^([^:]*:[^:]*:)[0-9]*:/\1$((SOURCE_DATE_EPOCH / 86400)):/" /etc/shadow \
    # systemd's postinst draws a random /etc/machine-id. Every sandbox is a copy of this image, so they \
    # shared whichever one the build drew; now they share this one, and two builds are the same. \
    && printf '%s\n' b0c4a1e6d4f84c7a9a5b3f2e1d0c9b8a > /etc/machine-id \
    && chmod 0444 /etc/machine-id
# Network identity comes from the kernel command line the node writes: cmdline-ip.service
# applies ip= (address, gateway, hostname, resolver) and systemd.setenv= gives every
# service, pod-daemon and the commands it runs HTTP_PROXY and friends. Time: chrony
# follows the host through /dev/ptp0 (ptp_kvm).
# Default productive listen: AF_VSOCK port 26500 (host dials via CH hybrid CONNECT).
# SSH agent: ssh-agent-vsock.service (vsock CID2:26501 → /run/agent-sandbox/ssh-agent.sock).
# Workspace: workspace-virtiofs.service mounts tag workspace on /workspace (exit 0 if absent).
ENV ASP_HOST_CID=2
ENV SSH_AUTH_SOCK=/run/agent-sandbox/ssh-agent.sock
USER sandboxd
ENTRYPOINT ["/usr/local/bin/pod-daemon"]
CMD ["--listen", "vsock", "--vsock-port", "26500"]
