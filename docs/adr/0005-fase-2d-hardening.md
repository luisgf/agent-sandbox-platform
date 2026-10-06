# ADR-0005: Fase 2d — Hardening (certs, mTLS estricto, SSH confirm, nft sketch)

- **Estado:** Aceptada
- **Fecha:** 2026-09
- **Extiende:** [0002](0002-networking.md), [0003](0003-identity.md)
- **Completado parcialmente por:** [0006](0006-fase-2e-nft-ssh-guest.md) (nft sketch → completo)
- **Extendido por:** [0011](0011-multi-node.md) (el CN del cert se compara en cada ruta de nodo; mTLS también del CP al nodo)

## Contexto

Tras Fase 2c (attestation software, FenceProvider, proxy hardening) el MVP “solution complete” seguía con cuatro huecos operativos que un auditor corporativo señalaría al día siguiente:

1. **Certs de nodo** robados o caducados podían seguir hablando al control plane indefinidamente (no había rotate/revoke de fingerprint).
2. **`VerifyClientCertIfGiven`** en el listener TLS permitía conexiones anónimas; si una ruta quedaba mal cableada (sin middleware), era sondeable sin client cert.
3. Un **guest comprometido** con el SSH bridge activo podía pedir firmas al agente del host **en silencio**.
4. Confiar solo en **`HTTP_PROXY` del guest** es voluntario: el malware diala directo y bypasea la allowlist.

Restricciones: CI/box sin root ni KVM → cualquier nft debe SoftFail; enroll de nodos nuevos no puede exigir un client cert que aún no tienen.

## Decisiones

### 1) Rotación y revocación de certs de nodo

**Por qué:** identidad de nodo = client cert mTLS; sin ciclo de vida, un leak es permanente.

**Qué decidimos:**

- `POST /v1/nodes/{id}/rotate-cert` — autorizado con bootstrap token **o** API key admin. Emite cert nuevo, persiste `cert_serial` + `cert_fingerprint`, y mete el fingerprint anterior en `node_cert_revocations`.
- `POST /v1/nodes/{id}/revoke` — marca `nodes.revoked_at` y revoca el fingerprint actual.
- Middleware mTLS rechaza fingerprints en la revoke set (`client certificate revoked`).
- Migración: `control-plane/migrations/006_node_cert_rotation.sql`.

### 2) mTLS estricto (`ASP_MTLS_STRICT=1`)

**Por qué:** `VerifyClientCertIfGiven` deja peers TLS sin certificado.

**Qué decidimos:**

- Con `ASP_MTLS_STRICT=1` el listener TLS principal usa `RequireAndVerifyClientCert`.
- El enroll **no puede** vivir ahí (el nodo aún no tiene cert) → listener plaintext separado `ASP_ENROLL_LISTEN` (default `127.0.0.1:8081`) solo para `/healthz` y `POST /v1/nodes/enroll`.
- Alternativa ops: rotar con client cert vigente + API key sobre el listener estricto (sin abrir plaintext).
- Sin strict (lab/dev): se mantiene `VerifyClientCertIfGiven` + middleware en rutas de nodo.

### 3) SSH agent confirmation gate

**Por qué:** bridge vsock/unix convierte el guest en cliente del agente host; SignRequest silencioso es exfiltración de capacidad de firma.

**Qué decidimos:**

- Flag `--ssh-agent-confirm` / `ASP_SSH_AGENT_CONFIRM=1`.
- Antes de `SSH2_AGENTC_SIGN_REQUEST`, hace falta approve one-shot: `POST /v1/internal/ssh-agent/approve` (TTL default 30s). Sin approve → `SSH_AGENT_FAILURE` (auto-deny).
- List keys / mensajes no-sign siguen funcionando.
- Aplica al bridge unix y a host-vsock :26501.

### 4) nftables anti-bypass (sketch) + enforcer

**Por qué:** ver ADR-0002 — proxy voluntario no es frontera.

**Qué decidimos en 2d (sketch):**

- Script `scripts/nftables-egress-redirect.sh` (dry-run / apply / remove).
- Flag `--egress-nft-redirect`: best-effort; **SoftFail** sin root/`nft`.
- Redirige TCP 80/443 del subnet guest al puerto del forward proxy.

> **Addendum:** el sketch se **completa** en ADR-0006 (puertos configurables, DNS redirect/drop, modos soft|enforce, alias `--nft-egress-redirect`).

## Alternativas consideradas

| Tema | Alternativa | Por qué no |
|---|---|---|
| Revocación | CRL/OCSP clásico | Pesado para PKI de lab; fingerprint set en DB basta para nodos ASP |
| mTLS estricto | Mutual TLS en enroll con pre-shared client cert | Chicken-egg; bootstrap token + CA local es el camino MVP |
| SSH confirm | Always-ask TTY en el host | No hay TTY en servicio; API one-shot es automatizable por ops/agente supervisor |
| nft | Solo documentar “pon HTTP_PROXY” | Insuficiente frente a guest malicioso |

## Consecuencias

### Positivas

- Ciclo de vida de identidad de nodo auditable (serial/fingerprint/revoke).
- Listener TLS de producción sin peers anónimos cuando strict está on.
- Firmas SSH dejan rastro de aprobación explícita.
- Camino hacia anti-bypass cableado (completado en 2e).

### Negativas

- Ops debe documentar y proteger `ASP_ENROLL_LISTEN` (localhost o red de gestión).
- Revocar un nodo exige **re-enroll** para volver a operar.
- Confirm gate añade fricción (intencional) a firmas automatizadas.
- Sketch nft 2d era incompleto (DNS/otros puertos) — deuda pagada en 2e.

### Follow-ups

- ADR-0006 (nft completo + SSH guest auto).
- Métricas/alertas de revoke y de SignRequest denegados (Fase 3).

## Detalle de implementación en este repo

| Pieza | Ruta |
|---|---|
| Rotate/Revoke handlers | `control-plane/internal/api/` (`RotateNodeCert`, `RevokeNode`) |
| Auth middleware | `control-plane/internal/api/auth.go` — revoke set + public/node paths |
| Strict TLS + enroll listener | `control-plane/cmd/api/main.go` (`ASP_MTLS_STRICT`, `ASP_ENROLL_LISTEN`) |
| Migración | `control-plane/migrations/006_node_cert_rotation.sql` |
| SSH confirm | `node-agent/internal/sshagent/confirm.go` |
| nft sketch→2e | `node-agent/internal/nftredirect/`, `scripts/nftables-egress-redirect.sh` |
| Why docs | `docs/why-2d-certs.md`, `why-2d-mtls.md`, `why-2d-ssh-confirm.md`, `why-2d-nft.md` |

## Límites honestos / no-goals

- SoftFail nft **no** demuestra bypass-proof en CI.
- Strict mTLS sin cuidar el enroll listener puede dejar un plaintext expuesto si se bind-ea a `0.0.0.0` por error.
- Confirm gate no distingue claves “sensibles” vs “ci”; es all-or-nothing por proceso.
- Attestation sigue siendo software (2c); este ADR no añade TPM.

## Referencias cruzadas

- Roadmap § Fase 2d: [`../roadmap.md`](../roadmap.md)
- Compleción nft/SSH guest: [0006](0006-fase-2e-nft-ssh-guest.md)
- Bare-metal §8d: [`../bare-metal-ch.md`](../bare-metal-ch.md)
