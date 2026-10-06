# Control plane

Servicio Go multi-tenant: API HTTP (TLS opcional), store in-memory (default) o PostgreSQL (`DATABASE_URL`), journal de eventos, API keys, enrollment PKI, egress allowlist, OIDC (JWKS/mint) y proxy de exec hacia node-agents.

## Endpoints

| Método | Ruta | Estado |
|---|---|---|
| GET | `/healthz` | OK (siempre público) |
| GET | `/.well-known/openid-configuration` | OIDC discovery |
| GET | `/oidc/jwks.json` | JWKS público |
| POST | `/v1/internal/oidc/token` | Mint JWT (nodo; tenant desde store) |
| POST | `/v1/sandboxes` | Create: coloca en un nodo con hueco (503 si ninguno cabe, 409 si el pin no vale); con `ASP_AUTO_PROVISION=1`, stub → `running` |
| GET | `/v1/sandboxes?tenant_id=` | List |
| GET | `/v1/sandboxes/{id}` | Get |
| GET | `/v1/sandboxes/{id}/events` | Audit trail |
| POST | `/v1/sandboxes/{id}/exec` | Proxy a node-agent (+ `egress_allowlist`) |
| POST | `/v1/sandboxes/{id}/claim` | Claim atómico del nodo asignado (+ lease 30s) |
| POST | `/v1/sandboxes/{id}/status` | Estado observado por el agente |
| POST | `/v1/sandboxes/{id}/renew-lease` | Renueva `node_lease_until` |
| POST | `/v1/sandboxes/{id}/attest` | Guarda evidencia de boot firmada (nodo) |
| GET | `/v1/sandboxes/{id}/attestation` | Última atestación |
| POST | `/v1/attestation/verify` | Verifica bundle sin persistir |
| PUT/GET | `/v1/tenants/{id}/egress` | Allowlist de egress |
| POST | `/v1/tenants/{id}/egress/check` | Helper de evaluación |
| POST | `/v1/nodes/enroll` | Bootstrap token → client cert PEMs |
| POST | `/v1/nodes/{id}/rotate-cert` | Nuevo cert (bootstrap o API key); revoca fingerprint anterior |
| POST | `/v1/nodes/{id}/revoke` | Marca nodo + fingerprint revocados |
| POST | `/v1/nodes/register` | Registra/actualiza nodo |
| POST | `/v1/nodes/{id}/heartbeat` | `last_seen_at` |
| GET | `/v1/nodes/{id}/work` | Trabajo para reconciler (solo sus sandboxes; refresca `last_seen_at`) |
| POST | `/v1/nodes/{id}/cordon` | Sin colocaciones nuevas (admin) |
| POST | `/v1/nodes/{id}/uncordon` | Vuelve al reparto (admin) |
| GET | `/v1/nodes` | Lista nodos con asignado/ofrecido y si son planificables (admin u operador) |

## Variables de entorno

| Variable | Default | Descripción |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | Bind address |
| `ASP_SHUTDOWN_TIMEOUT` | `30s` | Al recibir SIGTERM/SIGINT el API deja de aceptar conexiones y espera hasta este tiempo a las peticiones en curso (los exec en streaming incluidos); después cierra las que queden y lo registra con su número. Los bucles de fondo (idle reaper, monitor de nodos, refresco del JWKS) paran y el pool de Postgres se cierra al final. |
| `DATABASE_URL` | (unset) | Si está set → PostgresStore + migraciones embebidas |
| `ASP_REQUIRE_API_KEY` | unset | `1` fuerza Bearer auth |
| `ASP_IDP_ISSUER` | unset | Issuer OIDC corporativo; vacío = IdP off (lab) |
| `ASP_IDP_AUDIENCE` | unset | Audiencia esperada del JWT (`aud`) |
| `ASP_IDP_JWKS_URL` | unset | JWKS; si vacío → discovery desde issuer. Se refresca cada 5 min (una clave que el IdP retira deja de validar) y, ante un `kid` desconocido, como mucho una vez cada 30 s |
| `ASP_IDP_REQUIRE_EXP` | `1` | `0` acepta JWT sin `exp` (no recomendado: no caducarían). `nbf` admite 1 min de desfase de reloj; un `iat` más de 5 min en el futuro se rechaza |
| `ASP_IDP_REQUIRED` | `0` | `1` exige JWT IdP en create/list/get/exec/destroy/events |
| `ASP_IDP_ROLE_CLAIM` | `groups` | Claim de grupos/roles para RBAC (fase 3) |
| `ASP_IDP_ROLE_MAP` | unset | CSV `claim:role` (admin\|operator\|viewer); si set, gana sobre prefijo |
| `ASP_IDP_ROLE_PREFIX` | `asp-` | Prefijo → rol (`asp-admin`, …) cuando no hay map |
| `ASP_IDP_DESTROY_ANY_GROUP` | `sandbox:destroy-any` | Operator puede destroy no-propios si el claim lo incluye |
| `ASP_BOOTSTRAP_API_KEY` | unset | Key `bootstrap` tenant `default` |
| `ASP_NODE_BOOTSTRAP_TOKEN` | unset | Token para `/v1/nodes/enroll` |
| `ASP_CA_CERT` / `ASP_CA_KEY` | `/tmp/asp-dev-ca/ca.*` | CA de enrollment |
| `ASP_TLS_CERT` / `ASP_TLS_KEY` | unset | TLS servidor |
| `ASP_CLIENT_CA` | unset | Client CA (register/heartbeat/oidc mint); habilita check de revocación y ata el CN del cert a cada ruta de nodo (403 si es otro nodo) |
| `ASP_SCHED_POLICY` | `spread` | `spread` o `binpack` ([`ops-multi-node.md`](../docs/ops-multi-node.md)) |
| `ASP_SCHED_CPU_OVERCOMMIT` | `4` | vCPU por core físico; la memoria no se sobresuscribe |
| `ASP_NODE_STALE_AFTER` | `90s` | Sin señales más tiempo → el nodo no recibe sandboxes y pasa a `offline` |
| `ASP_NODE_MONITOR_INTERVAL` | `15s` | Cada cuánto revisa el monitor la vida de los nodos |
| `ASP_NODE_FAILOVER_AFTER` | `5m` | Sin señales más tiempo → fencing y sandboxes del nodo → `failed` (`node_lost`); `0`/`off` desactiva (salvo revocados) |
| `ASP_INSECURE_AGENT_HTTP` | unset | `1` → permite `agent_endpoint` `http://` fuera de loopback (`exec` sin autenticar; solo lab). Por defecto: `http://` solo en loopback, `https://` con mTLS ([ADR-0011](../docs/adr/0011-multi-node.md)) |
| `ASP_MTLS_STRICT` | unset | `1` → `RequireAndVerifyClientCert` en listener TLS |
| `ASP_ENROLL_LISTEN` | `127.0.0.1:8081` | Plaintext enroll-only cuando `ASP_MTLS_STRICT=1` |
| `ASP_OIDC_KEY` | `/tmp/asp-oidc-key.pem` | PEM RSA de firma actual (auto-create; mint) |
| `ASP_OIDC_KEY_PREV` | unset | PEM RSA previa (solo JWKS durante rotación) |
| `ASP_OIDC_ISSUER` | `http://127.0.0.1$LISTEN_ADDR` | Issuer OIDC |
| `ASP_ATTEST_KEY` | `/tmp/asp-attest-key.pem` | PEM ECDSA firma/verificación atestación |
| `ASP_ATTEST_MAX_AGE` | `10m` | Freshness para verify + claim OIDC |
| `ASP_FENCE_PROVIDER` | `noop` | `noop`\|`http_webhook`\|`redfish`\|`ipmi` |
| `ASP_FENCE_USER` | | Usuario Redfish/IPMI |
| `ASP_EGRESS_DEFAULT_ALLOW` | `1` en memory-dev | Vacío = allow-all |
| `ASP_EGRESS_DENY_DEFAULT` | unset | Vacío = deny (harden; desactiva allow memory) |
| `ASP_AUTO_PROVISION` | unset/false | `1` = stub sync Create→running; default deja `requested` |
| `ASP_SANDBOX_IDLE_TIMEOUT` | unset = **off** | Parada por inactividad. Duración Go (`2h`, `1h`, `90m`). `0` / `off` / `false` / `disabled` desactiva. El valor recomendado de lab/producción es **2h** (también vale `1h`); no es el default del proceso, para que los smokes cortos no tumben sandboxes. Flag equivalente: `-idle-timeout` / `--idle-timeout` (pisa el env). |
| `ASP_SANDBOX_IDLE_SWEEP` | `1m` | Cada cuánto el bucle del CP llama al reaper. No enciende el reaper por sí solo. Mínimo efectivo 1s. |


```bash
go test ./...
ASP_NODE_BOOTSTRAP_TOKEN=dev go run ./cmd/api
```

## Logs y coste por petición

Cada petición deja una línea `request` con método, ruta, estado, bytes, duración y origen. Los sondeos de los nodos (`/work`, heartbeats, claim, status…) y `/healthz` van a nivel Debug: con varios nodos sondeando cada 2 s taparían el resto. El middleware de API keys cuenta las claves como mucho cada 10 s y escribe `last_used_at` como mucho una vez por minuto y clave, en vez de un `count(*)` y un `UPDATE` por petición.

## Parada por inactividad

**Por qué.** Una sesión `asp session` (o un create olvidado) deja la microVM encendida hasta un `DELETE`. El reaper del control-plane marca `stopping` (o `stopped` si nunca se asignó nodo) cuando no hay actividad durante el umbral, y el reconciler del nodo apaga la VM.

**Qué cuenta como actividad.** `last_activity_at` se mueve en: create, transición a `running` (start) y **exec con respuesta correcta del node-agent** (aunque el proceso del guest salga ≠ 0). No cuentan: GET, heartbeat, renovación de lease, ni un exec que ni siquiera llega al guest.

**Cómo se configura.** Default del binario: apagado. En lab/producción:

```bash
ASP_SANDBOX_IDLE_TIMEOUT=2h   # recomendado; alternativa 1h
# o al arrancar: ./api -idle-timeout 2h
# ASP_SANDBOX_IDLE_TIMEOUT=0  o  off  → desactiva
```

La unit `scripts/systemd/asp-control-plane.service` fija `2h`. Los tests y smokes cortos no exportan la variable.

**Límites.** El reaper no es un sustituto de `asp session stop`: el fichero local de sesión sigue apuntando al id; `asp session status` y `exec` lo dicen (`idle timeout` / `idle_reaped`) y hay que `asp session start --force`. La migración `008` rellena `last_activity_at` de filas viejas con `now()`, así que al activar el reaper no se destruye de golpe todo lo creado hace horas; el reloj de esas filas empieza en la migración. Evento de auditoría: `sandbox.idle_reaped` (`stop_reason=idle_timeout`).

## Timeouts hacia el node-agent

**Por qué.** El cliente hacia el agente tenía un timeout total de 30 s, y ese timeout cuenta también la lectura del body: un `exec` en streaming (`?stream=1`) o una sesión PTY se cortaba a los 30 s aunque siguiera saliendo salida.

**Qué se limita.** Cada fase de la llamada, igual por HTTP plano (agente en el mismo host) que por mTLS (`https://`):

| Fase | Límite |
|---|---|
| Conexión TCP | 10 s |
| Handshake TLS | 10 s |
| Hasta que el agente empieza a responder (cabeceras) | 30 s |
| `exec` sin `?stream=1` y `exec/stdin`, de principio a fin | 30 s |

Un stream no tiene límite total: dura lo que el comando. Si el cliente cuelga, el plano de control cancela la llamada al agente. Si el nodo desaparece a mitad de stream, lo detecta el keep-alive de TCP en unos minutos.

**Límites.** El agente responde a un `exec` acumulado cuando el comando termina, así que ese `exec` sigue limitado a 30 s, como antes; para algo más largo, el stream (`asp session exec` lo usa por defecto). El node-agent y el plano de control envían las cabeceras del stream en cuanto el comando arranca, así que un comando que no escribe nada no agota esos 30 s. Los valores no se configuran por entorno. Por debajo, el node-agent limita a 60 s solo las llamadas acumuladas al pod-daemon, y el pod-daemon solo mata el exec acumulado a los `--exec-timeout-secs` (default 30). Un stream dura lo que el comando: termina si el cliente se va (el guest mata el comando y su grupo de procesos) y, con `--stream-idle-timeout-secs`, tras ese tiempo sin salida ni entrada.

## Lab IdP (Keycloak)

En ncc1701d el CP lab carga `ASP_IDP_*` desde `/home/ubuntu/.secrets/asp-idp.env` (no en git). Unit: `asp-control-plane.service` → `127.0.0.1:18112`. Guía: [`docs/ops-idp-keycloak-lab.md`](../docs/ops-idp-keycloak-lab.md).
