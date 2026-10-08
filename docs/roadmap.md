# Roadmap

Historia de fases del MVP hasta el estado **solution complete** (2a) y endurecimiento post-MVP (2b–2f). Cada fase hecha incluye qué entregó, por qué importaba y gaps residuales.

Tras 2f: **readiness corporativa** (IdP humano, multi-user) y Fases 3–4 (multi-nodo / escala). La parte multi-nodo de la Fase 3 (3m.*) está hecha: [ADR-0011](adr/0011-multi-node.md). Ver § «Readiness corporativa» y [ADR-0007](adr/0007-multi-user-identity.md). Atribución de flujos de red a `owner_sub`: [ADR-0008](adr/0008-network-flow-attribution.md) (evaluación, no implementada). Red local bajo demanda (túnel completo de la sesión, default off, sin CIDR en v1): [ADR-0010](adr/0010-on-demand-local-net.md) (comandos `ip`/`wg` por sesión; sin lab de paquetes).

**Dirección de producto (agentes):** el aislamiento se usa en una **sesión** larga (un sandbox: `owner_sub`, workspace del guest, egress, idle), no con create→exec→destroy por comando de shell. [ADR-0009](adr/0009-agent-sessions.md). `asp sandbox run` sigue siendo la primitiva de CI/ops.

## Fase 0 — Esqueleto

- Contratos de componentes, ADRs iniciales y esquema SQL.
- API, node-agent y pod-daemon compilables con comportamiento placeholder.

**Qué entregó / por qué importaba:** fijó el mapa mental (CP → node-agent → microVM → pod-daemon) y las cuatro ADRs fundacionales (VMM, red, identidad, K8s) antes de acumular código disposable.

**Gaps residuales:** sin dataplane real; threat model solo en prosa.

- **Hecho.**

## Fase 1a — MVP slice

- Control plane: store in-memory; Create/Get/List sandboxes; stub provisioner (`requested` → `running` + `node_id=local-dev`).
- Registro de nodos; node-agent con cliente CH + `FakeVMM` / `--dry-run`.
- Egress: tipo `Allowlist` + `Check(host)`.
- Smoke: [`mvp-smoke.md`](mvp-smoke.md).

**Qué entregó / por qué importaba:** primer camino HTTP end-to-end en lab sin KVM ni Postgres; demostró que FakeVMM desbloquea CI.

**Gaps residuales:** sin Postgres real ni CH en CI; provisioner stub mentía el estado `running`.

- **Hecho.**

## Fase 1b — Postgres control-plane

- `PostgresStore` + migraciones embebidas; journal `sandbox_events` / `node_events`; API keys opcionales; `docker-compose.yml`.

**Qué entregó / por qué importaba:** persistencia y audit trail; MemoryStore sigue siendo default offline para tests rápidos.

**Gaps residuales:** aún sin enrollment mTLS ni exec real.

- **Hecho.**

## Fase 1c — mTLS enrollment + exec dataplane

- PKI de lab; `POST /v1/nodes/enroll`; TLS opcional; heartbeat.
- pod-daemon HTTP JSON; node-agent exec proxy; CP reenvía exec.
- Smoke: [`scripts/smoke-enroll-exec.sh`](../scripts/smoke-enroll-exec.sh).

**Qué entregó / por qué importaba:** identidad de nodo + primer `echo hello` vía CP → agent → pod-daemon (unix dry-run).

**Gaps residuales:** sin allowlist de egress ni OIDC/SSH; sin reconciler (stub auto-provision).

- **Hecho.**

## Fase 1d — Egress allowlist + OIDC/SSH bridge

- Tenant egress API; OIDC discovery/JWKS/mint; identity proxy; SSH agent bridge.
- Smoke: [`scripts/smoke-identity-egress.sh`](../scripts/smoke-identity-egress.sh).

**Qué entregó / por qué importaba:** cerró el story de “secrets fuera del guest” a nivel de protocolo (aún sin host-vsock productivo ni nft).

**Gaps residuales:** HTTP_PROXY voluntario; SSH mount guest manual; sin reconciler.

- **Hecho.**

## Fase 1e — Reconciler real

- `ASP_AUTO_PROVISION` default false; claim/work/status/destroy; `--reconcile`.
- Smoke: [`scripts/smoke-reconcile.sh`](../scripts/smoke-reconcile.sh).

**Qué entregó / por qué importaba:** el CP deja de mentir `running`; el node-agent es la fuente de verdad del VMM.

**Gaps residuales:** FakeVMM o CH shared; sin multi-socket ni vsock hybrid.

- **Hecho.**

## Fase 1f — Ops guide bare-metal CH

- Guía: [instalar un nodo](how-to/install-node.md).

**Qué entregó / por qué importaba:** documento el camino KVM real para quien sí tiene `/dev/kvm`; separó dry-run de producción.

**Gaps residuales:** la guía creció con 2a–2e; hay que mantenerla alineada al código (TAP auto, nft, host-vsock).

- **Hecho.**

## Fase 1g — Multi-socket CH spawn por sandbox

- Spawn `cloud-hypervisor --api-socket /run/asp/ch-{id}.sock` por Start; Stop delete+kill+rm.

**Qué entregó / por qué importaba:** aislamiento de fallos por sandbox; deja atrás el CH shared de debug como default.

**Gaps residuales:** exec aún podía caer a unix sock; vsock productivo en 1h.

- **Hecho.**

## Fase 1h — Vsock productivo guest↔host exec

- CID único ≥ 3; HybridVsockDialer CONNECT **26500**; guest `--listen vsock`.

**Qué entregó / por qué importaba:** dataplane exec sin depender de montar el unix sock del pod-daemon en el host.

**Gaps residuales:** identity/SSH guest→host aún no en AF_VSOCK (llega en 2a).

- **Hecho.**

## Fase 2a — Solution MVP complete

Cierre shippable del MVP:

1. **Identity + SSH guest→host vsock** — `HostVsockService` (`--host-vsock`): **26501** SSH / **26502** identity; lab `--host-vsock-dir`. En CH productive: reconciler **`AttachSandbox`** → UDS `{vsock}_{port}` (ver [vsock híbrido](concepts/node-runtime.md#del-guest-al-host-identidad-y-agente-ssh---host-vsock)); AF_VSOCK Listen solo no basta.
2. **TAP auto** — `--tap-auto` SoftFail sin CAP_NET_ADMIN.
3. **Guest image** — Dockerfile + systemd/OpenRC; `scripts/build-guest-rootfs.sh`.
4. **Pack** — `scripts/pack-release.sh` + `Makefile`.
5. **Docs** — roadmap MVP + bare-metal e2e.

**Qué entregó / por qué importaba:** un tarball + guía con los tres puertos vsock documentados; demo coherente dry-run↔bare-metal.

### Known limits (honestos)

| Límite | Detalle |
|---|---|
| Nested virt | Solo lab; densidad/latencia peores |
| No Windows guests | Solo Linux microVMs |
| CH version pin | Ops fija release validada |
| Virtiofs SSH sock | Path auto = vsock proxy guest (**2e**); virtiofs = ops manual |
| CH hybrid guest→host | Requiere `{muxer}_{26501|26502}`; sin Attach → RST (fix post-2a) |
| Packet intercept | Proxy en path; nft redirect **2e** (HTTP+DNS, soft\|enforce) |
| Boot attestation | Software-signed (**2c**); TPM/SEV = plug-in futuro |
| STONITH | Leases soft + FenceProvider stub; BMC real = ops |

**Gaps residuales al cerrar 2a:** forward proxy en wire, leases multi-nodo, attest, cert rotation, nft completo, CLI ergonómica — cubiertos en 2b–2f.

**Criterio de salida MVP:** `go test` + `cargo test` + smokes dry-run; tarball; bare-metal documentado.

## Fase 2b — Post-MVP slices

1. **Egress HTTP forward proxy** — `--egress-proxy-listen`; DNS sink opcional; guest `HTTP_PROXY` → TAP host.
2. **Multi-node leases** — migración `004`; claim/renew TTL 30s; **sin STONITH**. (Retirados en 2026-10: el conjunto `assigned` del sondeo de `/work` los sustituye.)
3. **OIDC key rotation** — `ASP_OIDC_KEY` + `ASP_OIDC_KEY_PREV`.

**Qué entregó / por qué importaba:** el proxy dejó de ser solo “check API”; leases evitan double-claim ingenuo entre nodos.

**Gaps residuales:** HTTP_PROXY sigue voluntario sin nft; fencing real ausente; attest ausente.

- **Hecho.**

## Fase 2c — Seguridad endurecida

1. **Remote attestation (MVP práctico)** — `BootStatement` firmado ECDSA (`ASP_ATTEST_KEY`); APIs attest/verify; claim OIDC opcional. Interfaz `Attestor` para TPM/SEV futuro.
2. **FenceProvider** — Noop, HTTPWebhook, Redfish/IPMI stubs; al reclaim de `running` con lease expirado. Desde 3m.3 ya no hay reclaim entre nodos: lo invoca el monitor de nodos antes de fallar las sandboxes de un nodo perdido.
3. **Proxy hardening** — token bucket; body limit; deny non-HTTP schemes; audit JSON; MITM opcional off-by-default.

**Qué entregó / por qué importaba:** evidencia de boot medible en software; gancho de fence sin fingir BMC; proxy listo para abuso.

**Gaps residuales (→ 2d):** rotación/revocación certs; mTLS estricto; SSH confirm; nft sketch.

- **Hecho.**

## Fase 2d — Hardening operativo

1. **Node cert rotation & revocation** — `rotate-cert` / `revoke`; migración `006`.
2. **Strict mTLS** — `ASP_MTLS_STRICT=1` + `ASP_ENROLL_LISTEN`.
3. **SSH agent confirmation gate** — `--ssh-agent-confirm` + approve one-shot.
4. **nftables anti-bypass sketch** — script + SoftFail (**completado en 2e**).

**Qué entregó / por qué importaba:** ciclo de vida de identidad de nodo; listener TLS sin anónimos; firmas SSH no silenciosas; primer clavo anti-bypass.

**Gaps residuales:** nft incompleto (DNS/puertos); SSH guest mount manual → **2e**.

Docs: [`adr/0005-fase-2d-hardening.md`](adr/0005-fase-2d-hardening.md).

- **Hecho.**

## Fase 2e — nft completo + SSH guest auto

1. **nftables anti-bypass completo** — HTTP(S) puertos configurables + DNS redirect/drop; `--nft-egress-mode=soft|enforce`.
2. **SSH agent mount automatizado** — `vsock-ssh-agent-proxy` + `ssh-agent-vsock.service` en la imagen del guest.

**Qué entregó / por qué importaba:** frontera de egress host-side creíble en bare-metal; `SSH_AUTH_SOCK` out-of-the-box en la imagen.

**Límites honestos 2e:** sin TAP/KVM en CI no hay bypass-proof hardware; Enforce = ops bare-metal; virtiofs SSH sigue manual.

ADR: [`adr/0006-fase-2e-nft-ssh-guest.md`](adr/0006-fase-2e-nft-ssh-guest.md).

- **Hecho.**

## Fase 2f — Agent/ops CLI `asp`

1. **CLI demo** — `cli/` + binario `asp` (`make asp`): `sandbox create|get|list|exec|delete|run`.

**Qué entregó / por qué importaba:** one-liner create→wait→exec→destroy sin reimplementar poll en cada script; exit code del guest al shell.

**Gaps residuales:** no es SDK multi-lenguaje ni TUI. `sandbox run` sigue en JSON acumulado; `asp session exec` streamea NDJSON y puede pedir PTY/stdin (no es SSH; ver límites en ops).

Docs: [README del CLI](../cli/README.md).

- **Hecho.**

### Sesión de agente (`asp session`) — post-2f, dirección primaria

[ADR-0009](adr/0009-agent-sessions.md) fija esto como **la** forma en que un agente usa el aislamiento (sesión larga; el exec es el dataplane, no la integración). El one-shot de la fase 2f no es el producto.

Contrato para harnesses (OpenCode y similares) que enganchan el shell **dentro** del sandbox sin create/destroy por tool:

- `asp session start|exec|status|stop --name` (default `default`)
- Estado local `~/.cache/asp/sessions/<nombre>.json` (0600): id + URL del CP, sin token. `ASP_SESSION_DIR`. `--session-file` sigue como override
- `exec` reutiliza `POST /v1/sandboxes/{id}/exec`. Por defecto NDJSON (`?stream=1`); `--buffered` conserva el JSON acumulado de los smokes
- `--workspace` guarda `workspace_host_path`. El nodo arranca `virtiofsd` y CH recibe `fs` tag `workspace`. La imagen nueva auto-monta `/workspace` (sale 0 si no hay tag). Un rootfs viejo monta a mano. Sin binario el start falla. Sin path no hay `fs`

**Qué entregó / por qué importaba:** el one-shot queda para CI; la sesión es el camino del agente y evita pagar el boot en cada tool. Narrativa: [ADR-0009](adr/0009-agent-sessions.md).

**Límites honestos:** no es plugin de OpenCode (hay wrapper de ejemplo); el auto-mount de virtiofs está en la imagen nueva, no en un rootfs ya desplegado; el PTY no es un terminal completo (sin SIGWINCH, stderr mezclado, timeout del pod-daemon); sin GC si se olvida `stop` y el reaper está apagado; el idle sigue siendo global (el exec terminado y el stdin proxyado refrescan el reloj).

Docs: [`ops-asp-session.md`](ops-asp-session.md).

- **Hecho** (CLI + tests `httptest`; no exige un host KVM).

## Readiness corporativa — Identidad multi-usuario (diseño)

Gap post-2f (cerrado en código 3u.1–3u.5): faltaba sujeto humano, RBAC fino y `user_sub` en tokens de workload. **Implementado** en CP/node-agent; queda **ops**: JWKS IdP corporativo, memberships, socks SSH por usuario. Sin template, el bridge SSH global sigue siendo inseguro multi-user.

**Decisión de diseño:** [ADR-0007](adr/0007-multi-user-identity.md).

Rollout:

| Subfase | Entrega | Estado |
|---|---|---|
| **3u.1** | Schema `owner_sub` / `owner_email` + audit `actor_sub` | ✅ **Hecho** (migración `007_multi_user_identity.sql`; lab: vacío OK; header `X-ASP-Actor-Sub`) |
| **3u.2** | Validación JWT IdP (Entra/Okta/OIDC) en API del CP | ✅ **Hecho** (`internal/authn/idp`; `ASP_IDP_*`; lab default off) |
| **3u.3** | RBAC admin / operator / viewer desde claims IdP | ✅ **Hecho** (`ASP_IDP_ROLE_*`; list tenant-wide; destroy operator=propios) |
| **3u.4** | SSH: confirm default-on + registry/template por `owner_sub` (opción A MVP) | ✅ **Hecho** (`ASP_SSH_AGENT_SOCK_TEMPLATE`; upstream por sandbox; approve con `sandbox_id` y `actor_sub`; no spawner de agents) |
| **3u.5** | Mint OIDC workload con `user_sub` / `act` desde `owner_sub` | ✅ **Hecho** (`oidc.MintIdentity`; CP desde store; guest override ignorado; lab sin owner omite claims) |

**Criterio “corporate ready” (identidad):** create/exec/destroy atribuibles a humano; JWT de workload con cadena `user_sub`; SSH no compartido a ciegas entre usuarios del mismo nodo. API keys quedan como principals de servicio.

**Estado:** diseño aceptado (2026-10). Fases **3u.1–3u.5** implementadas (schema + JWT IdP + RBAC + SSH scoped MVP + workload `user_sub`/`act`). **Lab ops:** un Keycloak real (realm `asp`) cableado al plano de control del laboratorio — ver [`lab/idp-keycloak.md`](lab/idp-keycloak.md); para conectar el tuyo, [`how-to/idp.md`](how-to/idp.md). Gaps residuales: IdP corporativo Entra/Okta, tabla `tenant_memberships`, materializar socks SSH por usuario. SSH MVP = template/path ops, no daemon manager.

## Futuro — Atribución de flujos de red → `owner_sub` (evaluación)

Gap post-3u: el plano de control ya atribuye create/exec/OIDC a humano ([ADR-0007](adr/0007-multi-user-identity.md)), pero el **egress TCP/HTTPS** del guest no etiqueta de forma no forgeable quién es el dueño. El proxy solo ve `X-ASP-Sandbox-ID` opcional (guest-controlled); los TAP comparten CIDR host por defecto.

**Decisión de diseño (evaluación):** [ADR-0008](adr/0008-network-flow-attribution.md).

Camino preferente a evaluar: **egress proxy attribution** + IP/TAP (o `ct mark`) por sandbox; VLAN/VRF/eBPF como opciones pesadas. Guest-set marks/headers **no** son autoridad. Visibilidad corporativa = forced egress con identidad **host-injected**.

| Subfase (prevista) | Entrega | Estado |
|---|---|---|
| **3n.0** | ADR + why + enlaces roadmap/arquitectura | ✅ **Hecho** (docs only) |
| **3n.1** | Tabla host `srcKey → sandbox_id → owner_sub` + audit proxy enriquecido | ⏳ No implementado |
| **3n.2** | Direccionamiento TAP / nft `ct mark` por iif | ⏳ No implementado |
| **3n.3** | Forced egress → proxy corporativo (headers/mTLS host-side) | ⏳ No implementado |

**Estado:** propuesta ([ADR-0008](adr/0008-network-flow-attribution.md)), implementada en parte (2026-10): una /30 y un TAP por sandbox, y el proxy identifica la sandbox por la IP de origen, así que su audit lleva `sandbox_id`; falta `owner_sub` en esa línea, la identidad hacia un proxy corporativo y las marcas de nft. Criterio futuro: dos sandboxes, distinto `owner_sub`, mismo destino → audit distinto sin headers del guest.

## Red local bajo demanda

Gap distinto del egress público: un sandbox en un nodo remoto no puede hablar con la LAN del usuario. No debe poder hacerlo abriendo el router de casa, ni desviando `0.0.0.0/0` por el portátil **salvo** un opt-in explícito de esa sesión. Ese opt-in es el diseño de v1; el desvío por defecto, y el desvío silencioso, no.

**Decisión de diseño:** [ADR-0010](adr/0010-on-demand-local-net.md).

Default **apagado** (`local_net=false`). Solo `asp session start --local-net` (o `"local_net": true` en `POST /v1/sandboxes`) lo enciende. No hay prefijos ni puertos que declarar: v1 es todo o nada. El agente local abre un túnel saliente (WireGuard preferido, en un netns o TUN de usuario; el nodo no escucha en la LAN) y la **ruta por defecto de esa sesión** (`0.0.0.0/0` y `::/0` si existe, DNS incluido) sale por él. El portátil ve y hace NAT de todo ese tráfico, también el Internet público; la allowlist del nodo no aplica mientras el túnel está up. Si el agente se desconecta, el nodo blackholea esa default y **no** cae en silencio al proxy. El túnel muere con la sesión o el idle. Identidad: `owner_sub`. El guest no enciende ni apaga el flag. Excepciones estrechas, más adelante; no en v1.

| Subfase (prevista) | Entrega | Estado |
|---|---|---|
| **3l.0** | ADR y enlaces desde el roadmap, el README, 0008 y 0009 | ✅ **Hecho** (docs only) |
| **3l.1** | Campo `local_net` bool en el CP (default false; 400 si el body trae policy, CIDR o puertos) | ✅ Corte mínimo (memoria + migración 010) |
| **3l.2** | Túnel por sandbox: default de esa sesión por el túnel; blackhole si no está up; sin redirect al proxy ni al DNS sink; sin filtro de CIDR | ✅ Plan en el node-agent (memoria). No hay `ip`/`wg` en CI |
| **3l.3** | Agente local (`asp session local-net up/down`) y teardown al stop / idle / detach | ✅ `ip`+`wg` si hay tools y CAP_NET_ADMIN. Sin keepalive de sueño ni NAT demostrado |

**Estado:** comandos del dispositivo cableados (2026-10-03). Hace falta `wireguard-tools` y `CAP_NET_ADMIN`. No hay lab de paquetes. No es requisito de ADR-0008; el log, cuando exista, usa el mismo `owner_sub`. Ops: [`ops-local-net.md`](ops-local-net.md).

## Fase 3 — Multi-nodo y fiabilidad

Varios servidores de microVMs ([ADR-0011](adr/0011-multi-node.md), ops: [`ops-multi-node.md`](ops-multi-node.md)):

| Subfase | Qué | Estado |
|---|---|---|
| **3m.1** | Identidad de nodo atada al cert en cada ruta; mTLS plano de control → nodo (`--agent-tls-listen`); HTTP plano solo en loopback; secretos de fencing fuera de las respuestas | ✅ |
| **3m.2** | Colocación por capacidad al crear (`spread`/`binpack`, CPU 4×, 503 sin hueco); trabajo solo para el nodo asignado; cordon; capacidad real del host; `asp node` | ✅ |
| **3m.3** | Detección de nodos caídos (`offline` a 90 s, failover a 5 min), fallo de sus sandboxes, fencing, autodefensa del nodo, transiciones validadas, reinicio del agente | ✅ Sin lab KVM multi-servidor |
| **3m.4** | Tras reiniciar, el node-agent borra lo que dejó el proceso anterior (VMs, TAPs, túneles, sockets; los discos de las sandboxes paradas se conservan, [ADR-0012](adr/0012-retained-disks.md), y las VMs confinadas se adoptan, [ADR-0014](adr/0014-vms-outlive-the-agent.md)); unit systemd `asp-node-agent.service` con `KillMode=control-group` | ✅ |
| **3m.5** | Exec en streaming sin corte en el plano de control (timeouts por fase hacia el agente) | ✅ Plano de control, node-agent y pod-daemon: el stream dura lo que el comando, las cabeceras salen al arrancar y el guest mata el comando si el cliente se va |

Relacionado, fuera de 3m: el proxy de identidad toma la sandbox de la conexión vsock del guest y limita el tamaño de sus peticiones ([ADR-0003](adr/0003-identity.md) § 2).

- Métricas/SLOs; caos; fencing BMC de producción endurecido.
- Attestors hardware (TPM/SEV) vía `Attestor`.
- Observabilidad de revoke, SignRequest deny, nft enforce failures.
- Identidad multi-usuario / IdP: ver § readiness + ADR-0007 (puede avanzar en paralelo a 3u.*).

## Fase 4 — Escala y perfiles

- Pools precalentados; perfil Firecracker; workspace persistente cifrado.
- IPv6 / ampliación de nft; posibles NetworkPolicy-like rules por sandbox en el nodo.
- Atribución de flujos → `owner_sub` (si 3n.* se acepta tras evaluación): ver ADR-0008.
- Red local bajo demanda (si 3l.* se acepta): ver ADR-0010. No adelantar el túnel completo sin el flag, ni un fallback silencioso al proxy, ni inbound a la LAN.

## Fuera de alcance inicial

- Ejecutar sandboxes como Kubernetes Pods (ADR-0004).
- Exponer microVMs directamente a Internet.
- Guardar claves privadas o refresh tokens en imágenes guest.
- Compatibilidad con workloads que requieran `NET_ADMIN` en el guest.
- Prometer que SoftFail nft/TAP equivale a enforce en producción.
- Tratar UID Linux del guest como identidad humana (rechazado en ADR-0007).
- Tratar marks/headers puestos por el guest como atribución de red (rechazado en ADR-0008; evaluación).
- Tratar create→exec→destroy por comando de shell como la superficie de integración del agente (rechazado como producto en [ADR-0009](adr/0009-agent-sessions.md); `asp sandbox run` se conserva como primitiva de CI/ops).
- Abrir la LAN del usuario por defecto o por port forward (rechazado en [ADR-0010](adr/0010-on-demand-local-net.md)). El túnel completo hacia el portátil es solo el opt-in `--local-net` (comandos WireGuard cableados, sin paquetes demostrados); sin ese flag, y si el agente cae, no hay `0.0.0.0/0` ni vuelta silenciosa al proxy del nodo.
- Declarar SSH multi-user-safe **sin** `ASP_SSH_AGENT_SOCK_TEMPLATE` (bridge global legacy); con template + confirm multi-user, el aislamiento es por path — keys siguen siendo responsabilidad de ops (ADR-0007 fase 3u.4).
