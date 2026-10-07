# syntax=docker/dockerfile:1.7
# The control plane image goreleaser builds at a release, from the binary it already
# built (the build context holds <os>/<arch>/asp-control-plane). control-plane/Dockerfile
# is the same image built from source; keep the runtime stages the same.
FROM gcr.io/distroless/static-debian12:debug-nonroot@sha256:d5563cc7f2f44313f332e91138cc8c6a158899afeeeab2fce3b0f9ccdb3cf9ee AS state
USER 0:0
RUN ["/busybox/sh", "-c", "mkdir /state"]

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG TARGETPLATFORM
COPY --from=state --chown=65532:65532 /state /var/lib/asp
COPY ${TARGETPLATFORM}/asp-control-plane /usr/local/bin/asp-control-plane
ENV ASP_LISTEN_ADDR=0.0.0.0:8080 \
    ASP_CA_CERT=/var/lib/asp/ca.crt \
    ASP_CA_KEY=/var/lib/asp/ca.key \
    ASP_OIDC_KEY=/var/lib/asp/oidc-key.pem \
    ASP_ATTEST_KEY=/var/lib/asp/attest-key.pem
VOLUME /var/lib/asp
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/asp-control-plane"]
