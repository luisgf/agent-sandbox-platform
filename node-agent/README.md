# Node agent

Agente privilegiado en cada nodo de sandboxes. Habla con Cloud Hypervisor vía HTTP sobre Unix socket, se enrolla/registra en el control plane (mTLS opcional), expone proxy localhost de exec/egress-check hacia `pod-daemon`, identity proxy OIDC, bridge de SSH agent, **host-vsock** guest→host y **TAP auto**.

## Flags

| Flag | Env | Default |
|---|---|---|
| `--control-plane-url` | `CONTROL_PLANE_URL` | `http://127.0.0.1:8080` |
| `--node-id` | `NODE_ID` | hostname |
| `--ch-socket-dir` | `CH_SOCKET_DIR` | `/run/asp` — sockets `ch-{id}.sock` (default per-sandbox spawn) |
| `--ch-api-socket` | `CH_API_SOCKET` | vacío — shared/legacy override (sin spawn) |
| `--ch-binary` | `CLOUD_HYPERVISOR_BIN` | `cloud-hypervisor` |
| `--dry-run` | `DRY_RUN=1` | false — usa `FakeVMM` |
| `--enroll` | `ASP_ENROLL=1` | enrollment con bootstrap token |
| `--bootstrap-token` | `ASP_NODE_BOOTSTRAP_TOKEN` | token de enroll |
| `--cert-dir` | `ASP_CERT_DIR` | dir de client certs |
| `--mtls` | `ASP_MTLS=1` | exigir client certs |
| `--agent-listen` | `ASP_AGENT_LISTEN` | `127.0.0.1:9100` |
| `--pod-daemon-sock` | `ASP_POD_DAEMON_SOCK` | unix sock de pod-daemon (dry-run / fallback) |
| `--pod-daemon-port` | | `26500` — puerto guest vsock/TCP para HTTP |
| `--egress-enforce` | `ASP_EGRESS_ENFORCE=1` | 403 en egress-check denegado |
| `--egress-proxy-listen` | `ASP_EGRESS_PROXY_LISTEN` | forward proxy HTTP(S) (p.ej. `:8888`) |
| `--egress-dns-sink` | `ASP_EGRESS_DNS_SINK` | UDP DNS sink NXDOMAIN (p.ej. `:5353`) |
| `--egress-mitm` | `ASP_EGRESS_MITM=1` | CONNECT TLS bump (default off; corp caution) |
| `--egress-mitm-ca` | `ASP_EGRESS_MITM_CA` | PEM CA MITM (generate/load) |
| `--ssh-agent-bridge` | `ASP_SSH_AGENT_BRIDGE` | unix sock bridge → `SSH_AUTH_SOCK` / FakeAgent |
| `--identity-listen` | `ASP_IDENTITY_LISTEN` | unix `.sock` o TCP para `POST /v1/tokens/oidc` |
| `--default-sandbox-id` | `ASP_SANDBOX_ID` | sandbox por defecto del identity proxy |
| `--reconcile` | `ASP_RECONCILE=1` | poll work / claim / Start-Stop VMM |
| `--reconcile-interval` | | default `2s` |
| `--tap-auto` | `ASP_TAP_AUTO=1` | crea/borra TAP `asp-{shortid}` (soft-fail sin perms) |
| `--host-vsock` | `ASP_HOST_VSOCK=1` | AF_VSOCK 26501 SSH + 26502 identity (guest CID 2) |
| `--host-vsock-dir` | `ASP_HOST_VSOCK_DIR` | lab: unix bajo este dir en vez de AF_VSOCK |
| `--ssh-agent-confirm` | `ASP_SSH_AGENT_CONFIRM=1` | exige approve one-shot antes de SignRequest |
| `--egress-nft-redirect` / `--nft-egress-redirect` | `ASP_EGRESS_NFT_REDIRECT` / `ASP_NFT_EGRESS_REDIRECT` | nftables `asp_egress` HTTP+DNS redirect |
| `--nft-egress-mode` | `ASP_NFT_EGRESS_MODE` | `soft` (default) \| `enforce` |
| `--nft-http-ports` | `ASP_NFT_HTTP_PORTS` | default `80,443` |
| `--nft-dns-action` | `ASP_NFT_DNS_ACTION` | `redirect` (default) \| `drop` |
| `--guest-ssh-agent-auto` | `ASP_GUEST_SSH_AGENT_AUTO` | default on with `--host-vsock` / bridge |
| `--guest-subnet` | `ASP_GUEST_SUBNET` | `10.200.0.0/16` — CIDR para nft redirect |

```bash
go test ./...
go run ./cmd/node-agent \
  --control-plane-url=http://127.0.0.1:8080 --node-id=dev-node \
  --dry-run --reconcile --enroll --bootstrap-token=dev \
  --cert-dir=/tmp/asp-node-certs --agent-listen=127.0.0.1:9100 \
  --pod-daemon-sock=/tmp/pod-daemon.sock \
  --egress-enforce \
  --ssh-agent-bridge=/tmp/asp-ssh-agent.sock \
  --identity-listen=/tmp/asp-identity.sock \
  --host-vsock --host-vsock-dir=/tmp/asp-hv \
  --tap-auto
```

Endpoints internos (`--agent-listen`): `GET /healthz`, `POST /v1/internal/exec`, `POST /v1/internal/egress-check`, `POST /v1/internal/ssh-agent/approve` (con `--ssh-agent-confirm`).

Guest→host: ver [`scripts/guest-vsock-notes.md`](../scripts/guest-vsock-notes.md).

Fase 2e (nft + SSH guest auto): [`docs/why-2e-nft-redirect.md`](../docs/why-2e-nft-redirect.md), [`docs/why-2e-ssh-guest-mount.md`](../docs/why-2e-ssh-guest-mount.md), ADR-0006.
