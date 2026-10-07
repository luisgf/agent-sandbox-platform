# syntax=docker/dockerfile:1.7
# The control plane as a container image, built from source. From the repository root:
#   docker build -f control-plane/Dockerfile -t asp-control-plane .
# Releases build the image from the binaries goreleaser already made
# (packaging/docker/control-plane.Dockerfile): the same runtime stage.
#
# Run it with Postgres and a first key (state lives in the volume):
#   docker run -d --name asp-cp -p 8080:8080 -v asp-cp-state:/var/lib/asp \
#     -e ASP_DATABASE_URL=postgres://asp:...@db:5432/asp -e ASP_BOOTSTRAP_API_KEY=... asp-control-plane
# Authentication is always on, so listening on every address of the container is safe
# enough for what it is; put TLS in front (or ASP_TLS_CERT/ASP_TLS_KEY) before nodes on
# other hosts talk to it. Every variable: control-plane/README.md.

FROM golang:1.27-bookworm@sha256:8d48e12ec56735e9358640898b9d9b9fcca110612ed8a5567438c0a1baa24e66 AS build
WORKDIR /src/control-plane
COPY control-plane/go.mod control-plane/go.sum ./
RUN go mod download
COPY control-plane/ ./
ARG VERSION=dev
ARG COMMIT=
ARG DATE=
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w \
        -X github.com/luisgf/agent-sandbox-platform/control-plane/internal/version.Version=${VERSION} \
        -X github.com/luisgf/agent-sandbox-platform/control-plane/internal/version.Commit=${COMMIT} \
        -X github.com/luisgf/agent-sandbox-platform/control-plane/internal/version.Date=${DATE}" \
      -o /out/asp-control-plane ./cmd/api

# distroless has no shell to make a directory the nonroot user can write to: its debug
# variant has one, and only this stage uses it.
FROM gcr.io/distroless/static-debian12:debug-nonroot@sha256:d5563cc7f2f44313f332e91138cc8c6a158899afeeeab2fce3b0f9ccdb3cf9ee AS state
USER 0:0
RUN ["/busybox/sh", "-c", "mkdir /state"]

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=state --chown=65532:65532 /state /var/lib/asp
COPY --from=build /out/asp-control-plane /usr/local/bin/asp-control-plane
# The CA and the keys must not be in /tmp (the control plane refuses to run with them
# there once it has a database): the volume.
ENV ASP_LISTEN_ADDR=0.0.0.0:8080 \
    ASP_CA_CERT=/var/lib/asp/ca.crt \
    ASP_CA_KEY=/var/lib/asp/ca.key \
    ASP_OIDC_KEY=/var/lib/asp/oidc-key.pem \
    ASP_ATTEST_KEY=/var/lib/asp/attest-key.pem
VOLUME /var/lib/asp
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/asp-control-plane"]
