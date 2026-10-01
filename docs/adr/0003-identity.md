# ADR-0003: Identidad dentro del guest (SSH agent + OIDC)

- **Estado:** Aceptada
- **Fecha:** 2026-09
- **Relacionados:** [0001](0001-vmm-choice.md), [0005](0005-fase-2d-hardening.md) (SSH confirm), [0006](0006-fase-2e-nft-ssh-guest.md) (guest mount auto), [0007](0007-multi-user-identity.md) (humano ↔ sandbox / IdP), [`../why-2e-ssh-guest-mount.md`](../why-2e-ssh-guest-mount.md), [`../why-ch-hybrid-guest-host.md`](../why-ch-hybrid-guest-host.md) (CH hybrid guest→host), [`../why-multi-user-identity.md`](../why-multi-user-identity.md)

## Contexto

Los workloads de agente necesitan:

1. **Firmar operaciones SSH** (git fetch, deploy keys, bastion) sin copiar claves privadas al disco del guest.
2. **Identidad federada de corta vida** (OIDC) hacia APIs corporativas, sin refresh tokens ni atributos de tenancy elegidos por el guest.
3. Canal host↔guest que no dependa de la red TAP (la red es no confiable / restringida).

Amenazas: guest comprometido que exfiltre `~/.ssh`, que pida firmas en silencio, o que fabrique un JWT con `tenant_id`/`sandbox_id` arbitrarios.

## Decisión

Dos mecanismos complementarios; **ningún secreto de larga duración** vive en la imagen guest.

### 1) SSH agent host-held (vsock)

- El agente SSH y sus claves permanecen en el **host** (`SSH_AUTH_SOCK` del operador/servicio, o `FakeAgent` en lab).
- El node-agent expone un bridge:
  - Unix: `--ssh-agent-bridge=/path.sock`
  - Guest→host productivo (Cloud Hypervisor / Firecracker hybrid): con `--host-vsock --reconcile`, el reconciler hace **`AttachSandbox`** y escucha UDS **`{vsockPath}_26501`** (p. ej. `/run/asp/vsock-{id}.sock_26501`). El guest diala AF_VSOCK CID **2**:26501; el VMM conecta a ese UDS. Ver [`../why-ch-hybrid-guest-host.md`](../why-ch-hybrid-guest-host.md).
  - Lab / VMM no-hybrid: `--host-vsock` también puede abrir **AF_VSOCK Listen(26501)** y/o `--host-vsock-dir` → `host-vsock-26501.sock`. **AF_VSOCK Listen solo no basta** con el muxer hybrid de CH (sin `{muxer}_{port}` → RST).
- En el guest, `vsock-ssh-agent-proxy` (+ `ssh-agent-vsock.service`) materializa `SSH_AUTH_SOCK=/run/agent-sandbox/ssh-agent.sock` dialando `2:26501` (Fase 2e, `--guest-ssh-agent-auto`) — sin cambios en el guest entre AF_VSOCK global y hybrid attach.
- Opcional: `--ssh-agent-confirm` exige `POST /v1/internal/ssh-agent/approve` (TTL one-shot) antes de cada `SSH2_AGENTC_SIGN_REQUEST`; sin approve → `SSH_AGENT_FAILURE`.

### 2) OIDC ligado a atestación / sandbox

- El guest solo puede pedir token vía socket de identidad: `POST /v1/tokens/oidc` con body **`{ "aud": "…" }`** (y opcionalmente nonce).
- El node-agent (`--identity-listen` o host-vsock **26502**, en CH via `{vsockPath}_26502` + mismo dial guest CID 2:26502) fija `tenant_id`, `sandbox_id`, nodo, TTL y claims de autoridad a partir de la conexión / headers internos / store — **el guest no los elige**.
- El control plane firma JWT de corta vida (`ASP_OIDC_KEY`) y publica JWKS (`GET /oidc/jwks.json`) + discovery.
- Rotación básica: `ASP_OIDC_KEY` (mint) + `ASP_OIDC_KEY_PREV` (overlap en JWKS).
- Tras Fase 2c, el mint puede exigir evidencia de attestation fresca (`x_asp_attestation`).

## Alternativas consideradas

| Alternativa | Pros | Contras | Decisión |
|---|---|---|---|
| Copiar deploy key al guest | Simple | Exfiltración trivial; rotación dolorosa | Rechazada |
| virtiofs del `SSH_AUTH_SOCK` host | Sin proxy | Montaje CH por sandbox; path frágil; no auto | Alternativa ops manual; auto = vsock (ADR-0006) |
| SPIFFE/SPIRE completo | Estándar fuerte | Ops pesada para MVP | Futuro posible; hoy OIDC corto + JWKS |
| Tokens opacos en disco guest | Familiar | Larga duración / robo de imagen | Rechazada |
| Guest genera su propio JWT | Cero round-trips | El guest no es autoridad | Rechazada |

## Consecuencias

### Positivas

- Claves privadas nunca cruzan la frontera de la microVM.
- Claims de tenancy son **server-side**; un guest roto no se auto-promociona de tenant.
- Confirm gate añade fricción humana/API intencional ante firmas sensibles.
- Mismo modelo de puertos vsock documentado (26500/26501/26502) en diagrama y smokes.

### Negativas

- Dependencia de vsock / host-vsock correctamente cableado (en CH: `AttachSandbox` por sandbox); sin unidad guest, no hay `SSH_AUTH_SOCK`.
- Confirm gate puede romper automatizaciones que firman en bucle → hay que aprobar o desactivar el flag en lab.
- Indisponibilidad de JWKS/atestación → **falla cerrada** (no hay token de respaldo persistente).
- Attestation MVP es software-signed (ECDSA `ASP_ATTEST_KEY`), no TPM/SEV.

### Follow-ups

- Guest mount auto (hecho en 2e).
- Hardware attestors vía interfaz `Attestor` (Fase 3+).
- Política de confirm por fingerprint de clave (hoy: gate global on/off).
- **Sujeto humano / IdP corporativo** (`owner_sub`, RBAC, workload `user_sub`): [0007](0007-multi-user-identity.md) — fases 1–5 hechas (incl. SSH scoped MVP + `user_sub`/`act` en mint).

## Detalle de implementación en este repo

| Pieza | Ruta / API |
|---|---|
| SSH bridge | `node-agent/internal/sshagent/` (`bridge.go`, `confirm.go`, `guest_mount.go`) |
| Host vsock | `node-agent/internal/hostvsock/` — `AttachSandbox` hybrid `{vsock}_{26501|26502}` + AF_VSOCK / `--host-vsock-dir` lab |
| Identity proxy | `node-agent/internal/identity/` — unix/TCP → CP mint |
| Guest proxy | `images/guest/cmd/vsock-ssh-agent-proxy/` + `images/guest/systemd/ssh-agent-vsock.service` |
| OIDC signer | `control-plane/internal/oidc/` |
| Attest | `control-plane/internal/attest/` + `node-agent/internal/attest/` |
| Mint | `POST /v1/internal/oidc/token` (nodo/lab); guest vía identity proxy |
| JWKS / discovery | `GET /oidc/jwks.json`, `GET /.well-known/openid-configuration` |
| Approve SSH | `POST /v1/internal/ssh-agent/approve` (node-agent) |
| Flags | `--host-vsock`, `--host-vsock-dir`, `--ssh-agent-bridge`, `--ssh-agent-confirm`, `--identity-listen`, `--guest-ssh-agent-auto`, `ASP_OIDC_KEY`, `ASP_OIDC_KEY_PREV`, `ASP_OIDC_ISSUER`, `ASP_ATTEST_KEY` |
| Notas vsock | `scripts/guest-vsock-notes.md`, `docs/why-ch-hybrid-guest-host.md` |
| Smoke | `scripts/smoke-identity-egress.sh` |

### Rotación de claves OIDC (ops)

1. Generar nuevo PEM RSA 2048 (`openssl genrsa`).
2. Desplegar `ASP_OIDC_KEY=<nuevo>` y `ASP_OIDC_KEY_PREV=<anterior>`.
3. JWKS publica ambas; mint firma solo con la actual (`kid` current).
4. Tras TTL (≤ 5m default) + refresco de cachés JWKS, retirar `ASP_OIDC_KEY_PREV`.

## Límites honestos / no-goals

- El socket Unix en el guest con permisos de fichero **no** es la autorización real; lo es el node-agent + CP.
- FakeAgent (0 keys) en dry-run sin `SSH_AUTH_SOCK` — útil para protocolo, inútil para git real.
- Un par UDS hybrid por sandbox×puerto; override de `VsockPath` mid-life requiere re-Attach (hoy no).
- No hay vault de secretos genérico dentro del guest; solo SSH bridge + OIDC corto.
- Virtiofs SSH sigue documentado como path manual, no el automatizado.

## Referencias cruzadas

- ADR-0005 (confirm), ADR-0006 (guest auto), **ADR-0007 (multi-user / IdP humano)**
- Why: [`../why-2d-ssh-confirm.md`](../why-2d-ssh-confirm.md), [`../why-2e-ssh-guest-mount.md`](../why-2e-ssh-guest-mount.md), [`../why-multi-user-identity.md`](../why-multi-user-identity.md)
- Arquitectura § Identidad: [`../architecture.md`](../architecture.md)
- Diagrama: [`../diagram.mmd`](../diagram.mmd)
