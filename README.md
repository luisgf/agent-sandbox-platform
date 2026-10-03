# Agent Sandbox Platform

Diseño y esqueleto de referencia para una plataforma de sandboxes de agentes al estilo de un IDE agente: **VMM FOSS + plano de control propio**. Cada sandbox corre dentro de una microVM y se administra con un daemon invitado (`pod-daemon`) y un canal host–guest explícito (vsock).

> **Proyecto independiente.** No está afiliado, patrocinado ni respaldado por Cursor, Anysphere, anyrun ni sus compañías o productos relacionados. Está inspirado en ese tipo de sandboxes; el código y el threat model son propios.

## Para qué sirve

Un agente es un proceso **largo**. La forma primaria de usar el aislamiento es una **sesión** atada a un sandbox (identidad `owner_sub` del IdP, workspace del guest, egress, idle) — no una microVM por comando de shell. Dirección: [ADR-0009](docs/adr/0009-agent-sessions.md) · [por qué](docs/why-agent-sessions.md). `asp sandbox run` queda como primitiva interna (CI / un comando).

- Aislar workloads de agentes (código no confiable) con frontera **microVM** (Cloud Hypervisor por defecto; `FakeVMM` en dry-run/CI).
- Orquestar el ciclo de vida multi-tenant vía API (`control-plane`) y reconciliación en el nodo (`node-agent --reconcile`).
- Ejecutar comandos y (según evolución) archivos a través de `pod-daemon` por vsock — sin exponer el hipervisor al cliente.
- Controlar **egress deny-by-default** con forward proxy HTTP(S) + DNS sink + nft redirect (`soft|enforce`).
- Mantener **credenciales fuera del guest**: SSH agent host-held (vsock 26501) y tokens OIDC de corta vida (26502 / identity proxy).

## Qué no es

- Un RuntimeClass de Kubernetes ni “microVMs como Pods” (ver [ADR-0004](docs/adr/0004-k8s-scope.md)).
- Attestation TPM/SEV de hardware (hoy: firma software de `BootStatement`; interfaz lista para plug-ins).
- STONITH BMC de producción (hay leases + `FenceProvider` stub/webhook; BMC real es ops).
- Un SDK multi-lenguaje: la CLI `asp` es demo/ops sobre el HTTP API existente.
- Magia SoftFail: sin root/`nft`/KVM, CI demuestra el plano de control, **no** bypass-proof ni aislamiento real.

## Diagrama de arquitectura

![Arquitectura Agent Sandbox Platform](docs/diagram.svg)

Vista editable: [`docs/diagram.mmd`](docs/diagram.mmd). Regenerar SVG: `./scripts/gen-diagram.sh`.

Narrativa completa (threat model, trust boundaries, identidad, leases): [`docs/architecture.md`](docs/architecture.md).

## Estado del MVP — **solution complete** (+ hardening 2b–2f)

| Área | Contenido |
|---|---|
| Control plane | sandboxes/nodes/events; API keys; enrollment PKI; exec proxy; egress; OIDC; attest; leases; cert rotate/revoke; `ASP_MTLS_STRICT` |
| Node-agent | CH spawn / FakeVMM; reconciler; hybrid vsock exec; host-vsock 26501/26502; TAP auto; egress proxy+DNS; nft soft\|enforce; SSH confirm; guest SSH auto |
| pod-daemon | HTTP JSON unix/vsock/tcp; `ASP_HOST_CID=2` |
| Guest image | Dockerfile + systemd/OpenRC + `vsock-ssh-agent-proxy` + auto-mount virtiofs `workspace` |
| CLI | `asp` (`make asp`) — `session start/exec/stop` (agente), `sandbox run` (primitiva), `auth login` (IdP Bearer) |
| Pack | `make pack` → tarball de release |

**Aún no:** bypass-proof nft en hardware (CI = soft/dry-run); TPM/SEV; Windows guests; virtiofs SSH automatizado; **prueba KVM del share** (el nodo sí arranca virtiofsd y la imagen nueva monta `/workspace`; CI no bootea la VM) y **plugin OpenCode** — la sesión con nombre y el exec NDJSON ya están ([ADR-0009](docs/adr/0009-agent-sessions.md)).

## Cómo funciona (mapa rápido)

```text
Cliente / asp ──HTTPS+API key──► Control plane
                                    │ mTLS desired state
                                    ▼
                              Node agent ──Start/Stop──► Cloud Hypervisor / FakeVMM
                                    │                         │
                     exec 26500 ◄───┼──── hybrid vsock ───────┤
                     SSH  26501 ◄───┼──── guest→host ─────────┤
                     OIDC 26502 ◄───┼─────────────────────────┘
                                    │
                              TAP → nft asp_egress → proxy :8888 → Internet (allowlist)
```

## Límites (honestos, no negociables en docs)

| Tema | Realidad en código |
|---|---|
| Dry-run | `--dry-run` = FakeVMM; útil para CI; no es KVM |
| nft | `--nft-egress-mode=soft` SoftFail sin root; `enforce` exige privilegios |
| Attest | Software ECDSA (`ASP_ATTEST_KEY`) ≠ TPM/SEV |
| Leases | TTL software + FenceProvider opcional ≠ STONITH BMC |
| Idle stop | `ASP_SANDBOX_IDLE_TIMEOUT` apagado por defecto (smokes); lab systemd usa `2h`. Actividad = create, paso a `running`, exec OK. No es un GC del fichero `asp session` |
| K8s | Opcional solo para desplegar el CP; sandboxes no son Pods |

## Mapa de documentación

| Doc | Contenido |
|---|---|
| [`docs/architecture.md`](docs/architecture.md) | Arquitectura, threat model, flujos |
| [`docs/diagram.svg`](docs/diagram.svg) / [`.mmd`](docs/diagram.mmd) | Diagrama |
| [`docs/roadmap.md`](docs/roadmap.md) | Fases 0–2f + gaps; sesión primero ([ADR-0009](docs/adr/0009-agent-sessions.md)) |
| [`docs/why-agent-sessions.md`](docs/why-agent-sessions.md) | Por qué la sesión es el producto y el one-shot no |
| [`docs/mvp-smoke.md`](docs/mvp-smoke.md) | Smoke dry-run (sin KVM) |
| [`docs/bare-metal-ch.md`](docs/bare-metal-ch.md) | Ops CH + KVM real |
| [`docs/ops-idp-keycloak-lab.md`](docs/ops-idp-keycloak-lab.md) | Lab Keycloak IdP (realm asp) + systemd CP — secretos fuera de git |
| [`docs/ops-asp-agent-runner.md`](docs/ops-asp-agent-runner.md) | Primitiva one-shot + `asp auth` / Bearer (no es la superficie del agente) |
| [`docs/ops-asp-session.md`](docs/ops-asp-session.md) | CLI de la sesión: `asp session` para el shell del harness (OpenCode) |
| [`docs/adr/`](docs/adr/) | Decisiones (0001–0009) |
| [`docs/why-*.md`](docs/) | Por qué / qué ganamos (2d, 2e, CLI, sesiones, multi-user, network-flow attribution) |
| [`scripts/guest-vsock-notes.md`](scripts/guest-vsock-notes.md) | Puertos vsock |

### ADRs

1. [VMM: Cloud Hypervisor](docs/adr/0001-vmm-choice.md)
2. [Red y egress](docs/adr/0002-networking.md)
3. [Identidad SSH/OIDC](docs/adr/0003-identity.md)
4. [Alcance de Kubernetes](docs/adr/0004-k8s-scope.md)
5. [Fase 2d hardening](docs/adr/0005-fase-2d-hardening.md)
6. [Fase 2e nft + SSH guest](docs/adr/0006-fase-2e-nft-ssh-guest.md)
7. [Identidad multi-usuario / IdP](docs/adr/0007-multi-user-identity.md) — fases 1–5 (schema + JWT IdP + RBAC + SSH scoped + workload user_sub/act); ver [`docs/why-multi-user-identity.md`](docs/why-multi-user-identity.md) · lab Keycloak: [`docs/ops-idp-keycloak-lab.md`](docs/ops-idp-keycloak-lab.md)
8. [Atribución de flujos de red → owner_sub](docs/adr/0008-network-flow-attribution.md) — **evaluación** (no implementada); ver [`docs/why-network-flow-attribution.md`](docs/why-network-flow-attribution.md)
9. [Sesiones de agente](docs/adr/0009-agent-sessions.md) — **aceptada como dirección**: la sesión es la forma primaria de aislamiento; el one-shot es primitiva interna. Ver [`docs/why-agent-sessions.md`](docs/why-agent-sessions.md)

## Mapa de componentes

| Ruta | Responsabilidad |
|---|---|
| [`control-plane/`](control-plane/) | API HTTP/TLS, tenancy, PKI, OIDC, attest, leases |
| [`node-agent/`](node-agent/) | VMM, reconciler, egress/nft, host-vsock, TAP |
| [`pod-daemon/`](pod-daemon/) | API en el guest (exec) |
| [`images/guest/`](images/guest/) | Rootfs Debian + helpers SSH |
| [`cli/`](cli/) | Demo CLI `asp` |
| [`docs/`](docs/) | Arquitectura, ADRs, ops |
| [`scripts/`](scripts/) | Smokes, pack, nft, rootfs, diagrama |

## Quickstart (dry-run)

Requisitos: Go 1.22+, Rust/Cargo. Docker opcional (Postgres / guest rootfs). **No hace falta KVM.**

```bash
make test
make smoke          # smokes enroll/identity/reconcile
make asp            # → build/asp
make smoke-asp      # CLI e2e dry-run
```

Arranque manual mínimo (tres terminales):

```bash
export ASP_NODE_BOOTSTRAP_TOKEN=dev-node-bootstrap
(cd control-plane && go run ./cmd/api)

(cd pod-daemon && cargo run -- --listen unix --unix-socket /tmp/pod-daemon.sock)

(cd node-agent && go run ./cmd/node-agent \
  --control-plane-url=http://127.0.0.1:8080 --node-id=dev-node \
  --dry-run --enroll --bootstrap-token=dev-node-bootstrap \
  --cert-dir=/tmp/asp-node-certs --agent-listen=127.0.0.1:9100 \
  --pod-daemon-sock=/tmp/pod-daemon.sock \
  --reconcile \
  --host-vsock --host-vsock-dir=/tmp/asp-hv \
  --ssh-agent-bridge=/tmp/asp-ssh-agent.sock \
  --guest-ssh-agent-auto \
  --identity-listen=/tmp/asp-identity.sock)
# Opcional nft soft: --egress-proxy-listen=:8888 --nft-egress-redirect --nft-egress-mode=soft
```

Sesión de agente (forma primaria; dry-run local). El harness engancha el shell a `session exec` durante horas; **no** hay sync del workspace del host:

```bash
./build/asp session start --node-id=dev-node
./build/asp session exec --cmd 'echo hello'
./build/asp session stop
```

Dirección y límites: [ADR-0009](docs/adr/0009-agent-sessions.md) · [`docs/why-agent-sessions.md`](docs/why-agent-sessions.md) · contrato CLI [`docs/ops-asp-session.md`](docs/ops-asp-session.md).

Primitiva one-shot (CI / un solo comando, no el bucle del agente):

```bash
./build/asp sandbox run --node-id=dev-node --cmd 'echo hello'
```

Lab IdP (ncc1701d, CP `127.0.0.1:18112` — secretos en el host), misma primitiva o `session start` con `--tenant=default`:

```bash
export ASP_CP_URL=http://127.0.0.1:18112 ASP_IDP_REQUIRED=1
./build/asp sandbox run --tenant=default --cmd 'echo hello'
```

Auth del Bearer y por qué el one-shot no es la integración: [`docs/ops-asp-agent-runner.md`](docs/ops-asp-agent-runner.md).

Detalle de precondiciones, resultados esperados y fallos: [`docs/mvp-smoke.md`](docs/mvp-smoke.md).  
Host con KVM: [`docs/bare-metal-ch.md`](docs/bare-metal-ch.md).

### Postgres opcional

```bash
docker compose up -d postgres
export DATABASE_URL='postgres://asp:asp@127.0.0.1:5432/asp?sslmode=disable'
(cd control-plane && DATABASE_URL="$DATABASE_URL" go test ./... -count=1)
```

## Vsock ports

| Port | Direction | Role |
|---|---|---|
| 26500 | host→guest | pod-daemon HTTP |
| 26501 | guest→host | SSH agent |
| 26502 | guest→host | OIDC identity |

Detalle: [`scripts/guest-vsock-notes.md`](scripts/guest-vsock-notes.md).
