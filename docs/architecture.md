# Arquitectura — Agent Sandbox Platform

## Resumen

La plataforma separa **orquestación**, **ejecución privilegiada en el nodo** y **carga invitada**. El camino de control es:

```text
cliente (IDE / asp / automatización)
  → control-plane (API multi-tenant, estado, OIDC, attest, fence)
    → node-agent (reconciler, VMM, TAP, nft, proxies, host-vsock)
      → microVM (Cloud Hypervisor | FakeVMM)
        → pod-daemon (exec/files por vsock)
```

La frontera de seguridad **primaria** es la microVM. Los contenedores dentro del guest, si se incorporan, son comodidad de empaquetado y **no** sustituyen esa frontera.

> Proyecto independiente FOSS, inspirado en sandboxes de agentes de IDE. No afiliado a Cursor, Anysphere ni anyrun.

## Diagrama

![Arquitectura](diagram.svg)

Fuente Mermaid editable: [`diagram.mmd`](diagram.mmd). Regenerar SVG: `./scripts/gen-diagram.sh`.

## Threat model (sketch)

| Activo | Amenaza | Mitigación en ASP |
|---|---|---|
| Secretos del operador (SSH keys, OIDC) | Exfiltración desde guest | Claves host-held; OIDC corto; guest no elige claims |
| Aislamiento entre sandboxes / tenants | Escape / lateral movement | microVM + TAP por sandbox; deny-default egress |
| Integridad del nodo | Node-agent o cert comprometido | mTLS enroll; rotate/revoke cert; identidad del cert atada a cada ruta de nodo (ADR-0011); attest software |
| Canal CP → nodo | `exec` sin autenticar en la red | Loopback por defecto; en otro host, mTLS con `ServerName` = node id y solo el cert del CP (ADR-0011) |
| Egress corporativo | Guest bypasea proxy | Forward proxy + DNS sink + nft `asp_egress` (enforce en bare-metal) |
| Atribución de flujos a humano | Tras NAT no se sabe qué empleado dialó | Futuro: ADR-0008 (proxy + IP/mark → `owner_sub`); no implementado |
| Split-brain multi-nodo | Dos nodos creen poseer el mismo sandbox | Solo el nodo asignado reclama; transiciones validadas; monitor de nodos + FenceProvider; el nodo para sus VMs si el CP rechaza su lease (≠ STONITH BMC real) ([ADR-0011](adr/0011-multi-node.md)) |
| Plano de control | API anónima / path mal cableado | API keys; `ASP_MTLS_STRICT`; rutas públicas mínimas |

**No cubierto (honestidad):** TPM/SEV hardware attestation; bypass-proof nft medido en CI sin KVM; Windows guests; “Kubernetes NetworkPolicy como frontera”.

## Trust boundaries

```text
┌─ Cliente ─────────────────────────────────────────────┐
│  Confianza: API key Bearer. No habla con VMM.         │
└───────────────────────────┬───────────────────────────┘
                            │ HTTPS
┌─ Control plane ───────────┴───────────────────────────┐
│  Autoridad de tenancy, cuotas, JWKS, attest verify,   │
│  leases/fence. Store Postgres (o MemoryStore lab).    │
└───────────────────────────┬───────────────────────────┘
                            │ mTLS en los dos sentidos
                            │ (cert del nodo ↔ cert del CP)
┌─ Node agent (privileged host) ────────────────────────┐
│  Único invocado del VMM. Proxies egress. Host-vsock.  │
│  SoftFail sin CAP_NET_ADMIN en lab.                   │
└───────────────────────────┬───────────────────────────┘
                            │ virtio / vsock (no confiar en guest)
┌─ Guest (untrusted) ───────────────────────────────────┐
│  pod-daemon + workload. Sin NET_ADMIN. Sin secretos   │
│  largos. Solo aud + requests de firma.                │
└───────────────────────────────────────────────────────┘
```

## Capas

### 1. Cliente

CLI `asp`, IDE o servicio automatizado. Se autentica ante el CP. El uso primario de un agente largo es una **sesión** (un sandbox, muchos `exec`); create→exec→destroy es la primitiva de un comando ([ADR-0009](adr/0009-agent-sessions.md)). Nunca recibe credenciales de infraestructura permanentes ni sockets del hipervisor.

### 2. Control plane (Go)

Paquete `control-plane/`: API HTTP/TLS, store, PKI de enrollment, OIDC, attestation verify, planificador, monitor de nodos y fencing.

Responsabilidades:

- Estado deseado de sandboxes y journal `sandbox_events`.
- Inventario de nodos (enroll, register, heartbeat, rotate/revoke).
- Planificador por capacidad: elige el nodo al crear (filtros de vida, cordon, perfil, CPU/memoria/huecos; `spread` o `binpack`) y rechaza con 503 si nada cabe ([ADR-0011](adr/0011-multi-node.md), [`ops-multi-node.md`](ops-multi-node.md)).
- Work queue: `GET /v1/nodes/{id}/work` (solo las sandboxes de ese nodo) + claim/status/renew-lease.
- Proxy de exec hacia `agent_endpoint` del nodo (`POST /v1/sandboxes/{id}/exec`): HTTP en loopback, o HTTPS con mTLS hacia un nodo en otro host ([ADR-0011](adr/0011-multi-node.md)).
- Egress policies por tenant; JWKS público.

Puede vivir en Kubernetes **solo como Deployment del API** (ADR-0004); no ejecuta workloads de usuario.

### 3. Node agent (Go)

Proceso privilegiado por nodo (`node-agent/`). Prepara TAP, aplica nft (soft|enforce), arranca FakeVMM o Cloud Hypervisor, registra dialers vsock, expone:

| Puerto / socket | Rol |
|---|---|
| `--agent-listen` (default `127.0.0.1:9100`) | Exec proxy y rutas de operador (`/v1/internal/exec`, `approve`, `egress-check`). HTTP sin autenticar: solo loopback salvo `--insecure-agent-listen` |
| `--agent-tls-listen` (p. ej. `0.0.0.0:9443`) | `exec` para un plano de control en otro host. mTLS con el cert del nodo; solo acepta el cert del CP ([ADR-0011](adr/0011-multi-node.md)) |
| host→guest **26500** | pod-daemon HTTP (hybrid vsock CONNECT) |
| guest→host **26501** | SSH agent pump (CH: `{vsock}_{26501}` hybrid UDS) |
| guest→host **26502** | OIDC identity HTTP (CH: `{vsock}_{26502}`) |
| `--egress-proxy-listen` | Forward proxy allowlist |
| `--egress-dns-sink` | DNS NXDOMAIN non-allowlisted |

> Guest→host under Cloud Hypervisor: see [`why-ch-hybrid-guest-host.md`](why-ch-hybrid-guest-host.md). AF_VSOCK Listen alone is insufficient for CH hybrid muxer.

### 4. microVM

Debian mínimo (`images/guest/`). Sin socket del runtime del host, sin `NET_ADMIN`, sin secretos persistentes. NIC virtio restringida + vsock. Cada VM arranca una copia privada del rootfs (`--disk-dir/rootfs-{id}.img`, reflink o copia sparse) que se borra al parar; la imagen base no se escribe nunca.

### 5. pod-daemon (Rust)

PID de servicio en el guest. Escucha vsock (prod) o unix (dry-run). Expone `Exec` (y contratos de files/metrics según evolución). Materializa paths locales; **no** decide tenancy.

## Ciclo de vida (secuencia)

Estados (`store.SandboxState`):

```text
requested → starting → running ⇄ paused → stopping → stopped
                ↘ failed (terminal para el intento)
```

Con `ASP_AUTO_PROVISION=0` (default prod/bare-metal):

1. Cliente `POST /v1/sandboxes` → `requested` (+ evento).
2. Node-agent `--reconcile` hace `GET …/work`.
3. `POST …/claim` atómico → `starting` + lease TTL (~30s).
4. TAP (si `--tap-auto`) → VMM `Start` → dialer vsock → `POST …/status` `running`.
5. Attestation opcional: nodo firma `BootStatement`, CP `POST …/attest`.
6. Heartbeat + `renew-lease` mientras corre.
7. `DELETE /v1/sandboxes/{id}` → `stopping` → VMM Stop + cleanup TAP/sockets → `stopped`.

Reintentos idempotentes; el reconciler **reconcilia** estado real vs deseado en lugar de asumir RPC perfectos. `state_version` evita lost updates.

## Flujos de identidad

### SSH agent (host-held)

```text
guest herramienta ssh
  → SSH_AUTH_SOCK=/run/agent-sandbox/ssh-agent.sock
    → vsock-ssh-agent-proxy → AF_VSOCK CID 2:26501
      → node-agent host-vsock / bridge
        → [confirm gate; multi-user default-on] → HostSock por sandbox (template) / FakeAgent / legacy SSH_AUTH_SOCK
```

Nunca se copia la clave privada. Confirm: ADR-0005. Auto mount: ADR-0006.

### OIDC

```text
guest POST /v1/tokens/oidc {"aud":"https://api.ejemplo"}   # user_sub/act ignorados si vienen
  → vsock identity 26502 (CH: {vsock}_26502 de su sandbox)
    → node-agent identity proxy (sandbox = el de la conexión; X-ASP-Sandbox-ID de otra → 403)
      → CP POST /v1/internal/oidc/token
        → JWT corto + claims server-side; JWKS en /oidc/jwks.json
```

Rotación: `ASP_OIDC_KEY` + `ASP_OIDC_KEY_PREV`. Attest claim opcional `x_asp_attestation`.

> **ADR-0007 fases 1–5 hechas** (schema + JWT IdP + RBAC + SSH scoped + workload `user_sub`/`act`). Gaps ops: IdP real, `tenant_memberships`, socks SSH. Ver [ADR-0007](adr/0007-multi-user-identity.md), [`why-multi-user-identity.md`](why-multi-user-identity.md).

## Red, egress y nft

Ver ADR-0002 y ADR-0006. Resumen operativo:

1. TAP `asp-{shortid}` con su propia /30 de `--guest-subnet` (TAP `.1`, guest `.2` vía `ip=` en la cmdline). El proxy identifica el sandbox por esa IP de origen.
2. NAT MASQUERADE ops (`asp_nat`) — conectividad mínima hacia el proxy.
3. Guest `HTTP_PROXY=http://<gateway>:8888`.
4. `--nft-egress-redirect --nft-egress-mode=enforce` fuerza HTTP(S)+DNS por proxy/sink y descarta el resto: otros puertos, guest→guest, guest→servicios del host y orígenes falsificados.
5. En CI: `--nft-egress-mode=soft` (SoftFail sin root).

**Atribución de flujos → humano (futuro):** hoy el proxy puede ver `X-ASP-Sandbox-ID` (forgeable) y no propaga `owner_sub`. Diseño en evaluación — [ADR-0008](adr/0008-network-flow-attribution.md), [`why-network-flow-attribution.md`](why-network-flow-attribution.md): lookup host-side (IP/TAP o `ct mark`) → `sandbox_id` → `owner_sub`; forced egress corporativo con identidad inyectada en el host. **No implementado.**

**LAN del usuario (corte mínimo, sin WireGuard de kernel):** sin opt-in, el sandbox no llega a prefijos tipo `192.168.1.0/24` (salen por el egress del nodo y mueren ahí). [ADR-0010](adr/0010-on-demand-local-net.md) describe un túnel saliente, solo con `local_net` explícito: cuando está on, la ruta por defecto de **esa** sesión (`0.0.0.0/0` y `::/0` si existe, DNS incluido) va por el agente local, que la ve y la hace NAT. No hay lista de CIDR en v1. Si el agente cae, el egress se hunde; no vuelve en silencio al proxy del nodo. No abre el router de casa ni la ruta por defecto del nodo. Ver [`why-on-demand-local-net.md`](why-on-demand-local-net.md).

## Modelo de datos (control plane)

Tablas / entidades principales (migraciones `001`–`007`):

| Entidad | Campos clave |
|---|---|
| `sandboxes` | tenant_id, state, node_id, vmm_profile, resources, state_version, node_lease_until, **owner_sub**, **owner_email** (007 / ADR-0007 fase 1) |
| `sandbox_events` | journal append-only; **actor_sub** (007) |
| `nodes` | endpoint, agent_endpoint, capacity (cpu, mem, max_sandboxes), cordoned, accepts_work, local_net_dial, cert_fingerprint/serial, fence_*, revoked_at |
| `node_cert_revocations` | fingerprints revocados (006) |
| `api_keys` | sha256 del secreto; Bearer |
| `tenant_egress_rules` | host_pattern, port, enabled (003) |
| attestation evidence | BootStatement firmado (005) |

Stores: `PostgresStore` si `DATABASE_URL`; si no, `MemoryStore` (lab; se pierde al reiniciar).

## Fallos y recuperación

| Escenario | Comportamiento |
|---|---|
| Nodo sin señales > `ASP_NODE_STALE_AFTER` (90 s) | Sale del reparto y pasa a `offline` |
| Nodo sin señales > `ASP_NODE_FAILOVER_AFTER` (5 min) o revocado | `FenceProvider` (si tiene sandboxes, una vez por caída); sus sandboxes → `failed` (`node_lost`), `stopping` → `stopped`. No se mueven |
| El nodo vuelve tras una partición | Vuelve a `ready`; sus renovaciones de lease reciben 409 y para esas VMs |
| Node-agent reinicia | `agent_instance_id` nuevo: sus `running`/`paused` → `failed` (`node_agent_restarted`); `requested`/`starting` las arranca el proceso nuevo |
| Reinicio del plano de control | Gracia: el silencio se cuenta desde el arranque del monitor |
| SoftFail TAP/nft | Log warning; CH puede fallar al abrir TAP; **no** hay frontera de red real |
| Attest/JWKS caído | Mint OIDC falla cerrado |
| FakeVMM dry-run | Todo el plano de control funciona; **cero** aislamiento KVM |

**Lease software ≠ STONITH.** BMC out-of-band real sigue siendo ops (documentado en bare-metal §8b–8c). Detalle: [`ops-multi-node.md`](ops-multi-node.md).

## Dry-run vs bare-metal

| | Dry-run (`--dry-run`) | Bare-metal |
|---|---|---|
| VMM | `FakeVMM` | Cloud Hypervisor spawn por sandbox |
| pod-daemon | unix `--pod-daemon-sock` | hybrid vsock CONNECT 26500 |
| host-vsock | `--host-vsock-dir` unix | AF_VSOCK real |
| TAP / nft | SoftFail típico | `--tap-auto` + nft `enforce` |
| Guía | [`mvp-smoke.md`](mvp-smoke.md) | [`bare-metal-ch.md`](bare-metal-ch.md) |

## Cómo usan la plataforma los agentes

1. Ops levanta CP + node-agent (+ pod-daemon en dry-run).
2. Un agente largo abre **una sesión** y engancha el shell a `exec` (no una VM por tool). Dirección: [ADR-0009](adr/0009-agent-sessions.md) · [`why-agent-sessions.md`](why-agent-sessions.md) · [`ops-asp-session.md`](ops-asp-session.md).

```bash
make asp
./build/asp session start --node-id=dev-node
./build/asp session exec --cmd 'echo hello'
./build/asp session stop
```

3. `asp sandbox run` (create → wait `running` → exec → destroy) es la primitiva de CI/un comando, no la integración del bucle. Auth Bearer: [`ops-asp-agent-runner.md`](ops-asp-agent-runner.md).
4. El exec (`POST /v1/sandboxes/{id}/exec`) es el dataplane dentro de la sesión: NDJSON si `?stream=1`, JSON acumulado si no. No es un PTY. `--workspace` queda en el spec; en KVM el host no entra al guest.
5. Detalle CLI: [`why-cli-asp.md`](why-cli-asp.md).

Los agentes **no** necesitan hablar con CH ni con nft; solo con el control plane.

## Límites operativos (reafirmados)

- Kubernetes opcional solo para el CP; sandboxes ≠ Pods (ADR-0004).
- Attestation software ≠ TPM/SEV.
- SoftFail nft/TAP ≠ enforce.
- Sin entrada directa desde Internet a microVMs.
- Sin claves privadas ni refresh tokens en la imagen guest.

## Evolución

Orden de fases, gaps y criterios: [`roadmap.md`](roadmap.md). Decisiones normativas: [`adr/`](adr/). Atribución de red a `owner_sub` (evaluación): [ADR-0008](adr/0008-network-flow-attribution.md).
