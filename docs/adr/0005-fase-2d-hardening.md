# ADR-0005: Fase 2d — Hardening (certs, mTLS estricto, SSH confirm, nft redirect)

- **Estado:** Accepted
- **Fecha:** 2026-09-29

## Contexto

Tras Fase 2c (attestation, fencing, proxy hardening) quedaban cuatro huecos operativos:

1. Certs de nodo robados o caducados podían seguir hablando al control plane.
2. `VerifyClientCertIfGiven` permitía hits anónimos TLS en rutas no protegidas por middleware.
3. Un guest comprometido podía firmar en silencio vía el SSH agent bridged.
4. `HTTP_PROXY` es voluntario: el guest puede bypassear la allowlist de egress.

## Decisiones (con Por qué / Qué ganamos)

### 1) Rotación y revocación de certs de nodo

**Por qué:** un cert robado o expirado no debe poder seguir autenticándose al CP indefinidamente.

**Qué ganamos:**

- `POST /v1/nodes/{id}/rotate-cert` (bootstrap token **o** API key admin) emite un cert nuevo, guarda `cert_serial` + `cert_fingerprint`, y mete el fingerprint anterior en `node_cert_revocations`.
- `POST /v1/nodes/{id}/revoke` marca `nodes.revoked_at` y revoca el fingerprint actual.
- El middleware mTLS rechaza fingerprints revocados (`client certificate revoked`).
- Migración `006_node_cert_rotation.sql`.

### 2) mTLS estricto (`ASP_MTLS_STRICT=1`)

**Por qué:** `VerifyClientCertIfGiven` deja pasar conexiones TLS sin client cert; rutas mal cableadas o públicas por error quedan expuestas a sondas anónimas.

**Qué ganamos:**

- Con `ASP_MTLS_STRICT=1` el listener TLS principal usa `RequireAndVerifyClientCert`.
- El enroll no puede vivir en ese listener → listener plaintext separado `ASP_ENROLL_LISTEN` (default `127.0.0.1:8081`) solo para `/healthz` y `POST /v1/nodes/enroll`.
- Alternativa documentada: rotar certs con client cert vigente + API key sobre el listener estricto (sin plaintext).
- Sin strict, se mantiene `VerifyClientCertIfGiven` + middleware (lab/dev).

### 3) SSH agent confirmation gate

**Por qué:** un guest comprometido con el bridge activo puede pedir firmas al agente del host sin que el operador lo note.

**Qué ganamos:**

- Flag `--ssh-agent-confirm` / `ASP_SSH_AGENT_CONFIRM=1`.
- Antes de `SSH2_AGENTC_SIGN_REQUEST`, hace falta un approve one-shot: `POST /v1/internal/ssh-agent/approve` (TTL, default 30s). Sin approve → `SSH_AGENT_FAILURE` (auto-deny).
- Identities y mensajes no-sign siguen funcionando.
- Aplica al bridge unix y a host-vsock :26501.

### 4) nftables anti-bypass (sketch) + enforcer

**Por qué:** confiar solo en `HTTP_PROXY` del guest es voluntario; el malware puede dial directo.

**Qué ganamos:**

- Script `scripts/nftables-egress-redirect.sh` (dry-run / apply / remove).
- Flag `--egress-nft-redirect`: best-effort apply; **SoftFail** sin root/`nft`.
- Redirige TCP 80/443 desde el subnet guest hacia el puerto del forward proxy → fuerza tráfico web por allowlist/audit.
- No es un filtro completo (UDP, otros puertos, IPv6 quedan fuera del sketch).

## Consecuencias

- Ops debe documentar el listener de enroll en despliegues strict.
- Revocar un nodo exige re-enroll para volver a operar.
- Confirm gate añade fricción humana/API en firmas SSH (intencional).
- nft redirect requiere CAP_NET_ADMIN / root en el host; en CI solo dry-run + SoftFail.

## Referencias

- Migración: `control-plane/migrations/006_node_cert_rotation.sql`
- Script: `scripts/nftables-egress-redirect.sh`
- Roadmap: [`../roadmap.md`](../roadmap.md) § Fase 2d

## Addendum (Fase 2e)

El punto 4 (nft sketch) se **completa** en [`0006-fase-2e-nft-ssh-guest.md`](0006-fase-2e-nft-ssh-guest.md):
HTTP puertos configurables + DNS redirect/drop + modos soft|enforce. El SSH guest
mount automatizado también vive en 0006.
