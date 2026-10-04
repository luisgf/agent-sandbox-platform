# syntax=docker/dockerfile:1.7
# Build from repo root:
#   docker build -t agent-sandbox-guest -f images/guest/Dockerfile .
FROM rust:1-bookworm AS build-rust
WORKDIR /src/pod-daemon
COPY pod-daemon/Cargo.toml pod-daemon/Cargo.lock* ./
COPY pod-daemon/src ./src
RUN if [ -f Cargo.lock ]; then cargo build --release --locked; else cargo build --release; fi

FROM golang:1.27-bookworm AS build-go
WORKDIR /src/vsock-ssh-agent-proxy
COPY images/guest/cmd/vsock-ssh-agent-proxy/go.mod ./
COPY images/guest/cmd/vsock-ssh-agent-proxy/*.go ./
RUN go mod tidy && CGO_ENABLED=0 go build -o /out/vsock-ssh-agent-proxy .

FROM debian:bookworm-slim
# CH boots the kernel with root=/dev/vda and /sbin/init. docker export does not
# keep ENTRYPOINT/CMD, and bookworm-slim ships neither systemd nor /sbin/init.
# systemd-sysv is the package that links /sbin/init -> /lib/systemd/systemd.
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
         ca-certificates iproute2 kmod systemd systemd-sysv \
    && rm -rf /var/lib/apt/lists/* \
    && install -d -o root -g root -m 0755 /var/lib/sandboxd \
    && useradd --system --uid 10001 --home-dir /var/lib/sandboxd --shell /usr/sbin/nologin sandboxd \
    && chown sandboxd:sandboxd /var/lib/sandboxd \
    && install -d -o sandboxd -g sandboxd -m 0750 /run/agent-sandbox \
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
# Default productive listen: AF_VSOCK port 26500 (host dials via CH hybrid CONNECT).
# SSH agent: ssh-agent-vsock.service (vsock CID2:26501 → /run/agent-sandbox/ssh-agent.sock).
# Workspace: workspace-virtiofs.service mounts tag workspace on /workspace (exit 0 if absent).
ENV ASP_HOST_CID=2
ENV SSH_AUTH_SOCK=/run/agent-sandbox/ssh-agent.sock
USER sandboxd
ENTRYPOINT ["/usr/local/bin/pod-daemon"]
CMD ["--listen", "vsock", "--vsock-port", "26500"]
