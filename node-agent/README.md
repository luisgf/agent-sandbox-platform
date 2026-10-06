# Node agent

Agente privilegiado en cada nodo de sandboxes. Habla con Cloud Hypervisor vía HTTP sobre Unix socket, se enrolla/registra en el control plane (mTLS opcional), expone proxy localhost de exec/egress-check hacia `pod-daemon`, identity proxy OIDC, bridge de SSH agent, **host-vsock** guest→host y **TAP auto**.

## Flags

| Flag | Env | Default |
|---|---|---|
| `--control-plane-url` | `CONTROL_PLANE_URL` | `http://127.0.0.1:8080` |
| `--node-id` | `NODE_ID` | CN del cert enrolado en `--cert-dir`; si no hay, hostname |
| `--ch-socket-dir` | `CH_SOCKET_DIR` | `/run/asp` — sockets `ch-{id}.sock` (default per-sandbox spawn) |
| `--ch-api-socket` | `CH_API_SOCKET` | vacío — shared/legacy override (sin spawn) |
| `--ch-binary` | `CLOUD_HYPERVISOR_BIN` | `cloud-hypervisor` |
| `--dry-run` | `DRY_RUN=1` | false — usa `FakeVMM` |
| `--reap-leftovers` | `ASP_REAP_LEFTOVERS` | `on` — al arrancar, antes de registrarse, para y borra lo que dejó un node-agent anterior: `cloud-hypervisor`/`virtiofsd` de `--ch-socket-dir`, sus sockets, TAPs `asp-*`, túneles `wg-asp-*`, copias en `--disk-dir`. `report` solo lo lista; `off`. Con `--dry-run` solo informa ([bare-metal §5.6](../docs/bare-metal-ch.md#56-servicio-systemd-y-reinicios-del-agente)) |
| `--reap-only` | | hace solo esa limpieza y sale; se niega si corre un agente con ese `--ch-socket-dir` (lock `node-agent.lock`) |
| `--enroll` | `ASP_ENROLL=1` | enrollment con bootstrap token |
| `--bootstrap-token` | `ASP_NODE_BOOTSTRAP_TOKEN` | token de enroll |
| `--cert-dir` | `ASP_CERT_DIR` | dir de client certs |
| `--mtls` | `ASP_MTLS=1` | exigir client certs |
| `--control-plane-ca` | `ASP_CONTROL_PLANE_CA` | CA del cert TLS del CP (enroll y llamadas); por defecto `cert-dir/ca.crt` |
| `--enroll-url` | `ASP_ENROLL_URL` | URL de enroll si no es `--control-plane-url` (`ASP_MTLS_STRICT`) |
| `--agent-listen` | `ASP_AGENT_LISTEN` | `127.0.0.1:9100` — HTTP sin autenticar; fuera de loopback no arranca |
| `--insecure-agent-listen` | `ASP_INSECURE_AGENT_LISTEN=1` | permite `--agent-listen` fuera de loopback (solo lab) |
| `--capacity-cpu` | `ASP_CAPACITY_CPU` | `-1` = núcleos del host; `0` = no limita |
| `--capacity-mem-mib` | `ASP_CAPACITY_MEM_MIB` | `-1` = `MemTotal − max(1 GiB, 10 %)`; `0` = no limita |
| `--max-sandboxes` | `ASP_MAX_SANDBOXES` | `0` = sin tope |
| `--local-net-dial` | `ASP_LOCAL_NET_DIAL` | `host[:puerto]` que marca el portátil para local-net en este nodo |
| `--agent-tls-listen` | `ASP_AGENT_TLS_LISTEN` | vacío — `exec` con mTLS para un CP en otro host (p.ej. `0.0.0.0:9443`); solo acepta el cert del CP |
| `--endpoint` | `NODE_ENDPOINT` | anunciado al CP; por defecto `https://<hostname>:<puerto>` con `--agent-tls-listen`, si no `http://<agent-listen>` |
| `--pod-daemon-sock` | `ASP_POD_DAEMON_SOCK` | unix sock de pod-daemon (dry-run / fallback) |
| `--pod-daemon-port` | | `26500` — puerto guest vsock/TCP para HTTP |
| `--egress-enforce` | `ASP_EGRESS_ENFORCE=1` | 403 en egress-check denegado |
| `--egress-proxy-listen` | `ASP_EGRESS_PROXY_LISTEN` | forward proxy HTTP(S) (p.ej. `:8888`). `ASP_EGRESS_MAX_BODY` (8 MiB) limita el body de las peticiones HTTP (413); las respuestas pasan enteras, como por un túnel CONNECT. No reenvía cabeceras hop-by-hop (`Connection`, `Upgrade`, `Keep-Alive`, `Proxy-Authorization`…) |
| `--egress-dns-sink` | `ASP_EGRESS_DNS_SINK` | UDP DNS sink (p.ej. `:5353`): NXDOMAIN para nombres no permitidos |
| `--egress-mitm` | `ASP_EGRESS_MITM=1` | CONNECT TLS bump (default off; corp caution) |
| `--egress-mitm-ca` | `ASP_EGRESS_MITM_CA` | PEM CA MITM (generate/load) |
| `--ssh-agent-bridge` | `ASP_SSH_AGENT_BRIDGE` | unix sock bridge → `SSH_AUTH_SOCK` / FakeAgent |
| `--identity-listen` | `ASP_IDENTITY_LISTEN` | unix `.sock` o TCP para `POST /v1/tokens/oidc`; sin binding de sandbox: 403 salvo `--insecure-identity-sandbox-header` |
| `--default-sandbox-id` | `ASP_SANDBOX_ID` | sandbox de las peticiones sin `X-ASP-Sandbox-ID` en listeners sin binding; solo con `--insecure-identity-sandbox-header` |
| `--insecure-identity-sandbox-header` | `ASP_INSECURE_IDENTITY_SANDBOX_HEADER=1` | los listeners de identidad sin binding (`--identity-listen`, host-vsock global) toman la sandbox de `X-ASP-Sandbox-ID`: quien llegue a ellos pide el token de cualquier sandbox (solo lab) |
| `--reconcile` | `ASP_RECONCILE=1` | poll work / claim / Start-Stop VMM |
| `--reconcile-interval` | | default `2s` |
| `--tap-auto` | `ASP_TAP_AUTO=1` | crea/borra TAP `asp-{shortid}` con su propia /30 de `--guest-subnet` (soft-fail sin perms) |
| `--disk-dir` | `ASP_DISK_DIR` | `/var/lib/asp/disks` — copia privada del rootfs por sandbox (`rootfs-{id}.img`), borrada al parar; no aplica con `--dry-run` |
| `--host-vsock` | `ASP_HOST_VSOCK=1` | AF_VSOCK 26501 SSH + 26502 identity (guest CID 2) |
| `--host-vsock-dir` | `ASP_HOST_VSOCK_DIR` | lab: unix bajo este dir en vez de AF_VSOCK |
| `--ssh-agent-confirm` | `ASP_SSH_AGENT_CONFIRM` | exige approve one-shot; default on si multi-user/template (`=0` fuerza off) |
| `--ssh-agent-sock-template` | `ASP_SSH_AGENT_SOCK_TEMPLATE` | path template por sandbox (`{owner_sub}`/`{sandbox_id}`); missing → FakeAgent |
| `--multi-user` | `ASP_MULTI_USER=1` (o `ASP_IDP_REQUIRED=1`) | perfil multi-user: confirm default-on |
| `--egress-nft-redirect` / `--nft-egress-redirect` | `ASP_EGRESS_NFT_REDIRECT` / `ASP_NFT_EGRESS_REDIRECT` | nftables `asp_egress` HTTP+DNS redirect |
| `--nft-egress-mode` | `ASP_NFT_EGRESS_MODE` | `soft` (default) \| `enforce` |
| `--nft-http-ports` | `ASP_NFT_HTTP_PORTS` | default `80,443` |
| `--nft-dns-action` | `ASP_NFT_DNS_ACTION` | `redirect` (default) \| `drop` |
| `--guest-ssh-agent-auto` | `ASP_GUEST_SSH_AGENT_AUTO` | default on with `--host-vsock` / bridge |
| `--guest-subnet` | `ASP_GUEST_SUBNET` | `10.200.0.0/16` — pool de /30 por sandbox (TAP `.1`, guest `.2`) y match de las reglas nft |

```bash
go test ./...
go run ./cmd/node-agent \
  --control-plane-url=http://127.0.0.1:8080 --node-id=dev-node \
  --dry-run --reconcile --enroll --bootstrap-token=dev \
  --cert-dir=/tmp/asp-node-certs --agent-listen=127.0.0.1:9100 \
  --pod-daemon-sock=/tmp/pod-daemon.sock \
  --egress-enforce \
  --ssh-agent-bridge=/tmp/asp-ssh-agent.sock \
  --identity-listen=/tmp/asp-identity.sock --insecure-identity-sandbox-header \
  --host-vsock --host-vsock-dir=/tmp/asp-hv \
  --tap-auto
```

Endpoints internos (`--agent-listen`, loopback): `GET /healthz`, `POST /v1/internal/exec`, `POST /v1/internal/egress-check`, `POST /v1/internal/ssh-agent/approve` (con confirm; body/header `actor_sub` audit).

`--agent-tls-listen` solo sirve `GET /healthz`, `POST /v1/internal/exec` y `POST /v1/internal/exec/stdin` al plano de control (CN `asp-control-plane`). Necesita un cert de nodo con uso de servidor: los enrolados antes de [ADR-0011](../docs/adr/0011-multi-node.md) deben re-enrolar o rotar.

Identidad del guest ([ADR-0003](../docs/adr/0003-identity.md) § 2): el token es siempre el de la sandbox de la conexión. Con `--host-vsock --reconcile` cada sandbox tiene su `{vsock}_26502`, y el proxy liga ese id a cada petición; si el guest manda un `X-ASP-Sandbox-ID` de otra sandbox, 403. `--identity-listen` y los listeners host-vsock globales no saben qué guest llama: devuelven 403 salvo con `--insecure-identity-sandbox-header` (lab), que vuelve a confiar en la cabecera.

ADR-0007 fase 4 (SSH scoped): con template, cada sandbox usa su HostSock (ServeConnScoped, sin fallback a `SSH_AUTH_SOCK` global). Ops provisiona las keys en esa ruta; ASP no spawnea agents.

Guest→host: ver [`scripts/guest-vsock-notes.md`](../scripts/guest-vsock-notes.md).

Fase 2e (nft + SSH guest auto): [`docs/why-2e-nft-redirect.md`](../docs/why-2e-nft-redirect.md), [`docs/why-2e-ssh-guest-mount.md`](../docs/why-2e-ssh-guest-mount.md), ADR-0006.
