# ADR-0005: Rotación de certificados de nodo, mTLS estricto y confirmación de firmas SSH (antes «Fase 2d»)

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

- `POST /v1/nodes/{id}/rotate-cert` — autorizado con API key admin (plataforma) o admin del IdP, o por el certificado vigente del propio nodo (ver «renovación»). Originalmente también con el bootstrap token; retirado en 2026-10 (#94). Emite cert nuevo, persiste `cert_serial` + `cert_fingerprint`, y mete el fingerprint anterior en `node_cert_revocations`.
- `POST /v1/nodes/{id}/revoke` — marca `nodes.revoked_at` y revoca el fingerprint actual.
- Middleware mTLS rechaza fingerprints en la revoke set (`client certificate revoked`).

**Actualizado 2026-10:** «API key admin» era en realidad cualquier API key válida, de cualquier tenant, y un admin del IdP no podía usar estas rutas porque el middleware se las saltaba. Ahora pasan por el middleware y el handler aplica la misma regla que a cordon/uncordon:

| Llamante | rotate-cert | revoke, cordon, uncordon | `GET /v1/nodes` |
|---|---|---|---|
| IdP admin | sí | sí | sí |
| IdP operador | 403 | 403 | sí |
| IdP viewer | 403 | 403 | 403 |
| API key de plataforma (`scope=platform`, p. ej. `ASP_BOOTSTRAP_API_KEY`) | sí | sí | sí |
| API key de tenant | 403 | 403 | 403 |
| Bootstrap token de nodo | 401 (solo enrola) | 401 | 401 |

- Con `ASP_IDP_REQUIRED=1` estas rutas piden un token del IdP, como las demás de usuario; el bootstrap token no vale para `rotate-cert`: lo tienen todos los nodos, y con él uno podía quedarse con la identidad de otro (clave y certificado nuevos, el anterior revocado, y re-registro con su propio endpoint). Solo enrola.
- En el lab abierto (sin API keys ni IdP) revoke, cordon, uncordon y la lista quedan abiertas, como el resto de la API. `rotate-cert` sigue pidiendo credenciales (401 sin ellas, también con el bootstrap token): entrega la clave privada de un nodo.
- El bootstrap token se compara en tiempo constante.

**Actualizado 2026-10 (enroll):** `POST /v1/nodes/enroll` solo pedía el bootstrap token compartido, y el llamante elegía el `id`. Re-enrolar un id existente emitía un cert nuevo con ese CN, revocaba el anterior y limpiaba `revoked_at`: quien tuviera el token suplantaba cualquier nodo y echaba al legítimo. Ahora:

- **Tokens de enroll de un solo uso.** `POST /v1/nodes/enroll-tokens` (admin del IdP o API key de plataforma; `asp node enroll-token [--node-id ID] [--ttl 1h]`) devuelve un token `asp_enroll_…` que caduca (1 h por defecto, 7 días como mucho). El store guarda solo su SHA-256 (migración `015_node_enroll_tokens.sql`). Con `node_id` el token queda **fijado** a ese nodo.
- `/enroll` acepta el bootstrap token o un token de enroll. El token se marca usado en la misma transacción que el enroll (`SELECT … FOR UPDATE`): dos enrolls a la vez no pueden usarlo los dos.
- **Ningún enroll echa a un nodo vivo sin permiso.** Un id con certificado vigente y no revocado solo se re-enrola con un token fijado a él (**409** si no, con el comando para pedirlo). El bootstrap token sigue valiendo para labs, pero solo para un id sin certificado o un nodo revocado. Un enroll rechazado no revoca nada ni gasta el token.
- Errores: token usado, caducado o desconocido (o bootstrap token incorrecto) → **401**; token fijado a otro id → **403**.
- El plano de control comprueba todo esto antes de emitir el certificado y lo vuelve a comprobar al guardarlo. Si al final lo rechaza, el certificado se descarta: su clave privada nunca sale del proceso.
- El node-agent arrancado otra vez con `--enroll` recibe 409 y sigue con el certificado de `--cert-dir` si es de ese nodo y no ha caducado. `--enroll-token` (`ASP_NODE_ENROLL_TOKEN`) pasa un token de enroll en vez del bootstrap token.

**Actualizado 2026-10 (renovación):** los certificados de nodo duran 365 días y nada los renovaba: `rotate-cert` pedía un token o una API key que el nodo no tiene, así que al año todos los nodos fallaban el handshake mTLS sin aviso. Ahora:

- `rotate-cert` acepta también el **certificado vigente del propio nodo** por mTLS (`ASP_CLIENT_CA`): el middleware liga la identidad aunque la ruta no la exija (los admins la usan sin certificado de cliente), y el handler responde **403** si el certificado es de otro nodo. Un certificado ya revocado no puede rotar (**401**).
- El node-agent, con mTLS contra un control plane `https://`, revisa su certificado al arrancar y cada 24 h. Si le queda menos de un tercio de vida (unos 122 días con 365), pide uno nuevo, lo escribe en `--cert-dir` de forma atómica (fichero temporal + rename) y lo usa sin reiniciar: el cliente del control plane y el listener `--agent-tls-listen` leen el certificado en cada handshake, y las conexiones ociosas se cierran para que las nuevas presenten el renovado. La firma de atestaciones con la clave del certificado también sigue al nuevo.
- Si la renovación falla y quedan menos de 30 días, cada intento deja un aviso en el log; caducado, un error.
- El control plane guarda cuándo caduca el certificado vigente (`nodes.cert_not_after`, migración `016`), y `GET /v1/nodes` y `asp node list` lo muestran.
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
- Listar claves sigue funcionando. El resto de mensajes no llega nunca al agente del host ([ADR-0003](0003-identity.md) § 1).
- Aplica al bridge unix y a host-vsock :26501.
- El mount automático del agente en el guest ([0006](0006-fase-2e-nft-ssh-guest.md)) **no** desactiva el confirm gate: con `SSH_AUTH_SOCK` más a mano en el guest, la aprobación explícita importa más en los hosts sensibles.
- Con `ASP_MULTI_USER=1` o `ASP_SSH_AGENT_SOCK_TEMPLATE` el gate queda *default-on*; el approve acepta `actor_sub` / `X-ASP-Actor-Sub` para el audit, y el scoping de claves lo hace la plantilla de sockets ([0007](0007-multi-user-identity.md)), no el gate solo.

**Actualizado 2026-10 (aprobaciones por sandbox):** una aprobación la consumía la primera firma que llegara, de cualquier sandbox del nodo, y el `token` de la respuesta no lo pedía nadie. Ahora:

- `POST /v1/internal/ssh-agent/approve` exige `sandbox_id` (**400** sin él). La aprobación solo la consume una firma que llegue por el acceptor hybrid de esa sandbox (`{vsock}_26501`). Si hay varias, se usa la que caduca antes.
- La respuesta trae `approval_id` en vez de `token`. Es un id de auditoría: sale en el log al aprobar y en la firma que desbloquea, pero nadie tiene que presentarlo.
- `--ssh-agent-bridge` y el listener host-vsock global no saben qué guest llama. Con el gate activo, toda firma por ellos se deniega. En lab, `--insecure-ssh-agent-global-approvals` (`ASP_INSECURE_SSH_AGENT_GLOBAL_APPROVALS=1`) acepta aprobaciones sin `sandbox_id`, que solo consumen esos listeners: la usa el primer guest que firme por ellos.

### 4) nftables anti-bypass (sketch) + enforcer

**Por qué:** ver ADR-0002 — proxy voluntario no es frontera.

**Qué decidimos en 2d (sketch):**

- Script `scripts/nftables-egress-redirect.sh` (dry-run / apply / remove).
- Flag `--egress-nft-redirect`: best-effort; **SoftFail** sin root/`nft`.
- Redirige TCP 80/443 del subnet guest al puerto del forward proxy.

> **Addendum:** el sketch se **completa** en ADR-0006 (puertos configurables, DNS redirect/drop, modos soft|enforce, alias `--nft-egress-redirect`, hoy `--egress-nft-redirect`).

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

## Límites honestos / no-goals

- SoftFail nft **no** demuestra bypass-proof en CI.
- Strict mTLS sin cuidar el enroll listener puede dejar un plaintext expuesto si se bind-ea a `0.0.0.0` por error.
- Confirm gate no distingue claves “sensibles” vs “ci”; es all-or-nothing por proceso, y la aprobación es por sandbox, no por clave (no hay allowlist de fingerprints). Una automatización que firma en bucle necesita un supervisor que renueve aprobaciones, o desactivar el flag en un laboratorio.
- El endpoint de approve vive en la API local del agente (`--agent-listen`, con el token del agente): no se expone fuera del host.
- La CA de enrollment sigue siendo la raíz de confianza de los nodos: proteger `ASP_CA_KEY` es ops, no algo que resuelva esta API. Revocar no se deshace en silencio: el nodo vuelve con un re-enroll.
- `ASP_MTLS_STRICT` solo vale con TLS (`ASP_TLS_CERT` / `ASP_TLS_KEY`) y `ASP_CLIENT_CA`; sin TLS se ignora con un aviso. El enroll listener sirve `/healthz` a propósito, para probes locales, y no expone datos de ningún tenant.
- Attestation sigue siendo software (2c); este ADR no añade TPM.

## Referencias cruzadas

- Roadmap § Fase 2d: [`../roadmap.md`](../roadmap.md)
- Compleción nft/SSH guest: [0006](0006-fase-2e-nft-ssh-guest.md)
- Operaciones: [operaciones de seguridad](../how-to/security-operations.md)
