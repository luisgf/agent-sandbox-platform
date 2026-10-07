package main

// setting describes one variable the control plane reads: what it is for, what it is when
// nothing sets it, and whether it is a credential. The configuration file may set exactly
// these (its key is the name without ASP_, in lower case), and --print-config lists them.
type setting struct {
	// Group is the heading the reference page lists it under.
	Group   string
	Env     string
	Default string // documentation: where it is not obvious from the code, in words
	Help    string
	Secret  bool
}

// The headings of the reference page (docs/reference/configuration/control-plane.md), in order.
const (
	groupListen     = "Escucha y parada"
	groupDatabase   = "Base de datos"
	groupAuth       = "Autenticación y autorización"
	groupNodes      = "Nodos, CA y TLS"
	groupScheduling = "Planificación y vigilancia de los nodos"
	groupKeys       = "Claves de identidad, atestación y fencing"
	groupRetention  = "Salida a Internet, inactividad y retención"
)

// settingsTable is every variable the control plane reads. A test fails when the source reads
// one that is not here, or this lists one the source does not read.
var settingsTable = []setting{
	// Where it listens, and how it stops.
	{Group: groupListen, Env: "ASP_LISTEN_ADDR", Default: "127.0.0.1:8080", Help: "Dirección `host:puerto` en la que escucha el API"},
	{Group: groupListen, Env: "ASP_SHUTDOWN_TIMEOUT", Default: "30s", Help: "Al recibir SIGTERM/SIGINT el API deja de aceptar conexiones y espera hasta este tiempo a las peticiones en curso (los exec en streaming incluidos); después cierra las que queden y lo registra con su número. Los bucles de fondo (idle reaper, monitor de nodos, refresco del JWKS) paran y el pool de Postgres se cierra al final."},
	{Group: groupListen, Env: "ASP_BUFFERED_EXEC_TIMEOUT", Default: "10m", Help: "Cuánto puede tardar un `exec` sin `?stream=1` (`asp sandbox exec/run`, `session exec --buffered`), también en el guest. Una petición puede pedir menos con `timeout_seconds`. `0`/`off` sin límite. Pasado el límite, 504. Un stream no tiene límite"},
	{Group: groupListen, Env: "ASP_METRICS_LISTEN", Help: "`host:puerto` de un listener aparte con `GET /metrics` (Prometheus). Sin autenticación: solo loopback salvo `ASP_INSECURE_OBS_LISTEN=1`. Además, `GET /metrics` en el puerto del API sirve lo mismo a una clave de plataforma o a un admin/operator del IdP. Catálogo: [`docs/how-to/monitoring.md`](docs/how-to/monitoring.md)"},
	{Group: groupListen, Env: "ASP_PPROF_LISTEN", Help: "`host:puerto` de un listener aparte con los perfiles de Go (`/debug/pprof/`). Sin autenticación: solo loopback salvo `ASP_INSECURE_OBS_LISTEN=1`"},
	{Group: groupListen, Env: "ASP_INSECURE_OBS_LISTEN", Help: "`1` deja que `ASP_METRICS_LISTEN` / `ASP_PPROF_LISTEN` escuchen fuera de loopback (sin autenticación)"},

	// The database.
	{Group: groupDatabase, Env: "ASP_DATABASE_URL", Help: "Si está set → PostgresStore + migraciones embebidas. `sqlite:///var/lib/asp/server/asp.db` (o `sqlite:ruta.db`) guarda el estado en un fichero de este host: sin servidor de base de datos, para un único plano de control. El fichero se crea con modo 0600 (guarda hashes de claves y tokens de fence) en un directorio 0700; para copiarlo en caliente, `sqlite3 asp.db \".backup copia.db\"`", Secret: true},
	{Group: groupDatabase, Env: "ASP_DB_STATEMENT_TIMEOUT", Default: "30s", Help: "`statement_timeout` de cada conexión del pool: una sentencia que tarda más se cancela en el servidor (`57014`). Una migración lo desactiva para sí misma. `0`/`off` lo quita. Un `?statement_timeout=…` en `ASP_DATABASE_URL` manda sobre este valor."},
	{Group: groupDatabase, Env: "ASP_DB_LOCK_TIMEOUT", Default: "10s", Help: "`lock_timeout`: cuánto espera una sentencia a un bloqueo (incluido el advisory lock de la colocación) antes de fallar con `55P03`."},
	{Group: groupDatabase, Env: "ASP_DB_IDLE_TX_TIMEOUT", Default: "60s", Help: "`idle_in_transaction_session_timeout`: una transacción abierta que nadie usa (un handler que murió a medias) se cierra y libera su conexión y sus bloqueos."},

	// Who may call it.
	{Group: groupAuth, Env: "ASP_INSECURE_OPEN_API", Help: "`1` acepta peticiones sin credencial (solo labs y smokes dry-run; avisa al arrancar). Una credencial incorrecta se rechaza igual. `ASP_REQUIRE_API_KEY` ya no hace nada: la autenticación está siempre activa"},
	{Group: groupAuth, Env: "ASP_BOOTSTRAP_API_KEY", Help: "Key `bootstrap`: ámbito `platform` (ve todos los tenants) en el tenant `default`. Es la primera key: sin ninguna key en el store ni IdP configurado el CP **no arranca** (código 2). Con ella se crean las demás (`asp apikey create`, `POST /v1/api-keys`) y conviene rotarla o revocarla después. Sigue siendo `platform` por defecto porque con ámbito `tenant` no podría crear la key de plataforma de los nodos", Secret: true},
	{Group: groupAuth, Env: "ASP_BOOTSTRAP_API_KEY_SCOPE", Default: "platform", Help: "Ámbito de la key `bootstrap`: `platform` (ve todos los tenants) o `tenant`, que la confina a `ASP_BOOTSTRAP_API_KEY_TENANT` como a cualquier otra key"},
	{Group: groupAuth, Env: "ASP_BOOTSTRAP_API_KEY_TENANT", Default: "default", Help: "Tenant de la key `bootstrap`"},
	{Group: groupAuth, Env: "ASP_DEFAULT_TENANT", Default: "default", Help: "Tenant de un create sin `tenant_id` de un llamante sin tenant propio (lab abierto, key `platform`)"},
	{Group: groupAuth, Env: "ASP_REQUIRE_API_KEY", Help: "Ya no hace nada: la autenticación está siempre activa. Se acepta, con un aviso, para que una unidad antigua que la fija siga arrancando"},
	{Group: groupAuth, Env: "ASP_IDP_ISSUER", Help: "Issuer OIDC corporativo; vacío = IdP off (lab)"},
	{Group: groupAuth, Env: "ASP_IDP_AUDIENCE", Help: "Audiencia esperada del JWT (`aud`). **Obligatoria con `ASP_IDP_REQUIRED=1`**: sin ella el control plane no arranca (código 2), porque aceptaría el token que el emisor haya dado a cualquier otra aplicación de su realm. Con el IdP opcional solo deja un aviso"},
	{Group: groupAuth, Env: "ASP_IDP_ALLOW_ANY_AUDIENCE", Help: "`1` permite `ASP_IDP_REQUIRED=1` sin `ASP_IDP_AUDIENCE` (solo lab; avisa en el log)"},
	{Group: groupAuth, Env: "ASP_IDP_JWKS_URL", Help: "JWKS; si vacío → discovery desde issuer. Se refresca cada 5 min (una clave que el IdP retira deja de validar) y, ante un `kid` desconocido, como mucho una vez cada 30 s"},
	{Group: groupAuth, Env: "ASP_IDP_REQUIRE_EXP", Default: "1", Help: "`0` acepta JWT sin `exp` (no recomendado: no caducarían). `nbf` admite 1 min de desfase de reloj; un `iat` más de 5 min en el futuro se rechaza"},
	{Group: groupAuth, Env: "ASP_IDP_REQUIRED", Default: "0", Help: "`1` exige JWT IdP en create/list/get/exec/destroy/events"},
	{Group: groupAuth, Env: "ASP_IDP_ROLE_CLAIM", Default: "groups", Help: "Claim del JWT con los grupos o roles que dan el rol RBAC"},
	{Group: groupAuth, Env: "ASP_IDP_ROLE_MAP", Help: "CSV `claim:role` (admin|operator|user|viewer); si set, gana sobre prefijo"},
	{Group: groupAuth, Env: "ASP_IDP_ROLE_PREFIX", Default: "asp-", Help: "Prefijo → rol (`asp-admin`, …) cuando no hay map. Un grupo a secas (`admin`, `operator`) **no** concede rol: con prefijo o con `ASP_IDP_ROLE_MAP` solo cuenta lo que ellos nombran"},
	{Group: groupAuth, Env: "ASP_IDP_DESTROY_ANY_GROUP", Default: "sandbox:destroy-any", Help: "Operator puede destroy no-propios si el claim lo incluye"},
	{Group: groupAuth, Env: "ASP_IDP_EXEC_ANY_GROUP", Default: "sandbox:exec-any", Help: "Operator puede hacer exec en sandboxes no propias si el claim lo incluye; sin él, solo en las suyas"},
	{Group: groupAuth, Env: "ASP_IDP_TENANT_CLAIM", Default: "tenant_id", Help: "Claim del JWT con el tenant del usuario (string o array de un valor); sin tenant → 401"},
	{Group: groupAuth, Env: "ASP_IDP_DEFAULT_TENANT", Help: "Tenant de los JWT sin ese claim (un solo tenant)"},

	// Nodes.
	{Group: groupNodes, Env: "ASP_NODE_BOOTSTRAP_TOKEN", Help: "Token para `/v1/nodes/enroll`", Secret: true},
	{Group: groupNodes, Env: "ASP_CA_CERT", Default: "/tmp/asp-dev-ca/ca.crt", Help: "Certificado de la CA de enrollment, la que firma los certificados de nodo. Se crea si no existe. En producción, en almacenamiento persistente (ver `ASP_ALLOW_TMP_KEYS`)"},
	{Group: groupNodes, Env: "ASP_CA_KEY", Default: "/tmp/asp-dev-ca/ca.key", Help: "Clave privada de esa CA. Se crea junto al certificado si no existe"},
	{Group: groupNodes, Env: "ASP_ALLOW_TMP_KEYS", Help: "`1`: arranca en modo producción aunque `ASP_CA_CERT`, `ASP_CA_KEY`, `ASP_OIDC_KEY` o `ASP_ATTEST_KEY` apunten a un directorio temporal (`/tmp`, `/var/tmp`, `/dev/shm`, `$TMPDIR`), que un reinicio vacía: los certificados de nodo dejarían de verificar y los tokens cambiarían de `kid`. Es modo producción tener `ASP_DATABASE_URL`, `ASP_TLS_CERT`, `ASP_CLIENT_CA` o `ASP_IDP_REQUIRED=1`; ahí, sin esta variable, el control plane no arranca (código 2) y el error nombra cada variable. En un lab solo avisa"},
	{Group: groupNodes, Env: "ASP_TLS_CERT", Help: "Certificado TLS del servidor. Sus nombres (DNS, IP, CN, comodines) quedan reservados: ningún nodo puede enrolarse con uno de ellos como id ([dos raíces de confianza](docs/ops-multi-node.md#las-dos-raíces-de-confianza))"},
	{Group: groupNodes, Env: "ASP_TLS_KEY", Help: "Clave privada del certificado de `ASP_TLS_CERT`"},
	{Group: groupNodes, Env: "ASP_CLIENT_CA", Help: "Client CA (register/heartbeat/oidc mint); habilita check de revocación y ata el CN del cert a cada ruta de nodo (403 si es otro nodo)"},
	{Group: groupNodes, Env: "ASP_MTLS_STRICT", Help: "`1` → `RequireAndVerifyClientCert` en listener TLS"},
	{Group: groupNodes, Env: "ASP_ENROLL_LISTEN", Default: "127.0.0.1:8081", Help: "Plaintext enroll-only cuando `ASP_MTLS_STRICT=1`"},
	{Group: groupNodes, Env: "ASP_AGENT_TOKEN_FILE", Default: "/var/lib/asp/agent.token", Help: "Secreto del API local del node-agent del mismo host (su `--agent-token-file`): el CP lo manda como bearer en cada `exec` a un agente `http://`. Se relee si cambia. Un agente `https://` (mTLS) no lo recibe. Si el CP no puede leerlo, el agente responde 401 y el `exec` da 502 diciéndolo"},
	{Group: groupNodes, Env: "ASP_INSECURE_AGENT_HTTP", Help: "`1` → permite `agent_endpoint` `http://` fuera de loopback (`exec` sin autenticar; solo lab). Por defecto: `http://` solo en loopback, `https://` con mTLS ([ADR-0011](docs/adr/0011-multi-node.md))"},
	{Group: groupNodes, Env: "ASP_WORKSPACE_ROOTS", Help: "Raíces de workspace del despliegue (`<raíz>/<tenant>/…`). Con ellas, un `workspace_host_path` fuera da 400 al crear en vez de una sandbox que no arranca. El nodo aplica su propia regla (`--workspace-root`) con los enlaces resueltos; sin esta variable solo decide el nodo"},
	{Group: groupNodes, Env: "ASP_LOCAL_NET_DIAL", Help: "`host[:puerto]` que marcan los portátiles para llegar a los túneles local-net de un nodo cuando el nodo no dice otro (`--local-net-dial` del node-agent)"},

	// Scheduling and liveness.
	{Group: groupScheduling, Env: "ASP_SCHED_POLICY", Default: "spread", Help: "`spread` (el nodo menos cargado) o `binpack` (el más cargado que aún cabe), ver [varios nodos](docs/ops-multi-node.md)"},
	{Group: groupScheduling, Env: "ASP_SCHED_CPU_OVERCOMMIT", Default: "4", Help: "vCPU por core físico; la memoria no se sobresuscribe"},
	{Group: groupScheduling, Env: "ASP_SCHED_VM_OVERHEAD_MIB", Default: "64", Help: "Memoria que cuesta cada microVM además de su `memory_mib` (proceso del VMM, colas virtio); se suma a cada sandbox al comprobar y mostrar la memoria del nodo. Una sandbox pide como mínimo 64 MiB"},
	{Group: groupScheduling, Env: "ASP_NODE_STALE_AFTER", Default: "90s", Help: "Sin señales más tiempo → el nodo no recibe sandboxes y pasa a `offline`"},
	{Group: groupScheduling, Env: "ASP_NODE_MONITOR_INTERVAL", Default: "15s", Help: "Cada cuánto revisa el monitor la vida de los nodos"},
	{Group: groupScheduling, Env: "ASP_NODE_FAILOVER_AFTER", Default: "5m", Help: "Sin señales más tiempo → fencing y sandboxes del nodo → `failed` (`node_lost`); `0`/`off` desactiva (salvo revocados)"},
	{Group: groupScheduling, Env: "ASP_AUTO_PROVISION", Help: "`1`: el create deja la sandbox en `running` en el acto, sin nodo (un stub para pruebas). Por defecto queda en `requested` hasta que un nodo la reclama"},

	// Keys of the identity and attestation features.
	{Group: groupKeys, Env: "ASP_OIDC_KEY", Default: "/tmp/asp-oidc-key.pem", Help: "Clave RSA (PEM) con la que el control plane firma los tokens OIDC que emite a las sandboxes (`mint`). Se crea si no existe. En producción, en almacenamiento persistente"},
	{Group: groupKeys, Env: "ASP_OIDC_KEY_PREV", Help: "Clave RSA (PEM) anterior: solo se publica en el JWKS durante una rotación, para que los tokens ya emitidos sigan verificando"},
	{Group: groupKeys, Env: "ASP_OIDC_ISSUER", Default: "http://<dirección de escucha>", Help: "Issuer OIDC. Si otros hosts verifican los tokens contra este control plane, ponla con la URL con la que llegan a él"},
	{Group: groupKeys, Env: "ASP_ATTEST_KEY", Default: "$TMPDIR/asp-attest-key.pem", Help: "PEM ECDSA P-256 de atestación; su clave pública es de confianza (lab de un host: el node-agent usa el mismo fichero)"},
	{Group: groupKeys, Env: "ASP_ATTEST_PUB", Help: "PEM de clave pública que sustituye a la de `ASP_ATTEST_KEY` para verificar"},
	{Group: groupKeys, Env: "ASP_ATTEST_TRUSTED_PUBS", Help: "Bundle PEM (`PUBLIC KEY` y/o `CERTIFICATE`) de más claves de confianza. La clave que trae la evidencia (`public_key_pem`) nunca vale; por mTLS vale además la del certificado del nodo que llama"},
	{Group: groupKeys, Env: "ASP_ATTEST_MAX_AGE", Default: "10m", Help: "Freshness para verify + claim OIDC"},
	{Group: groupKeys, Env: "ASP_ATTEST_ALLOWED_IMAGES", Help: "JSON `{\"images\":[{\"name\",\"kernel\",\"rootfs\",\"vmm\"}]}` con las imágenes (SHA-256 del kernel y de la imagen base, y opcionalmente la versión del hipervisor) para las que se acepta evidencia de arranque. Con él, una evidencia de otra imagen o sin digests se rechaza (400) y no genera claim; se relee al cambiar el fichero. Sin él se guardan los digests que declare el nodo. `node-agent --print-measurement` imprime la entrada de un nodo"},
	{Group: groupKeys, Env: "ASP_FENCE_PROVIDER", Default: "noop", Help: "`noop`|`http_webhook`|`redfish`|`ipmi`"},
	{Group: groupKeys, Env: "ASP_FENCE_USER", Help: "Usuario Redfish/IPMI"},
	{Group: groupKeys, Env: "ASP_FENCE_PASS", Help: "Contraseña IPMI/Redfish por defecto si el nodo no tiene token. Un `ipmitool` la recibe por `IPMI_PASSWORD`", Secret: true},

	// Egress, idle and retention.
	{Group: groupRetention, Env: "ASP_EGRESS_DEFAULT_ALLOW", Default: "1 con el store en memoria, 0 con Postgres", Help: "Qué puede alcanzar una sandbox cuyo tenant no tiene reglas: `1` todo, `0` nada. `ASP_EGRESS_DENY_DEFAULT` era este mismo ajuste con el sentido contrario; sigue valiendo si este no está, con un aviso"},
	{Group: groupRetention, Env: "ASP_EGRESS_DENY_DEFAULT", Help: "El nombre antiguo de `ASP_EGRESS_DEFAULT_ALLOW`, con el sentido contrario (`1` deniega)"},
	{Group: groupRetention, Env: "ASP_SANDBOX_IDLE_TIMEOUT", Default: "off", Help: "Parada por inactividad. Duración Go (`2h`, `1h`, `90m`). `0` / `off` / `false` / `disabled` desactiva. El valor recomendado de lab/producción es **2h** (también vale `1h`); no es el default del proceso, para que los smokes cortos no tumben sandboxes. Flag equivalente: `-idle-timeout` / `--idle-timeout` (pisa el env)."},
	{Group: groupRetention, Env: "ASP_SANDBOX_IDLE_SWEEP", Default: "1m", Help: "Cada cuánto el bucle del CP llama al reaper. No enciende el reaper por sí solo. Mínimo efectivo 1s."},
	{Group: groupRetention, Env: "ASP_STOPPED_SANDBOX_TTL", Default: "7d", Help: "Una sandbox parada más de este tiempo se borra con su disco (`stop_reason=retention_expired`), contado desde `stopped_at`. `7d` o una duración Go (`48h`); `0`/`off` las conserva hasta que se borren ([ADR-0012](docs/adr/0012-retained-disks.md))"},
	{Group: groupRetention, Env: "ASP_MAX_STOPPED_PER_TENANT", Help: "Al superarlo, se borran las sandboxes paradas más antiguas del tenant (`tenant_cap`), con un aviso en el log"},
	{Group: groupRetention, Env: "ASP_RETENTION_SWEEP", Default: "1m", Help: "Cada cuánto corre el barrido de retención"},
}
