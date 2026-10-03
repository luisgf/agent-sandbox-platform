# Agent Sandbox Platform

Plataforma **FOSS** de sandboxes para agentes. Aísla el trabajo del agente del host: un plano de control propio, un node-agent en el nodo, y una **microVM** (Cloud Hypervisor por defecto; `FakeVMM` en dry-run y CI). El objeto de uso es una **sesión** larga, no una VM por comando. La identidad del dueño sale de un **IdP**. El egress del guest es deny-by-default.

> Proyecto independiente. No está afiliado, patrocinado ni respaldado por Cursor, Anysphere, anyrun ni productos relacionados. El diseño se inspira en ese tipo de sandboxes; el código y el threat model son propios.

## Qué es y qué hace

Cinco piezas, y solo esas, forman el sistema de hoy:

| Pieza | Qué hace |
|---|---|
| **Control plane** (`control-plane/`, Go) | API multi-tenant: sandboxes, nodos, exec, egress, leases, attest software. El `owner_sub` lo pone el JWT del IdP, no el guest ni un campo del cliente. |
| **Node agent** (`node-agent/`, Go) | Único proceso que habla con el VMM. Reconcilia el estado deseado, abre TAP, aplica nft (`soft` o `enforce`) y los puentes vsock. |
| **microVM** | Cloud Hypervisor + rootfs Debian. Dentro, `pod-daemon` (Rust) ejecuta comandos por vsock. El cliente no ve el socket del hipervisor. |
| **Sesión** (`asp session`) | Un sandbox para un agente que dura horas: mismo disco del guest, mismo egress, mismo dueño, muchos `exec`. `asp sandbox run` queda como primitiva de un solo comando (CI / ops). |
| **Egress** | Sin opt-in de red local: TAP, forward proxy HTTP(S), DNS sink y nft. Las credenciales largas no entran al guest (SSH agent en el host, tokens OIDC cortos). |

Dry-run (`--dry-run` / `FakeVMM`) ejercita el plano de control **sin KVM**. No es aislamiento real.

## Para qué sirve

Para que el código que escribe o ejecuta un agente **no corra en el host**.

La frontera primaria es la microVM. El harness habla solo con el control plane. No recibe la clave SSH del operador, ni el socket de Cloud Hypervisor, ni una red abierta. Un comando que el modelo lanza entra por `asp session exec` y sale dentro del guest. Si el agente es largo, esa frontera se mantiene entre tools: el disco del guest, el egress y el `owner_sub` siguen siendo los de la misma sesión.

No sustituye a Kubernetes (los sandboxes no son Pods), no atestigua con TPM/SEV y no es un SDK multi-lenguaje. La CLI `asp` es el contrato de ops y de integración.

## Casos de uso

### Harness estilo OpenCode, vía `asp session`

Un agente llama al shell decenas de veces. Pagar un boot de microVM por tool hace que el harness acabe ejecutando en el host. La sesión evita eso:

```bash
asp session start --name opencode --node-id=dev-node --workspace /ruta/absoluta/del/repo
asp session exec --name opencode --cmd 'echo hello'
asp session stop --name opencode
```

El harness **no** es un plugin de este repo. Se sustituye el shell del tool por un wrapper que llama a `asp session exec --name …`. El JSON local (`~/.cache/asp/sessions/<nombre>.json`) guarda id y URL, **sin** token. Contrato y wrapper: [`docs/ops-asp-session.md`](docs/ops-asp-session.md).

### Lab IdP (Keycloak)

En el lab, el control plane exige Bearer de Keycloak (realm `asp`, cliente `asp-api`). El grupo del token decide el rol; el `sub` queda como `owner_sub` del sandbox. Secretos fuera de git. No es Entra ni Okta, y el CP de ese lab escucha en loopback. Guía: [`docs/ops-idp-keycloak-lab.md`](docs/ops-idp-keycloak-lab.md).

### Red local bajo demanda (túnel completo, opt-in)

Cuando el trabajo está en la LAN de quien lanza el agente, `asp session start --local-net` pide que la ruta por defecto **de esa sesión** (`0.0.0.0/0`, y `::/0` si existe) salga por un túnel que abre el agente local. No hay lista de CIDR en v1. Si el agente no está, el plan es blackhole: no se vuelve en silencio al proxy del nodo.

**Hoy eso es plano de control y CLI.** El flag, el grant, el heartbeat y el plan de blackhole están. **No hay dispositivo WireGuard**, ni `wg` aplicado, ni NAT. El esqueleto de config no se instala solo. Sin `wireguard-tools` y sin privilegios de red no hay paquetes. Ops: [`docs/ops-local-net.md`](docs/ops-local-net.md).

### Parada por inactividad

`ASP_SANDBOX_IDLE_TIMEOUT` apaga un sandbox olvidado. El binario lo trae **apagado** (los smokes no deben morir solos). El unit de lab usa `2h`. Cuentan como actividad el create, el paso a `running` y un exec que el control plane proxyó bien. `status`, el heartbeat del nodo y el heartbeat de local-net **no** refrescan el reloj. El reaper no borra el JSON de `asp session`: el siguiente `exec` lo dice y sale 1.

### Workspace por virtiofs

`asp session start --workspace /ruta/absoluta` hace que el node-agent arranque `virtiofsd` (tag `workspace`) y que Cloud Hypervisor reciba el `fs`. El árbol del host no se copia al crear la sesión: el guest lo ve montado.

El **auto-mount está en el build de la imagen** (`workspace-virtiofs.service` y el helper, copiados por `scripts/build-guest-rootfs.sh` y por el Dockerfile). **El rootfs que ya corre en ncc1701d no se ha reconstruido.** Hasta apuntar `/opt/sandbox/rootfs.img` a una imagen nueva, dentro de ese guest sigue haciendo falta:

```sh
mkdir -p /workspace && mount -t virtiofs workspace /workspace
```

Sin `virtiofsd` en el nodo, un start con `--workspace` falla: no se arranca una VM que finja el directorio. CI no bootea la VM; no hay prueba KVM del share. Detalle: [`docs/why-virtiofs-pty.md`](docs/why-virtiofs-pty.md).

## Cómo encaja

La sesión es lo que usa el humano o el harness. El control plane guarda el estado deseado. El nodo lo materializa en una microVM. La red local, si se pidió, es un camino **aparte** y, en este corte, solo un handshake.

```mermaid
flowchart TB
  subgraph user["Máquina de quien lanza el agente"]
    H["Harness estilo OpenCode"]
    CLI["asp session<br/>start · exec · stop"]
    IDP["IdP<br/>Keycloak en el lab"]
    LN["local-net opcional<br/>grant y heartbeat<br/>sin iface WireGuard"]
  end

  subgraph control["Control plane"]
    CP["API<br/>owner_sub · egress · idle · leases"]
  end

  subgraph node["Nodo"]
    NA["node-agent<br/>reconciler · TAP · nft"]
    CH["Cloud Hypervisor<br/>FakeVMM si dry-run"]
    subgraph vm["microVM"]
      PD["pod-daemon"]
      WK["workload del agente"]
      WK --> PD
    end
    VFS["virtiofsd<br/>tag workspace"]
    EG["Egress del nodo<br/>proxy · DNS sink · nft"]
  end

  H -->|"sustituye el shell del tool"| CLI
  IDP -.->|"JWT Bearer"| CLI
  CLI -->|"HTTPS"| CP
  LN -.->|"solo handshake"| CP
  CP -->|"mTLS, estado deseado"| NA
  NA --> CH
  CH --> vm
  NA -->|"si --workspace"| VFS
  VFS -.->|"mount en imagen nueva<br/>no en el rootfs de ncc1701d"| vm
  vm --> EG
  CP -.->|"local_net pending: blackhole deseado<br/>no dataplane"| NA
```

Diagrama de componentes ya versionado (cliente, control plane, nodo, guest, egress; no dibuja la sesión ni local-net): [`docs/diagram.svg`](docs/diagram.svg). Fuente editable: [`docs/diagram.mmd`](docs/diagram.mmd). Regenerar el SVG: `./scripts/gen-diagram.sh`. Narrativa de fronteras de confianza: [`docs/architecture.md`](docs/architecture.md).

Puertos vsock dentro del nodo (no cruzan la WAN y no terminan el túnel local): **26500** exec host→guest, **26501** SSH agent guest→host, **26502** OIDC guest→host. Notas: [`scripts/guest-vsock-notes.md`](scripts/guest-vsock-notes.md).

## Mapa de la documentación

El resto del detalle vive fuera de esta página. Esta tabla solo abre la puerta.

| Si buscas… | Empieza aquí |
|---|---|
| Threat model, capas, flujos | [`docs/architecture.md`](docs/architecture.md) |
| Decisiones (sin repetirlas aquí) | [`docs/adr/`](docs/adr/) — índice debajo |
| Fases hechas y gaps | [`docs/roadmap.md`](docs/roadmap.md) |
| Smoke sin KVM | [`docs/mvp-smoke.md`](docs/mvp-smoke.md) |
| Cloud Hypervisor en bare metal | [`docs/bare-metal-ch.md`](docs/bare-metal-ch.md) |
| Sesión y wrapper del harness | [`docs/ops-asp-session.md`](docs/ops-asp-session.md) · [por qué](docs/why-agent-sessions.md) |
| Un solo comando (`asp sandbox run`) | [`docs/ops-asp-agent-runner.md`](docs/ops-asp-agent-runner.md) |
| Keycloak de lab | [`docs/ops-idp-keycloak-lab.md`](docs/ops-idp-keycloak-lab.md) |
| `--local-net` | [`docs/ops-local-net.md`](docs/ops-local-net.md) · [por qué](docs/why-on-demand-local-net.md) |
| virtiofs y PTY del exec | [`docs/why-virtiofs-pty.md`](docs/why-virtiofs-pty.md) |
| Notas `why-*` (2d, 2e, CLI, identidad, flujos) | [`docs/`](docs/) |

### ADRs

Índice. El texto normativo está en cada archivo.

| ADR | Tema |
|---|---|
| [0001](docs/adr/0001-vmm-choice.md) | VMM: Cloud Hypervisor |
| [0002](docs/adr/0002-networking.md) | Red y egress del nodo |
| [0003](docs/adr/0003-identity.md) | SSH agent e OIDC fuera del guest |
| [0004](docs/adr/0004-k8s-scope.md) | Kubernetes solo para desplegar el control plane |
| [0005](docs/adr/0005-fase-2d-hardening.md) | Hardening 2d |
| [0006](docs/adr/0006-fase-2e-nft-ssh-guest.md) | nft y SSH en el guest |
| [0007](docs/adr/0007-multi-user-identity.md) | Identidad multi-usuario / IdP |
| [0008](docs/adr/0008-network-flow-attribution.md) | Flujos de red → `owner_sub` (evaluación, no implementada) |
| [0009](docs/adr/0009-agent-sessions.md) | La sesión es el uso primario del aislamiento |
| [0010](docs/adr/0010-on-demand-local-net.md) | Red local: túnel completo, opt-in; corte mínimo sin WireGuard de kernel |

## Límites que el código sí tiene

| Tema | Realidad |
|---|---|
| Dry-run | `FakeVMM`. No es KVM. |
| nft | `soft` tolera la falta de root. `enforce` exige privilegios. CI no demuestra bypass-proof. |
| Attest | Firma software (`ASP_ATTEST_KEY`). No es TPM/SEV. |
| Leases | TTL en el control plane y `FenceProvider` stub. No es STONITH BMC. |
| Idle | Apagado por defecto. No barre el fichero local de la sesión. |
| local-net | Handshake y plan de blackhole. Sin dispositivo WireGuard ni NAT. |
| virtiofs | El nodo pone el dispositivo si hay binario. El auto-mount está en el **build** de la imagen. El rootfs de ncc1701d no se ha reconstruido. |
| OpenCode | No hay plugin. Hay un wrapper de ejemplo. |
| Atribución de flujos | ADR-0008, no implementada. |
| K8s | Opcional para el API. Los sandboxes no son Pods. |

## Arranque mínimo (dry-run)

Go 1.22+ y Rust/Cargo. Docker solo si quieres Postgres o construir el rootfs. No hace falta KVM. Procedimiento y fallos: [`docs/mvp-smoke.md`](docs/mvp-smoke.md). Host con KVM: [`docs/bare-metal-ch.md`](docs/bare-metal-ch.md).

```bash
make test
make smoke       # enroll / identity / reconcile
make asp         # → build/asp
make smoke-asp   # CLI e2e dry-run (sandbox run, no la sesión)
```

Tres procesos, en seco:

```bash
export ASP_NODE_BOOTSTRAP_TOKEN=dev-node-bootstrap
(cd control-plane && go run ./cmd/api)
(cd pod-daemon && cargo run -- --listen unix --unix-socket /tmp/pod-daemon.sock)
(cd node-agent && go run ./cmd/node-agent \
  --control-plane-url=http://127.0.0.1:8080 --node-id=dev-node \
  --dry-run --enroll --bootstrap-token=dev-node-bootstrap \
  --cert-dir=/tmp/asp-node-certs --agent-listen=127.0.0.1:9100 \
  --pod-daemon-sock=/tmp/pod-daemon.sock --reconcile \
  --host-vsock --host-vsock-dir=/tmp/asp-hv)
```

Postgres opcional: `docker compose up -d postgres` y `DATABASE_URL=postgres://asp:asp@127.0.0.1:5432/asp?sslmode=disable`. Sin esa variable el control plane usa memoria y pierde el estado al salir.

## Mapa del árbol

| Ruta | Responsabilidad |
|---|---|
| [`control-plane/`](control-plane/) | API, tenancy, PKI, OIDC, attest, leases, local-net |
| [`node-agent/`](node-agent/) | VMM, reconciler, egress/nft, vsock, TAP, virtiofsd |
| [`pod-daemon/`](pod-daemon/) | Exec en el guest |
| [`images/guest/`](images/guest/) | Rootfs Debian, unidad virtiofs, helpers SSH |
| [`cli/`](cli/) | `asp` |
| [`docs/`](docs/) | Arquitectura, ADRs, ops |
| [`scripts/`](scripts/) | Smokes, pack, nft, rootfs, diagrama |
