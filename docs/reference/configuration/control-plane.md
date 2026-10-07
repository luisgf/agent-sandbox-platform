<!-- Generado por `make docs` a partir de settingsTable (control-plane/cmd/api/settingstable.go). No lo edites a mano: un test falla si no coincide con el código. -->
# Configuración del plano de control

Cada ajuste es una variable de entorno `ASP_*`. Prioridad: **variable de entorno > fichero de configuración > valor por defecto**. El fichero es `/etc/asp/server.yaml` (más los `server.yaml.d/*.yaml` que lo acompañan), o el que nombren `--config FILE` o `ASP_CONFIG`; la clave de un ajuste es su variable sin `ASP_` y en minúsculas ([el fichero de configuración](../../how-to/config-file.md)). `asp-control-plane --print-config` lista cada ajuste con su valor y de dónde sale, sin las credenciales; las marcadas con 🔒 son credenciales y nunca se imprimen.

Los ajustes de los otros programas: [node-agent](node-agent.md), [`asp`](cli.md) y [`asp-server`](asp-server.md).

## Escucha y parada

| Variable | Clave del fichero | Por defecto | Qué hace |
|---|---|---|---|
| `ASP_LISTEN_ADDR` | `listen_addr` | `127.0.0.1:8080` | Dirección `host:puerto` en la que escucha el API |
| `ASP_SHUTDOWN_TIMEOUT` | `shutdown_timeout` | `30s` | Al recibir SIGTERM/SIGINT el API deja de aceptar conexiones y espera hasta este tiempo a las peticiones en curso (los exec en streaming incluidos); después cierra las que queden y lo registra con su número. Los bucles de fondo (idle reaper, monitor de nodos, refresco del JWKS) paran y el pool de Postgres se cierra al final. |
| `ASP_BUFFERED_EXEC_TIMEOUT` | `buffered_exec_timeout` | `10m` | Cuánto puede tardar un `exec` sin `?stream=1` (`asp sandbox exec/run`, `session exec --buffered`), también en el guest. Una petición puede pedir menos con `timeout_seconds`. `0`/`off` sin límite. Pasado el límite, 504. Un stream no tiene límite |
| `ASP_METRICS_LISTEN` | `metrics_listen` | — | `host:puerto` de un listener aparte con `GET /metrics` (Prometheus). Sin autenticación: solo loopback salvo `ASP_INSECURE_OBS_LISTEN=1`. Además, `GET /metrics` en el puerto del API sirve lo mismo a una clave de plataforma o a un admin/operator del IdP. Catálogo: [`docs/how-to/monitoring.md`](../../how-to/monitoring.md) |
| `ASP_PPROF_LISTEN` | `pprof_listen` | — | `host:puerto` de un listener aparte con los perfiles de Go (`/debug/pprof/`). Sin autenticación: solo loopback salvo `ASP_INSECURE_OBS_LISTEN=1` |
| `ASP_INSECURE_OBS_LISTEN` | `insecure_obs_listen` | — | `1` deja que `ASP_METRICS_LISTEN` / `ASP_PPROF_LISTEN` escuchen fuera de loopback (sin autenticación) |

## Base de datos

| Variable | Clave del fichero | Por defecto | Qué hace |
|---|---|---|---|
| `ASP_DATABASE_URL` 🔒 | `database_url` | — | Si está set → PostgresStore + migraciones embebidas. `sqlite:///var/lib/asp/server/asp.db` (o `sqlite:ruta.db`) guarda el estado en un fichero de este host: sin servidor de base de datos, para un único plano de control. El fichero se crea con modo 0600 (guarda hashes de claves y tokens de fence) en un directorio 0700; para copiarlo en caliente, `sqlite3 asp.db ".backup copia.db"` |
| `ASP_DB_STATEMENT_TIMEOUT` | `db_statement_timeout` | `30s` | `statement_timeout` de cada conexión del pool: una sentencia que tarda más se cancela en el servidor (`57014`). Una migración lo desactiva para sí misma. `0`/`off` lo quita. Un `?statement_timeout=…` en `ASP_DATABASE_URL` manda sobre este valor. |
| `ASP_DB_LOCK_TIMEOUT` | `db_lock_timeout` | `10s` | `lock_timeout`: cuánto espera una sentencia a un bloqueo (incluido el advisory lock de la colocación) antes de fallar con `55P03`. |
| `ASP_DB_IDLE_TX_TIMEOUT` | `db_idle_tx_timeout` | `60s` | `idle_in_transaction_session_timeout`: una transacción abierta que nadie usa (un handler que murió a medias) se cierra y libera su conexión y sus bloqueos. |

## Autenticación y autorización

| Variable | Clave del fichero | Por defecto | Qué hace |
|---|---|---|---|
| `ASP_INSECURE_OPEN_API` | `insecure_open_api` | — | `1` acepta peticiones sin credencial (solo labs y smokes dry-run; avisa al arrancar). Una credencial incorrecta se rechaza igual. `ASP_REQUIRE_API_KEY` ya no hace nada: la autenticación está siempre activa |
| `ASP_BOOTSTRAP_API_KEY` 🔒 | `bootstrap_api_key` | — | Key `bootstrap`: ámbito `platform` (ve todos los tenants) en el tenant `default`. Es la primera key: sin ninguna key en el store ni IdP configurado el CP **no arranca** (código 2). Con ella se crean las demás (`asp apikey create`, `POST /v1/api-keys`) y conviene rotarla o revocarla después. Sigue siendo `platform` por defecto porque con ámbito `tenant` no podría crear la key de plataforma de los nodos |
| `ASP_BOOTSTRAP_API_KEY_SCOPE` | `bootstrap_api_key_scope` | `platform` | Ámbito de la key `bootstrap`: `platform` (ve todos los tenants) o `tenant`, que la confina a `ASP_BOOTSTRAP_API_KEY_TENANT` como a cualquier otra key |
| `ASP_BOOTSTRAP_API_KEY_TENANT` | `bootstrap_api_key_tenant` | `default` | Tenant de la key `bootstrap` |
| `ASP_DEFAULT_TENANT` | `default_tenant` | `default` | Tenant de un create sin `tenant_id` de un llamante sin tenant propio (lab abierto, key `platform`) |
| `ASP_REQUIRE_API_KEY` | `require_api_key` | — | Ya no hace nada: la autenticación está siempre activa. Se acepta, con un aviso, para que una unidad antigua que la fija siga arrancando |
| `ASP_IDP_ISSUER` | `idp_issuer` | — | Issuer OIDC corporativo; vacío = IdP off (lab) |
| `ASP_IDP_AUDIENCE` | `idp_audience` | — | Audiencia esperada del JWT (`aud`). **Obligatoria con `ASP_IDP_REQUIRED=1`**: sin ella el control plane no arranca (código 2), porque aceptaría el token que el emisor haya dado a cualquier otra aplicación de su realm. Con el IdP opcional solo deja un aviso |
| `ASP_IDP_ALLOW_ANY_AUDIENCE` | `idp_allow_any_audience` | — | `1` permite `ASP_IDP_REQUIRED=1` sin `ASP_IDP_AUDIENCE` (solo lab; avisa en el log) |
| `ASP_IDP_JWKS_URL` | `idp_jwks_url` | — | JWKS; si vacío → discovery desde issuer. Se refresca cada 5 min (una clave que el IdP retira deja de validar) y, ante un `kid` desconocido, como mucho una vez cada 30 s |
| `ASP_IDP_REQUIRE_EXP` | `idp_require_exp` | `1` | `0` acepta JWT sin `exp` (no recomendado: no caducarían). `nbf` admite 1 min de desfase de reloj; un `iat` más de 5 min en el futuro se rechaza |
| `ASP_IDP_REQUIRED` | `idp_required` | `0` | `1` exige JWT IdP en create/list/get/exec/destroy/events |
| `ASP_IDP_ROLE_CLAIM` | `idp_role_claim` | `groups` | Claim del JWT con los grupos o roles que dan el rol RBAC |
| `ASP_IDP_ROLE_MAP` | `idp_role_map` | — | CSV `claim:role` (admin\|operator\|user\|viewer); si set, gana sobre prefijo |
| `ASP_IDP_ROLE_PREFIX` | `idp_role_prefix` | `asp-` | Prefijo → rol (`asp-admin`, …) cuando no hay map. Un grupo a secas (`admin`, `operator`) **no** concede rol: con prefijo o con `ASP_IDP_ROLE_MAP` solo cuenta lo que ellos nombran |
| `ASP_IDP_DESTROY_ANY_GROUP` | `idp_destroy_any_group` | `sandbox:destroy-any` | Operator puede destroy no-propios si el claim lo incluye |
| `ASP_IDP_EXEC_ANY_GROUP` | `idp_exec_any_group` | `sandbox:exec-any` | Operator puede hacer exec en sandboxes no propias si el claim lo incluye; sin él, solo en las suyas |
| `ASP_IDP_TENANT_CLAIM` | `idp_tenant_claim` | `tenant_id` | Claim del JWT con el tenant del usuario (string o array de un valor); sin tenant → 401 |
| `ASP_IDP_DEFAULT_TENANT` | `idp_default_tenant` | — | Tenant de los JWT sin ese claim (un solo tenant) |

## Nodos, CA y TLS

| Variable | Clave del fichero | Por defecto | Qué hace |
|---|---|---|---|
| `ASP_NODE_BOOTSTRAP_TOKEN` 🔒 | `node_bootstrap_token` | — | Token para `/v1/nodes/enroll` |
| `ASP_CA_CERT` | `ca_cert` | `/tmp/asp-dev-ca/ca.crt` | Certificado de la CA de enrollment, la que firma los certificados de nodo. Se crea si no existe. En producción, en almacenamiento persistente (ver `ASP_ALLOW_TMP_KEYS`) |
| `ASP_CA_KEY` | `ca_key` | `/tmp/asp-dev-ca/ca.key` | Clave privada de esa CA. Se crea junto al certificado si no existe |
| `ASP_ALLOW_TMP_KEYS` | `allow_tmp_keys` | — | `1`: arranca en modo producción aunque `ASP_CA_CERT`, `ASP_CA_KEY`, `ASP_OIDC_KEY` o `ASP_ATTEST_KEY` apunten a un directorio temporal (`/tmp`, `/var/tmp`, `/dev/shm`, `$TMPDIR`), que un reinicio vacía: los certificados de nodo dejarían de verificar y los tokens cambiarían de `kid`. Es modo producción tener `ASP_DATABASE_URL`, `ASP_TLS_CERT`, `ASP_CLIENT_CA` o `ASP_IDP_REQUIRED=1`; ahí, sin esta variable, el control plane no arranca (código 2) y el error nombra cada variable. En un lab solo avisa |
| `ASP_TLS_CERT` | `tls_cert` | — | Certificado TLS del servidor. Sus nombres (DNS, IP, CN, comodines) quedan reservados: ningún nodo puede enrolarse con uno de ellos como id ([dos raíces de confianza](../../ops-multi-node.md#las-dos-raíces-de-confianza)) |
| `ASP_TLS_KEY` | `tls_key` | — | Clave privada del certificado de `ASP_TLS_CERT` |
| `ASP_CLIENT_CA` | `client_ca` | — | Client CA (register/heartbeat/oidc mint); habilita check de revocación y ata el CN del cert a cada ruta de nodo (403 si es otro nodo) |
| `ASP_MTLS_STRICT` | `mtls_strict` | — | `1` → `RequireAndVerifyClientCert` en listener TLS |
| `ASP_ENROLL_LISTEN` | `enroll_listen` | `127.0.0.1:8081` | Plaintext enroll-only cuando `ASP_MTLS_STRICT=1` |
| `ASP_AGENT_TOKEN_FILE` | `agent_token_file` | `/var/lib/asp/agent.token` | Secreto del API local del node-agent del mismo host (su `--agent-token-file`): el CP lo manda como bearer en cada `exec` a un agente `http://`. Se relee si cambia. Un agente `https://` (mTLS) no lo recibe. Si el CP no puede leerlo, el agente responde 401 y el `exec` da 502 diciéndolo |
| `ASP_INSECURE_AGENT_HTTP` | `insecure_agent_http` | — | `1` → permite `agent_endpoint` `http://` fuera de loopback (`exec` sin autenticar; solo lab). Por defecto: `http://` solo en loopback, `https://` con mTLS ([ADR-0011](../../adr/0011-multi-node.md)) |
| `ASP_WORKSPACE_ROOTS` | `workspace_roots` | — | Raíces de workspace del despliegue (`<raíz>/<tenant>/…`). Con ellas, un `workspace_host_path` fuera da 400 al crear en vez de una sandbox que no arranca. El nodo aplica su propia regla (`--workspace-root`) con los enlaces resueltos; sin esta variable solo decide el nodo |
| `ASP_LOCAL_NET_DIAL` | `local_net_dial` | — | `host[:puerto]` que marcan los portátiles para llegar a los túneles local-net de un nodo cuando el nodo no dice otro (`--local-net-dial` del node-agent) |

## Planificación y vigilancia de los nodos

| Variable | Clave del fichero | Por defecto | Qué hace |
|---|---|---|---|
| `ASP_SCHED_POLICY` | `sched_policy` | `spread` | `spread` (el nodo menos cargado) o `binpack` (el más cargado que aún cabe), ver [varios nodos](../../ops-multi-node.md) |
| `ASP_SCHED_CPU_OVERCOMMIT` | `sched_cpu_overcommit` | `4` | vCPU por core físico; la memoria no se sobresuscribe |
| `ASP_SCHED_VM_OVERHEAD_MIB` | `sched_vm_overhead_mib` | `64` | Memoria que cuesta cada microVM además de su `memory_mib` (proceso del VMM, colas virtio); se suma a cada sandbox al comprobar y mostrar la memoria del nodo. Una sandbox pide como mínimo 64 MiB |
| `ASP_NODE_STALE_AFTER` | `node_stale_after` | `90s` | Sin señales más tiempo → el nodo no recibe sandboxes y pasa a `offline` |
| `ASP_NODE_MONITOR_INTERVAL` | `node_monitor_interval` | `15s` | Cada cuánto revisa el monitor la vida de los nodos |
| `ASP_NODE_FAILOVER_AFTER` | `node_failover_after` | `5m` | Sin señales más tiempo → fencing y sandboxes del nodo → `failed` (`node_lost`); `0`/`off` desactiva (salvo revocados) |
| `ASP_AUTO_PROVISION` | `auto_provision` | — | `1`: el create deja la sandbox en `running` en el acto, sin nodo (un stub para pruebas). Por defecto queda en `requested` hasta que un nodo la reclama |

## Claves de identidad, atestación y fencing

| Variable | Clave del fichero | Por defecto | Qué hace |
|---|---|---|---|
| `ASP_OIDC_KEY` | `oidc_key` | `/tmp/asp-oidc-key.pem` | Clave RSA (PEM) con la que el control plane firma los tokens OIDC que emite a las sandboxes (`mint`). Se crea si no existe. En producción, en almacenamiento persistente |
| `ASP_OIDC_KEY_PREV` | `oidc_key_prev` | — | Clave RSA (PEM) anterior: solo se publica en el JWKS durante una rotación, para que los tokens ya emitidos sigan verificando |
| `ASP_OIDC_ISSUER` | `oidc_issuer` | http://<dirección de escucha> | Issuer OIDC. Si otros hosts verifican los tokens contra este control plane, ponla con la URL con la que llegan a él |
| `ASP_ATTEST_KEY` | `attest_key` | `$TMPDIR/asp-attest-key.pem` | PEM ECDSA P-256 de atestación; su clave pública es de confianza (lab de un host: el node-agent usa el mismo fichero) |
| `ASP_ATTEST_PUB` | `attest_pub` | — | PEM de clave pública que sustituye a la de `ASP_ATTEST_KEY` para verificar |
| `ASP_ATTEST_TRUSTED_PUBS` | `attest_trusted_pubs` | — | Bundle PEM (`PUBLIC KEY` y/o `CERTIFICATE`) de más claves de confianza. La clave que trae la evidencia (`public_key_pem`) nunca vale; por mTLS vale además la del certificado del nodo que llama |
| `ASP_ATTEST_MAX_AGE` | `attest_max_age` | `10m` | Freshness para verify + claim OIDC |
| `ASP_ATTEST_ALLOWED_IMAGES` | `attest_allowed_images` | — | JSON `{"images":[{"name","kernel","rootfs","vmm"}]}` con las imágenes (SHA-256 del kernel y de la imagen base, y opcionalmente la versión del hipervisor) para las que se acepta evidencia de arranque. Con él, una evidencia de otra imagen o sin digests se rechaza (400) y no genera claim; se relee al cambiar el fichero. Sin él se guardan los digests que declare el nodo. `node-agent --print-measurement` imprime la entrada de un nodo |
| `ASP_FENCE_PROVIDER` | `fence_provider` | `noop` | `noop`\|`http_webhook`\|`redfish`\|`ipmi` |
| `ASP_FENCE_USER` | `fence_user` | — | Usuario Redfish/IPMI |
| `ASP_FENCE_PASS` 🔒 | `fence_pass` | — | Contraseña IPMI/Redfish por defecto si el nodo no tiene token. Un `ipmitool` la recibe por `IPMI_PASSWORD` |

## Salida a Internet, inactividad y retención

| Variable | Clave del fichero | Por defecto | Qué hace |
|---|---|---|---|
| `ASP_EGRESS_DEFAULT_ALLOW` | `egress_default_allow` | 1 con el store en memoria, 0 con Postgres | Qué puede alcanzar una sandbox cuyo tenant no tiene reglas: `1` todo, `0` nada. `ASP_EGRESS_DENY_DEFAULT` era este mismo ajuste con el sentido contrario; sigue valiendo si este no está, con un aviso |
| `ASP_EGRESS_DENY_DEFAULT` | `egress_deny_default` | — | El nombre antiguo de `ASP_EGRESS_DEFAULT_ALLOW`, con el sentido contrario (`1` deniega) |
| `ASP_SANDBOX_IDLE_TIMEOUT` | `sandbox_idle_timeout` | `off` | Parada por inactividad. Duración Go (`2h`, `1h`, `90m`). `0` / `off` / `false` / `disabled` desactiva. El valor recomendado de lab/producción es **2h** (también vale `1h`); no es el default del proceso, para que los smokes cortos no tumben sandboxes. Flag equivalente: `-idle-timeout` / `--idle-timeout` (pisa el env). |
| `ASP_SANDBOX_IDLE_SWEEP` | `sandbox_idle_sweep` | `1m` | Cada cuánto el bucle del CP llama al reaper. No enciende el reaper por sí solo. Mínimo efectivo 1s. |
| `ASP_STOPPED_SANDBOX_TTL` | `stopped_sandbox_ttl` | `7d` | Una sandbox parada más de este tiempo se borra con su disco (`stop_reason=retention_expired`), contado desde `stopped_at`. `7d` o una duración Go (`48h`); `0`/`off` las conserva hasta que se borren ([ADR-0012](../../adr/0012-retained-disks.md)) |
| `ASP_MAX_STOPPED_PER_TENANT` | `max_stopped_per_tenant` | — | Al superarlo, se borran las sandboxes paradas más antiguas del tenant (`tenant_cap`), con un aviso en el log |
| `ASP_RETENTION_SWEEP` | `retention_sweep` | `1m` | Cada cuánto corre el barrido de retención |
