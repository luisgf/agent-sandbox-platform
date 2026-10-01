# Roadmap

Historia de fases del MVP hasta el estado **solution complete** (2a) y endurecimiento post-MVP (2b–2f). Cada fase hecha incluye qué entregó, por qué importaba y gaps residuales.

Tras 2f: **readiness corporativa** (IdP humano, multi-user) y Fases 3–4 (multi-nodo / escala). Ver § «Readiness corporativa» y [ADR-0007](adr/0007-multi-user-identity.md).

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

- Guía: [`bare-metal-ch.md`](bare-metal-ch.md).

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

1. **Identity + SSH guest→host vsock** — `HostVsockService` (`--host-vsock`): **26501** SSH / **26502** identity; lab `--host-vsock-dir`. En CH productive: reconciler **`AttachSandbox`** → UDS `{vsock}_{port}` (ver [`why-ch-hybrid-guest-host.md`](why-ch-hybrid-guest-host.md)); AF_VSOCK Listen solo no basta.
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
2. **Multi-node leases** — migración `004`; claim/renew TTL 30s; **sin STONITH**.
3. **OIDC key rotation** — `ASP_OIDC_KEY` + `ASP_OIDC_KEY_PREV`.

**Qué entregó / por qué importaba:** el proxy dejó de ser solo “check API”; leases evitan double-claim ingenuo entre nodos.

**Gaps residuales:** HTTP_PROXY sigue voluntario sin nft; fencing real ausente; attest ausente.

- **Hecho.**

## Fase 2c — Seguridad endurecida

1. **Remote attestation (MVP práctico)** — `BootStatement` firmado ECDSA (`ASP_ATTEST_KEY`); APIs attest/verify; claim OIDC opcional. Interfaz `Attestor` para TPM/SEV futuro.
2. **FenceProvider** — Noop, HTTPWebhook, Redfish/IPMI stubs; al reclaim de `running` con lease expirado.
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

Docs: [`adr/0005-fase-2d-hardening.md`](adr/0005-fase-2d-hardening.md), `docs/why-2d-*.md`.

- **Hecho.**

## Fase 2e — nft completo + SSH guest auto

1. **nftables anti-bypass completo** — HTTP(S) puertos configurables + DNS redirect/drop; `--nft-egress-mode=soft|enforce`.
2. **SSH agent mount automatizado** — `vsock-ssh-agent-proxy` + `ssh-agent-vsock.service`; `--guest-ssh-agent-auto`.

**Qué entregó / por qué importaba:** frontera de egress host-side creíble en bare-metal; `SSH_AUTH_SOCK` out-of-the-box en la imagen.

**Límites honestos 2e:** sin TAP/KVM en CI no hay bypass-proof hardware; Enforce = ops bare-metal; virtiofs SSH sigue manual.

ADR: [`adr/0006-fase-2e-nft-ssh-guest.md`](adr/0006-fase-2e-nft-ssh-guest.md).

- **Hecho.**

## Fase 2f — Agent/ops CLI `asp`

1. **CLI demo** — `cli/` + binario `asp` (`make asp`): `sandbox create|get|list|exec|delete|run`.

**Qué entregó / por qué importaba:** one-liner create→wait→exec→destroy sin reimplementar poll en cada script; exit code del guest al shell.

**Gaps residuales:** no es SDK multi-lenguaje ni TUI; exec no streaméa byte-a-byte (JSON acumulado del CP).

Docs: [`why-cli-asp.md`](why-cli-asp.md).

- **Hecho.**

## Readiness corporativa — Identidad multi-usuario (diseño)

Gap explícito post-2f: la identidad operativa sigue siendo **tenant + sandbox + nodo** (API keys de CP). No hay sujeto humano, RBAC fino ni `user_sub` en tokens de workload hacia Entra/Okta. El SSH agent host-held es un bridge **global** al proceso — inseguro si varios humanos comparten nodo.

**Decisión de diseño:** [ADR-0007](adr/0007-multi-user-identity.md) · narrativa [`why-multi-user-identity.md`](why-multi-user-identity.md).

Rollout:

| Subfase | Entrega | Estado |
|---|---|---|
| **3u.1** | Schema `owner_sub` / `owner_email` + audit `actor_sub` | ✅ **Hecho** (migración `007_multi_user_identity.sql`; lab: vacío OK; header `X-ASP-Actor-Sub`) |
| **3u.2** | Validación JWT IdP (Entra/Okta/OIDC) en API del CP | ✅ **Hecho** (`internal/authn/idp`; `ASP_IDP_*`; lab default off) |
| **3u.3** | RBAC admin / operator / viewer desde claims IdP | ✅ **Hecho** (`ASP_IDP_ROLE_*`; list tenant-wide; destroy operator=propios) |
| **3u.4** | SSH: confirm default-on + registry/template por `owner_sub` (opción A MVP) | ✅ **Hecho** (`ASP_SSH_AGENT_SOCK_TEMPLATE`; ServeConnScoped; approve `actor_sub`; no spawner de agents) |
| **3u.5** | Mint OIDC workload con `user_sub` / `act` desde `owner_sub` | Pendiente |

**Criterio “corporate ready” (identidad):** create/exec/destroy atribuibles a humano; JWT de workload con cadena `user_sub`; SSH no compartido a ciegas entre usuarios del mismo nodo. API keys quedan como principals de servicio.

**Estado:** diseño aceptado (2026-10). Fases **3u.1–3u.4** implementadas (schema + JWT IdP + RBAC + SSH scoped MVP); workload `user_sub` (3u.5) pendiente. SSH MVP = template/path ops, no daemon manager.

## Fase 3 — Multi-nodo y fiabilidad

- Scheduler por capacidad; métricas/SLOs; caos; fencing BMC de producción endurecido.
- Attestors hardware (TPM/SEV) vía `Attestor`.
- Observabilidad de revoke, SignRequest deny, nft enforce failures.
- Identidad multi-usuario / IdP: ver § readiness + ADR-0007 (puede avanzar en paralelo a 3u.*).

## Fase 4 — Escala y perfiles

- Pools precalentados; perfil Firecracker; workspace persistente cifrado.
- IPv6 / ampliación de nft; posibles NetworkPolicy-like rules por sandbox en el nodo.

## Fuera de alcance inicial

- Ejecutar sandboxes como Kubernetes Pods (ADR-0004).
- Exponer microVMs directamente a Internet.
- Guardar claves privadas o refresh tokens en imágenes guest.
- Compatibilidad con workloads que requieran `NET_ADMIN` en el guest.
- Prometer que SoftFail nft/TAP equivale a enforce en producción.
- Tratar UID Linux del guest como identidad humana (rechazado en ADR-0007).
- Declarar SSH multi-user-safe **sin** `ASP_SSH_AGENT_SOCK_TEMPLATE` (bridge global legacy); con template + confirm multi-user, el aislamiento es por path — keys siguen siendo responsabilidad de ops (ADR-0007 fase 3u.4).
