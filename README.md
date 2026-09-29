# Agent Sandbox Platform

Diseño y esqueleto de referencia para una plataforma de sandboxes de agentes al estilo de un IDE agente: **VMM FOSS + plano de control propio**. Cada sandbox se ejecuta dentro de una microVM y se administra mediante un daemon invitado con un canal host–guest explícito.

> **Proyecto independiente.** No está afiliado, patrocinado ni respaldado por Cursor, Anysphere, anyrun ni sus compañías o productos relacionados.

## Objetivos

- Aislamiento fuerte mediante microVMs con Cloud Hypervisor por defecto.
- API multi-tenant y registro auditable del ciclo de vida.
- Ejecución y transferencia de archivos a través de `pod-daemon` por vsock (unix en dry-run).
- Egreso denegado por defecto, mediado por proxies HTTP y DNS.
- Credenciales fuera del disco invitado: SSH agent reenviado y tokens OIDC de corta vida.
- Despliegue operativo sencillo: nodos de sandbox fuera de Kubernetes.

## Estado del MVP — **solution complete**

- **Control plane**: sandboxes/nodes/events; API keys; enrollment PKI; exec proxy; tenant egress; OIDC discovery/JWKS/mint.
- **Node-agent**: CH / FakeVMM; enroll/mTLS; exec + egress-check; identity proxy; SSH agent bridge; **reconciler**; **per-sandbox CH spawn**; **hybrid vsock exec**; **`--host-vsock`** (26501 SSH / 26502 identity); **`--tap-auto`**.
- **pod-daemon**: HTTP JSON unix/vsock/tcp; `ASP_HOST_CID=2`.
- **Guest image**: Dockerfile + systemd/OpenRC; `scripts/build-guest-rootfs.sh`.
- **Pack**: `make pack` → `/workspace/agent-sandbox-platform-release.tar.gz`.
- **Ops**: [`docs/bare-metal-ch.md`](docs/bare-metal-ch.md) e2e; límites conocidos en [`docs/roadmap.md`](docs/roadmap.md).

**Post-MVP hecho:** egress HTTP forward proxy + DNS sink; leases multi-nodo; rotación OIDC `ASP_OIDC_KEY_PREV`; **Fase 2c:** remote attestation MVP, FenceProvider, proxy hardening; **Fase 2d:** rotación/revocación certs, `ASP_MTLS_STRICT`, SSH confirm; **Fase 2e:** nft redirect completo (HTTP+DNS, soft|enforce) + SSH agent auto en guest (vsock proxy). **CLI `asp`:** demo lifecycle (`cli/`, `docs/why-cli-asp.md`).

**Aún no:** bypass-proof nft en hardware (CI = soft/dry-run); TPM/SEV hardware attest; Windows guests; virtiofs SSH (alternativa manual; auto = vsock).

Smokes: [`docs/mvp-smoke.md`](docs/mvp-smoke.md), `make smoke`.

## Mapa de componentes

| Ruta | Responsabilidad |
|---|---|
| [`control-plane/`](control-plane/) | API HTTP/TLS, estado deseado, tenancy, PKI enrollment, inventario y exec proxy |
| [`node-agent/`](node-agent/) | Ciclo de vida de microVM, enrollment/mTLS, exec proxy, host-vsock, TAP |
| [`pod-daemon/`](pod-daemon/) | API dentro del guest para exec (unix/vsock/tcp) |
| [`images/guest/`](images/guest/) | Imagen Debian mínima y empaquetado de `pod-daemon` |
| [`docs/`](docs/) | Arquitectura, diagrama, roadmap y decisiones |
| [`cli/`](cli/) | Demo CLI `asp` (sandbox create/get/list/exec/delete/run) |
| [`scripts/`](scripts/) | Smokes, pack, guest rootfs, vsock notes |

Flujo principal:

```text
Cliente → Control plane → Node agent (localhost exec) → hybrid vsock CONNECT 26500 → pod-daemon
Guest → AF_VSOCK CID 2 :26501/26502 → node-agent SSH agent / identity
```

## Cómo leer las decisiones

Las decisiones vinculantes están en [`docs/adr/`](docs/adr/):

1. [VMM: Cloud Hypervisor](docs/adr/0001-vmm-choice.md)
2. [Red y egress](docs/adr/0002-networking.md)
3. [Identidad SSH/OIDC](docs/adr/0003-identity.md)
4. [Alcance de Kubernetes](docs/adr/0004-k8s-scope.md)
5. [Fase 2d hardening](docs/adr/0005-fase-2d-hardening.md)
6. [Fase 2e nft + SSH guest](docs/adr/0006-fase-2e-nft-ssh-guest.md)

Empieza por [`docs/architecture.md`](docs/architecture.md), consulta el [diagrama Mermaid](docs/diagram.mmd) y usa el [roadmap](docs/roadmap.md). Para CH real en host KVM: [`docs/bare-metal-ch.md`](docs/bare-metal-ch.md).

## Desarrollo local

Requisitos: Go 1.22+, Rust estable y Cargo. Docker opcional para Postgres / guest rootfs.

```bash
make test
make smoke
make asp          # binario build/asp
make smoke-asp    # opcional: CLI e2e dry-run
make pack   # → /workspace/agent-sandbox-platform-release.tar.gz

# Postgres smoke (opcional):
docker compose up -d postgres
export DATABASE_URL='postgres://asp:asp@127.0.0.1:5432/asp?sslmode=disable'
(cd control-plane && DATABASE_URL="$DATABASE_URL" go test ./... -count=1)
```

Arranque de demostración:

```bash
export ASP_NODE_BOOTSTRAP_TOKEN=dev-node-bootstrap
(cd control-plane && go run ./cmd/api)

# Otra terminal — pod-daemon + node-agent dry-run:
(cd pod-daemon && cargo run -- --listen unix --unix-socket /tmp/pod-daemon.sock)
(cd node-agent && go run ./cmd/node-agent \
  --control-plane-url=http://127.0.0.1:8080 --node-id=dev-node \
  --dry-run --enroll --bootstrap-token=dev-node-bootstrap \
  --cert-dir=/tmp/asp-node-certs --agent-listen=127.0.0.1:9100 \
  --pod-daemon-sock=/tmp/pod-daemon.sock \
  --host-vsock --host-vsock-dir=/tmp/asp-hv \
  --ssh-agent-bridge=/tmp/asp-ssh-agent.sock \
  --guest-ssh-agent-auto \
  --identity-listen=/tmp/asp-identity.sock)
# nft soft (dry-run / no root): add --egress-proxy-listen=:8888 --nft-egress-redirect --nft-egress-mode=soft
```

### CLI `asp` (ciclo de vida)

```bash
make asp   # → build/asp
# Requiere CP + pod-daemon + node-agent dry-run con --reconcile
# (docs/mvp-smoke.md §4) y pin al node-id enrollado:
./build/asp sandbox run --node-id=dev-node --cmd 'echo hello'
# Building blocks: create | get | list | exec | delete
# Docs: docs/why-cli-asp.md · smoke: make smoke-asp
```

Ver también [`docs/mvp-smoke.md`](docs/mvp-smoke.md) (dry-run) y [`docs/bare-metal-ch.md`](docs/bare-metal-ch.md) (CH + KVM real).

## Vsock ports

| Port | Direction | Role |
|---|---|---|
| 26500 | host→guest | pod-daemon HTTP |
| 26501 | guest→host | SSH agent |
| 26502 | guest→host | OIDC identity |

Detalle: [`scripts/guest-vsock-notes.md`](scripts/guest-vsock-notes.md).
