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
| GET | `/v1/sandboxes?tenant_id=` | List, de la más reciente a la más antigua. Oculta las `deleted`; `?include_deleted=1` las incluye |
| GET | `/v1/sandboxes/{id}` | Get |
| GET | `/v1/sandboxes/{id}/events` | Audit trail |
| POST | `/v1/sandboxes/{id}/exec` | Proxy a node-agent (+ `egress_allowlist`) |
| POST | `/v1/sandboxes/{id}/stop` | Para la sandbox y **conserva su disco** ([ADR-0012](../docs/adr/0012-retained-disks.md)): `stopping` → `stopped`. Dueño, admin u operador con `destroy-any` |
| POST | `/v1/sandboxes/{id}/start` | Reanuda una `stopped` en el nodo que tiene su disco: `requested`, `boot_count` + 1. 503 si ese nodo no tiene hueco, 409 si no puede tomar sandboxes o la sandbox no está parada. Pide además el derecho de crear |
| DELETE | `/v1/sandboxes/{id}` | Borra la VM y el disco: `deleting` → `deleted` (directo si ningún nodo tiene nada). La fila se queda |
| POST | `/v1/sandboxes/{id}/claim` | Claim atómico del nodo asignado |
| POST | `/v1/sandboxes/{id}/status` | Estado observado por el agente |
| POST | `/v1/sandboxes/{id}/renew-lease` | **410**: retirado; el conjunto `assigned` de `/work` lo sustituye |
| POST | `/v1/sandboxes/{id}/attest` | Guarda evidencia de boot firmada (nodo) |
| GET | `/v1/sandboxes/{id}/attestation` | Última atestación |
| POST | `/v1/attestation/verify` | Verifica bundle sin persistir |
| PUT/GET | `/v1/tenants/{id}/egress` | Allowlist de egress. Una regla sin `port` vale para 80 y 443; otros puertos piden una regla que los nombre |
| POST | `/v1/tenants/{id}/egress/check` | Helper de evaluación |
| POST | `/v1/nodes/enroll` | Bootstrap token o token de enroll → PEMs del cert de nodo. El bootstrap token solo enrola un id sin certificado o un nodo revocado; re-enrolar un nodo vivo pide un token fijado a él (409) |
| POST | `/v1/nodes/enroll-tokens` | Token de enroll de un solo uso (admin o API key de plataforma); `node_id` lo fija a un nodo, `ttl_seconds` (1 h por defecto, 7 días máx.) |
| POST | `/v1/nodes/{id}/rotate-cert` | Nuevo cert (admin, API key de plataforma o el certificado vigente del propio nodo por mTLS: así lo renuevan los node-agents; el bootstrap token no vale); revoca fingerprint anterior |
| POST | `/v1/nodes/{id}/revoke` | Marca nodo + fingerprint revocados (admin o API key de plataforma) |
| POST | `/v1/nodes/register` | Registra/actualiza nodo |
| POST | `/v1/nodes/{id}/heartbeat` | `last_seen_at` |
| GET | `/v1/nodes/{id}/work` | Trabajo para reconciler (solo sus sandboxes; refresca `last_seen_at`), `assigned`: las que el nodo debe seguir corriendo (para el resto), y `egress`: tenant de cada sandbox y política efectiva con `version` de cada tenant |
| POST | `/v1/nodes/{id}/cordon` | Sin colocaciones nuevas (admin o API key de plataforma) |
| POST | `/v1/nodes/{id}/uncordon` | Vuelve al reparto (admin o API key de plataforma) |
| PUT / DELETE | `/v1/nodes/{id}/fence` | Fija (`{"endpoint","token"}`; el token puede ser `env:NAME` o `file:/ruta`) o quita el destino de fencing del nodo (admin o API key de plataforma; 204). Nunca se devuelve |
| GET | `/v1/nodes` | Lista nodos con asignado/ofrecido, si son planificables y cuándo caduca su certificado (`cert_not_after`) (admin, operador o API key de plataforma) |

Administrar nodos (listar, cordon, uncordon, fence, revoke, rotate-cert) nunca acepta una API key de tenant: los nodos los comparten todos los tenants (403). Con `ASP_IDP_REQUIRED=1` hace falta un token del IdP con rol admin (operador para listar). `rotate-cert` acepta además el certificado vigente del propio nodo (mTLS), para que se renueve; el bootstrap token no vale ni ahí ni en `revoke`, porque lo tienen todos los nodos y con él uno podría quedarse con la identidad de otro. En el lab abierto (sin API keys ni IdP) estas rutas quedan abiertas como el resto, salvo `rotate-cert`, que siempre pide credenciales porque entrega la clave privada de un nodo.

## Variables de entorno

| Variable | Default | Descripción |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | Bind address |
| `ASP_SHUTDOWN_TIMEOUT` | `30s` | Al recibir SIGTERM/SIGINT el API deja de aceptar conexiones y espera hasta este tiempo a las peticiones en curso (los exec en streaming incluidos); después cierra las que queden y lo registra con su número. Los bucles de fondo (idle reaper, monitor de nodos, refresco del JWKS) paran y el pool de Postgres se cierra al final. |
| `DATABASE_URL` | (unset) | Si está set → PostgresStore + migraciones embebidas |
| `ASP_BOOTSTRAP_API_KEY` | unset | Crea la primera API key (ámbito `platform`). Sin ninguna key en el store ni IdP configurado el CP **no arranca** (código 2) |
| `ASP_INSECURE_OPEN_API` | unset | `1` acepta peticiones sin credencial (solo labs y smokes dry-run; avisa al arrancar). Una credencial incorrecta se rechaza igual. `ASP_REQUIRE_API_KEY` ya no hace nada: la autenticación está siempre activa |
| `ASP_IDP_ISSUER` | unset | Issuer OIDC corporativo; vacío = IdP off (lab) |
| `ASP_IDP_AUDIENCE` | unset | Audiencia esperada del JWT (`aud`). **Obligatoria con `ASP_IDP_REQUIRED=1`**: sin ella el control plane no arranca (código 2), porque aceptaría el token que el emisor haya dado a cualquier otra aplicación de su realm. Con el IdP opcional solo deja un aviso |
| `ASP_IDP_ALLOW_ANY_AUDIENCE` | unset | `1` permite `ASP_IDP_REQUIRED=1` sin `ASP_IDP_AUDIENCE` (solo lab; avisa en el log) |
| `ASP_IDP_JWKS_URL` | unset | JWKS; si vacío → discovery desde issuer. Se refresca cada 5 min (una clave que el IdP retira deja de validar) y, ante un `kid` desconocido, como mucho una vez cada 30 s |
| `ASP_IDP_REQUIRE_EXP` | `1` | `0` acepta JWT sin `exp` (no recomendado: no caducarían). `nbf` admite 1 min de desfase de reloj; un `iat` más de 5 min en el futuro se rechaza |
| `ASP_IDP_REQUIRED` | `0` | `1` exige JWT IdP en create/list/get/exec/destroy/events |
| `ASP_IDP_ROLE_CLAIM` | `groups` | Claim de grupos/roles para RBAC (fase 3) |
| `ASP_IDP_ROLE_MAP` | unset | CSV `claim:role` (admin\|operator\|user\|viewer); si set, gana sobre prefijo |
| `ASP_IDP_ROLE_PREFIX` | `asp-` | Prefijo → rol (`asp-admin`, …) cuando no hay map. Un grupo a secas (`admin`, `operator`) **no** concede rol: con prefijo o con `ASP_IDP_ROLE_MAP` solo cuenta lo que ellos nombran |
| `ASP_IDP_DESTROY_ANY_GROUP` | `sandbox:destroy-any` | Operator puede destroy no-propios si el claim lo incluye |
| `ASP_IDP_EXEC_ANY_GROUP` | `sandbox:exec-any` | Operator puede hacer exec en sandboxes no propias si el claim lo incluye; sin él, solo en las suyas |
| `ASP_BOOTSTRAP_API_KEY` | unset | Key `bootstrap`: ámbito `platform` (ve todos los tenants) en el tenant `default` |
| `ASP_BOOTSTRAP_API_KEY_SCOPE` / `_TENANT` | `platform` / `default` | `tenant` la confina a `_TENANT`, como cualquier otra key |
| `ASP_IDP_TENANT_CLAIM` | `tenant_id` | Claim del JWT con el tenant del usuario (string o array de un valor); sin tenant → 401 |
| `ASP_IDP_DEFAULT_TENANT` | unset | Tenant de los JWT sin ese claim (un solo tenant) |
| `ASP_DEFAULT_TENANT` | `default` | Tenant de un create sin `tenant_id` de un llamante sin tenant propio (lab abierto, key `platform`) |
| `ASP_NODE_BOOTSTRAP_TOKEN` | unset | Token para `/v1/nodes/enroll` |
| `ASP_CA_CERT` / `ASP_CA_KEY` | `/tmp/asp-dev-ca/ca.*` | CA de enrollment |
| `ASP_ALLOW_TMP_KEYS` | — | `1`: arranca en modo producción aunque alguna clave esté en un directorio temporal (ver abajo) |
| `ASP_TLS_CERT` / `ASP_TLS_KEY` | unset | TLS servidor |
| `ASP_CLIENT_CA` | unset | Client CA (register/heartbeat/oidc mint); habilita check de revocación y ata el CN del cert a cada ruta de nodo (403 si es otro nodo) |
| `ASP_SCHED_POLICY` | `spread` | `spread` o `binpack` ([`ops-multi-node.md`](../docs/ops-multi-node.md)) |
| `ASP_SCHED_CPU_OVERCOMMIT` | `4` | vCPU por core físico; la memoria no se sobresuscribe |
| `ASP_SCHED_VM_OVERHEAD_MIB` | `64` | Memoria que cuesta cada microVM además de su `memory_mib` (proceso del VMM, colas virtio); se suma a cada sandbox al comprobar y mostrar la memoria del nodo. Una sandbox pide como mínimo 64 MiB |
| `ASP_NODE_STALE_AFTER` | `90s` | Sin señales más tiempo → el nodo no recibe sandboxes y pasa a `offline` |
| `ASP_NODE_MONITOR_INTERVAL` | `15s` | Cada cuánto revisa el monitor la vida de los nodos |
| `ASP_NODE_FAILOVER_AFTER` | `5m` | Sin señales más tiempo → fencing y sandboxes del nodo → `failed` (`node_lost`); `0`/`off` desactiva (salvo revocados) |
| `ASP_AGENT_TOKEN_FILE` | `/var/lib/asp/agent.token`, o `$TMPDIR/asp-agent.token` | Secreto del API local del node-agent del mismo host (su `--agent-token-file`): el CP lo manda como bearer en cada `exec` a un agente `http://`. Se relee si cambia. Un agente `https://` (mTLS) no lo recibe. Si el CP no puede leerlo, el agente responde 401 y el `exec` da 502 diciéndolo |
| `ASP_WORKSPACE_ROOTS` | unset | Raíces de workspace del despliegue (`<raíz>/<tenant>/…`). Con ellas, un `workspace_host_path` fuera da 400 al crear en vez de una sandbox que no arranca. El nodo aplica su propia regla (`--workspace-root`) con los enlaces resueltos; sin esta variable solo decide el nodo |
| `ASP_INSECURE_AGENT_HTTP` | unset | `1` → permite `agent_endpoint` `http://` fuera de loopback (`exec` sin autenticar; solo lab). Por defecto: `http://` solo en loopback, `https://` con mTLS ([ADR-0011](../docs/adr/0011-multi-node.md)) |
| `ASP_MTLS_STRICT` | unset | `1` → `RequireAndVerifyClientCert` en listener TLS |
| `ASP_ENROLL_LISTEN` | `127.0.0.1:8081` | Plaintext enroll-only cuando `ASP_MTLS_STRICT=1` |
| `ASP_OIDC_KEY` | `/tmp/asp-oidc-key.pem` | PEM RSA de firma actual (auto-create; mint) |
| `ASP_OIDC_KEY_PREV` | unset | PEM RSA previa (solo JWKS durante rotación) |
| `ASP_OIDC_ISSUER` | `http://127.0.0.1$LISTEN_ADDR` | Issuer OIDC |
| `ASP_ATTEST_KEY` | `$TMPDIR/asp-attest-key.pem` | PEM ECDSA P-256 de atestación; su clave pública es de confianza (lab de un host: el node-agent usa el mismo fichero) |
| `ASP_ATTEST_PUB` | — | PEM de clave pública que sustituye a la de `ASP_ATTEST_KEY` para verificar |
| `ASP_ATTEST_TRUSTED_PUBS` | — | Bundle PEM (`PUBLIC KEY` y/o `CERTIFICATE`) de más claves de confianza. La clave que trae la evidencia (`public_key_pem`) nunca vale; por mTLS vale además la del certificado del nodo que llama |
| `ASP_ATTEST_MAX_AGE` | `10m` | Freshness para verify + claim OIDC |
| `ASP_FENCE_PROVIDER` | `noop` | `noop`\|`http_webhook`\|`redfish`\|`ipmi` |
| `ASP_FENCE_USER` | | Usuario Redfish/IPMI |
| `ASP_FENCE_PASS` | | Contraseña IPMI/Redfish por defecto si el nodo no tiene token. Un `ipmitool` la recibe por `IPMI_PASSWORD` |
| `ASP_EGRESS_DEFAULT_ALLOW` | `1` en memory-dev | Vacío = allow-all |
| `ASP_EGRESS_DENY_DEFAULT` | unset | Vacío = deny (harden; desactiva allow memory) |
| `ASP_AUTO_PROVISION` | unset/false | `1` = stub sync Create→running; default deja `requested` |
| `ASP_SANDBOX_IDLE_TIMEOUT` | unset = **off** | Parada por inactividad. Duración Go (`2h`, `1h`, `90m`). `0` / `off` / `false` / `disabled` desactiva. El valor recomendado de lab/producción es **2h** (también vale `1h`); no es el default del proceso, para que los smokes cortos no tumben sandboxes. Flag equivalente: `-idle-timeout` / `--idle-timeout` (pisa el env). |
| `ASP_STOPPED_SANDBOX_TTL` | `7d` | Una sandbox parada más de este tiempo se borra con su disco (`stop_reason=retention_expired`), contado desde `stopped_at`. `7d` o una duración Go (`48h`); `0`/`off` las conserva hasta que se borren ([ADR-0012](../docs/adr/0012-retained-disks.md)) |
| `ASP_MAX_STOPPED_PER_TENANT` | unset = sin tope | Al superarlo, se borran las sandboxes paradas más antiguas del tenant (`tenant_cap`), con un aviso en el log |
| `ASP_RETENTION_SWEEP` | `1m` | Cada cuánto corre el barrido de retención |
| `ASP_SANDBOX_IDLE_SWEEP` | `1m` | Cada cuánto el bucle del CP llama al reaper. No enciende el reaper por sí solo. Mínimo efectivo 1s. |


```bash
go test ./...
ASP_NODE_BOOTSTRAP_TOKEN=dev go run ./cmd/api
```


**Claves en directorios temporales.** Las rutas por defecto de la CA, la clave OIDC y la de atestación están en `/tmp` o `$TMPDIR`: un reinicio las borra, los certificados de nodo dejan de verificar y los tokens cambian de `kid`. En modo producción (`DATABASE_URL`, `ASP_TLS_CERT`, `ASP_CLIENT_CA` o `ASP_IDP_REQUIRED=1`) el control plane no arranca (código 2) si `ASP_CA_CERT`, `ASP_CA_KEY`, `ASP_OIDC_KEY` o `ASP_ATTEST_KEY` apuntan a `/tmp`, `/var/tmp`, `/dev/shm` o `$TMPDIR`, y el error nombra cada variable. Apúntalas a almacenamiento persistente (las que falten se crean ahí) o usa `ASP_ALLOW_TMP_KEYS=1`. En lab solo deja un aviso con las rutas.
## Logs y coste por petición

Cada petición deja una línea `request` con método, ruta, estado, bytes, duración y origen. Los sondeos de los nodos (`/work`, heartbeats, claim, status…) y `/healthz` van a nivel Debug: con varios nodos sondeando cada 2 s taparían el resto. El middleware de API keys cuenta las claves como mucho cada 10 s y escribe `last_used_at` como mucho una vez por minuto y clave, en vez de un `count(*)` y un `UPDATE` por petición.

## Parada por inactividad

**Por qué.** Una sesión `asp session` (o un create olvidado) deja la microVM encendida hasta que alguien la pare. El reaper del control-plane marca `stopping` (o `stopped` si nunca se asignó nodo) cuando no hay actividad durante el umbral, y el reconciler del nodo apaga la VM **conservando su disco**: es una parada, no un borrado, y `asp session resume` la trae de vuelta.

**Qué cuenta como actividad.** `last_activity_at` se mueve en: create, transición a `running` (start) y **exec con respuesta correcta del node-agent** (aunque el proceso del guest salga ≠ 0). No cuentan: GET, heartbeat, sondeos de `/work`, ni un exec que ni siquiera llega al guest.

**Cómo se configura.** Default del binario: apagado. En lab/producción:

```bash
ASP_SANDBOX_IDLE_TIMEOUT=2h   # recomendado; alternativa 1h
# o al arrancar: ./api -idle-timeout 2h
# ASP_SANDBOX_IDLE_TIMEOUT=0  o  off  → desactiva
```

La unit `scripts/systemd/asp-control-plane.service` fija `2h`. Los tests y smokes cortos no exportan la variable.

**Límites.** El reaper no es un sustituto de `asp session rm`: el fichero local de sesión sigue apuntando al id; `asp session status` y `exec` lo dicen (`idle timeout` / `idle_reaped`, con el disco conservado) y hay que `asp session resume`. Una sandbox parada fija su disco y su nodo hasta que se borre. La migración `008` rellena `last_activity_at` de filas viejas con `now()`, así que al activar el reaper no se destruye de golpe todo lo creado hace horas; el reloj de esas filas empieza en la migración. Evento de auditoría: `sandbox.idle_reaped` (`stop_reason=idle_timeout`).

## Retención de sandboxes paradas

Parar una sandbox (`POST …/stop`, `asp session stop`, el reaper de inactividad) **conserva su disco** en el nodo ([ADR-0012](../docs/adr/0012-retained-disks.md)); `DELETE` lo borra. Dos límites acotan lo que se acumula, y los aplica un barrido cada `ASP_RETENTION_SWEEP`:

- **TTL** (`ASP_STOPPED_SANDBOX_TTL`, por defecto 7 días): lo parado hace más que eso se borra (`deleting` para que el nodo quite el disco, `deleted` si ningún nodo tiene nada). Evento `sandbox.deleted` con `reason` y el TTL.
- **Tope por tenant** (`ASP_MAX_STOPPED_PER_TENANT`): se borran las más antiguas de cada tenant que pasen del tope, con `reason=tenant_cap` y un aviso en el log: el disco de un usuario se va para hacer sitio.

**Postgres es requisito.** Con el store en memoria, reiniciar el plano de control olvida las sandboxes paradas y el GC de cada nodo borra sus discos; el plano de control lo avisa al arrancar si hay retención activa. `asp node list` muestra, por nodo, cuántas paradas guardan un disco (`STOPPED (DISKS)`) y el espacio libre que el nodo informa en su heartbeat (`DISK FREE`, de `--disk-dir`).

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
