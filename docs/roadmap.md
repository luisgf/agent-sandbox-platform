# Roadmap

## Fase 0 — Esqueleto

- Contratos de componentes, ADRs y esquema SQL inicial.
- API, node-agent y pod-daemon compilables con comportamiento placeholder.
- Criterio de salida: builds locales reproducibles y revisión de amenazas inicial.
- **Hecho.**

## Fase 1a — MVP slice (hecho)

- Control plane: store in-memory thread-safe; Create/Get/List sandboxes; stub provisioner (`requested` → `running` + `node_id=local-dev`).
- Registro de nodos: `POST /v1/nodes/register`, `GET /v1/nodes`.
- Node-agent: cliente HTTP de Cloud Hypervisor sobre Unix socket (CreateVM/Boot/Delete + Ping/Info); `FakeVMM` y `--dry-run`.
- Egress: tipo `Allowlist` + `Check(host)` con tests.
- Smoke: ver [`mvp-smoke.md`](mvp-smoke.md).
- **Hecho** (sin Postgres real ni CH en CI).

## Fase 1b — Postgres control-plane (hecho)

- Persistencia PostgreSQL vía `DATABASE_URL` (`PostgresStore` + pgx/v5); `MemoryStore` sigue siendo el default offline.
- Migraciones embebidas (`migrations/001_init.sql`) aplicadas al arrancar.
- Journal: `sandbox_events` en create/state changes; `node_events` en register; `GET /v1/sandboxes/{id}/events`.
- API keys opcionales (tabla `api_keys`, sha256, Bearer middleware; `ASP_BOOTSTRAP_API_KEY` / `ASP_REQUIRE_API_KEY`).
- `docker-compose.yml` con `postgres:16` para smoke local.
- Tests offline sin PG; integración opcional si `DATABASE_URL` está set.
- **Hecho**.

## Fase 1c — mTLS enrollment + exec dataplane (hecho)

- PKI efímera de lab; `POST /v1/nodes/enroll`; TLS opcional; heartbeat.
- pod-daemon HTTP JSON; node-agent exec proxy; CP reenvía exec a `agent_endpoint`.
- Smoke: [`scripts/smoke-enroll-exec.sh`](../scripts/smoke-enroll-exec.sh).
- **Hecho**.

## Fase 1d — Egress allowlist + OIDC/SSH bridge (hecho)

- Tenant egress API; OIDC discovery/JWKS/mint; identity proxy; SSH agent bridge.
- Smoke: [`scripts/smoke-identity-egress.sh`](../scripts/smoke-identity-egress.sh).
- **Hecho**.

## Fase 1e — Reconciler real (hecho)

- `ASP_AUTO_PROVISION` default false; claim/work/status/destroy; `--reconcile`.
- Smoke: [`scripts/smoke-reconcile.sh`](../scripts/smoke-reconcile.sh).
- **Hecho**.

## Fase 1f — Ops guide bare-metal CH (hecho)

- Guía: [`bare-metal-ch.md`](bare-metal-ch.md).
- **Hecho**.

## Fase 1g — Multi-socket CH spawn por sandbox (hecho)

- Spawn `cloud-hypervisor --api-socket /run/asp/ch-{id}.sock` por Start; Stop delete+kill+rm.
- **Hecho**.

## Fase 1h — Vsock productivo guest↔host exec (hecho)

- CID único ≥ 3; HybridVsockDialer CONNECT 26500; guest `--listen vsock`.
- **Hecho**.

## Fase 2a — Solution MVP complete (hecho en este slice)

Cierre shippable del MVP “solution complete”:

1. **Identity + SSH guest→host vsock**
   - `HostVsockService` (`--host-vsock`): AF_VSOCK puertos **26501** (SSH agent pump) y **26502** (identity HTTP).
   - Guest diala CID **2** (`ASP_HOST_CID=2`); lab: `--host-vsock-dir` → unix `host-vsock-{port}.sock`.
   - Reconciler: symlink `/run/asp/ssh-agent-{id}.sock` → `--ssh-agent-bridge` (virtiofs docs).
   - Notas: [`scripts/guest-vsock-notes.md`](../scripts/guest-vsock-notes.md).
2. **TAP auto** — `--tap-auto`: crea/borra `asp-{shortid}` alrededor de Start/Stop; SoftFail sin CAP_NET_ADMIN.
3. **Guest image** — Dockerfile + systemd/OpenRC; `scripts/build-guest-rootfs.sh`.
4. **Pack** — `scripts/pack-release.sh` + `Makefile` (`test`, `smoke`, `pack`).
5. **Docs** — roadmap MVP completo; bare-metal e2e actualizado.

### Known limits (honestos)

| Límite | Detalle |
|---|---|
| Nested virt | Solo lab; densidad/latencia peores |
| No Windows guests | Solo Linux microVMs |
| CH version pin | Ops fija release validada; no tracking automático |
| Virtiofs SSH sock | Symlink host listo; **path auto = vsock proxy en guest (2e)**; virtiofs sigue ops manual |
| Packet intercept | Proxy HTTP/DNS en path; nft redirect **2e** (`--nft-egress-redirect`, soft\|enforce; HTTP+DNS) |
| Boot attestation | Software-signed MVP (Fase 2c); TPM/SEV = plug-in futuro |
| STONITH | Leases soft + FenceProvider opcional (webhook/Redfish/IPMI stub); BMC real sigue siendo ops |

**Criterio de salida MVP:** `go test` + `cargo test` + smokes dry-run verdes; tarball de release; bare-metal documentado.

## Fase 2b — Post-MVP slices (hecho en este parche)

1. **Egress HTTP forward proxy** — `--egress-proxy-listen :8888` (CONNECT + HTTP); allowlist desde env `ASP_EGRESS_ALLOWLIST_JSON`, header `X-ASP-Allowlist-JSON`, o cache del último exec; deny → 403. Opcional `--egress-dns-sink :5353` (NXDOMAIN non-allowlisted). Guest: `HTTP_PROXY`/`HTTPS_PROXY` → IP TAP host:8888. Ver [`bare-metal-ch.md`](bare-metal-ch.md) §3.4.
2. **Multi-node leases** — migración `004`: `sandboxes.node_lease_until`, `nodes.fence_token`. Claim/status/renew = TTL 30s; `POST /v1/sandboxes/{id}/renew-lease`; reconciler renueva handles; expiry → failed o re-request. **Sin STONITH** (límites documentados).
3. **OIDC key rotation (basic)** — `ASP_OIDC_KEY` (mint) + `ASP_OIDC_KEY_PREV` (JWKS overlap); JWKS 1–2 keys.

## Fase 2c — Seguridad endurecida (hecho en este slice)

1. **Remote attestation (MVP práctico)** — nodo firma `BootStatement` (sandbox_id, image_digest, vmm_profile, cid, node_id, ts) con `ASP_ATTEST_KEY` (ECDSA); CP `POST /v1/sandboxes/{id}/attest`, `GET …/attestation`, `POST /v1/attestation/verify`; claim OIDC opcional `x_asp_attestation` si evidencia fresh. Interfaz `Attestor` lista para TPM/SEV futuro.
2. **STONITH / fencing providers** — `FenceProvider`: `Noop`, `HTTPWebhook`, `Redfish` stub, `IPMI` stub (`ipmitool`, SoftFail). Al reclaim de sandbox **running** con lease expirado: fence del nodo viejo si `ASP_FENCE_PROVIDER` + `fence_endpoint`/`fence_token`. Docs: lease software ≠ STONITH real (BMC out-of-band).
3. **Proxy hardening** — token bucket por host/sandbox; body limit; deny non-HTTP schemes; audit JSON; MITM opcional `--egress-mitm-ca` / `ASP_EGRESS_MITM=1` (default off).

**Aún pendiente tras 2c (cubierto en 2d):** ~~rotación/revocación certs~~; ~~mTLS estricto~~; ~~SSH agent confirm~~; ~~nftables redirect sketch~~.

## Fase 2d — Hardening operativo (hecho en este slice)

1. **Node cert rotation & revocation** — `POST /v1/nodes/{id}/rotate-cert`, `POST /v1/nodes/{id}/revoke`; migración `006`; middleware rechaza fingerprints revocados.
   - **Por qué:** certs robados/expirados no deben hablar al CP para siempre.
   - **Qué ganamos:** ciclo de vida de identidad de nodo con serial/fingerprint + revoke set.
2. **Strict mTLS** — `ASP_MTLS_STRICT=1` → `RequireAndVerifyClientCert`; enroll en `ASP_ENROLL_LISTEN` (plaintext localhost).
   - **Por qué:** `VerifyClientCertIfGiven` deja hits anónimos.
   - **Qué ganamos:** listener TLS sin peers anónimos; enroll segregado.
3. **SSH agent confirmation gate** — `--ssh-agent-confirm` + `POST /v1/internal/ssh-agent/approve` (TTL one-shot); auto-deny SignRequest.
   - **Por qué:** guest comprometido puede firmar en silencio.
   - **Qué ganamos:** aprobación explícita antes de cada firma.
4. **nftables anti-bypass sketch** — `scripts/nftables-egress-redirect.sh` + `--egress-nft-redirect` (SoftFail).
   - **Por qué:** `HTTP_PROXY` es voluntario.
   - **Qué ganamos:** forzar TCP 80/443 del subnet guest por el proxy. **Completado en 2e.**

Docs: [`adr/0005-fase-2d-hardening.md`](adr/0005-fase-2d-hardening.md), `docs/why-2d-*.md`.

## Fase 2e — nft completo + SSH guest auto (hecho en este slice)

1. **nftables anti-bypass completo** — HTTP(S) puertos configurables + DNS redirect/drop; tabla `asp_egress`; `--nft-egress-mode=soft|enforce`.
   - **Por qué:** sketch 2d no cubría DNS ni otros puertos HTTP; SoftFail vs Enforce no estaba explícito.
   - **Qué ganamos:** reglas generadas/testeadas en dry-run; Enforce falla sin root/`nft`; SoftFail mantiene CI verde.
   - Docs: [`why-2e-nft-redirect.md`](why-2e-nft-redirect.md).
2. **SSH agent mount automatizado en guest** — `vsock-ssh-agent-proxy` + `ssh-agent-vsock.service`; preferencia vsock (no virtiofs); `--guest-ssh-agent-auto`.
   - **Por qué:** guest sin `SSH_AUTH_SOCK` estable no puede usar el agent del host.
   - **Qué ganamos:** unidad en imagen/rootfs; fallback unix para dry-run; contrato testeado.
   - Docs: [`why-2e-ssh-guest-mount.md`](why-2e-ssh-guest-mount.md).

**Límites honestos 2e:** sin TAP/KVM en CI no se prueba bypass-proof en hardware; Enforce nft = ops bare-metal. Virtiofs SSH sigue documentado como alternativa manual.

ADR: [`adr/0006-fase-2e-nft-ssh-guest.md`](adr/0006-fase-2e-nft-ssh-guest.md).


## Fase 2f — Agent/ops CLI `asp` (hecho en este slice)

1. **CLI demo** — módulo `cli/` + binario `asp` (`make asp`): `sandbox create|get|list|exec|delete|run`.
   - **Por qué:** smokes/`curl` no son ergonómicos para agentes/ops que prueban el lifecycle.
   - **Qué ganamos:** one-liner create→wait→exec→destroy; exit code del guest; tests httptest + smoke opcional.
   - Docs: [`why-cli-asp.md`](why-cli-asp.md).

## Fase 3 — Multi-nodo y fiabilidad

- Scheduler por capacidad; métricas/SLOs; caos; fencing BMC de producción endurecido.

## Fase 4 — Escala y perfiles

- Pools precalentados; perfil Firecracker; workspace persistente cifrado.

## Fuera de alcance inicial

- Ejecutar sandboxes como Kubernetes Pods.
- Exponer microVMs directamente a Internet.
- Guardar claves privadas o refresh tokens en imágenes guest.
- Compatibilidad con workloads que requieran `NET_ADMIN` en el guest.
