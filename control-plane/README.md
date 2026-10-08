# Control plane

Servicio Go multi-tenant: API HTTP (TLS opcional), store in-memory (default), un fichero SQLite (`ASP_DATABASE_URL=sqlite:///…`, un solo host) o PostgreSQL (`ASP_DATABASE_URL`), journal de eventos, API keys, enrollment PKI, egress allowlist, OIDC (JWKS/mint) y proxy de exec hacia node-agents.

## API

La API HTTP (`/v1`) tiene una referencia con las rutas, el cuerpo de cada petición y respuesta, los estados que devuelve cada ruta y quién puede llamarla: [la API del plano de control](../docs/reference/api.md). Se genera de [`internal/api/openapi.yaml`](internal/api/openapi.yaml), que el plano de control sirve también como JSON en `GET /openapi.json`, sin credenciales (para generar un cliente o abrirlo en Swagger UI).

Ese documento está escrito a mano y unos tests lo comparan con el código, así que no se queda atrás: cada ruta de `routes.go` está en él y al revés, los campos de cada cuerpo son los del tipo Go que lo escribe, un estado que un manejador devuelve y el documento no lista es un fallo, y las órdenes de `asp` (`cli/internal/client`) solo llaman a rutas, campos y estados que el documento tiene. Al añadir o cambiar una ruta se edita el documento y se ejecuta `make docs`.

## Configuración

Cada ajuste es una variable de entorno `ASP_*`, o la clave del mismo nombre sin `ASP_` y en minúsculas en `/etc/asp/server.yaml` (prioridad: variable > fichero > valor por defecto). **La lista completa, con valor por defecto y descripción, se genera del código: [configuración del plano de control](../docs/reference/configuration/control-plane.md).** Las reglas comunes a todos los programas (booleanos, nombres que cambiaron, `--print-config`) están en [Configuración](../docs/reference/configuration.md), y el fichero, con ejemplos, en [el fichero de configuración](../docs/how-to/config-file.md).

## Ejecutarlo y probarlo

```bash
go test ./...
ASP_NODE_BOOTSTRAP_TOKEN=dev go run ./cmd/api
```

**Imagen de contenedor.** `docker build -f control-plane/Dockerfile -t asp-control-plane .` desde la raíz del repositorio (una versión publica `ghcr.io/luisgf/asp-control-plane:<versión>`, amd64 y arm64): distroless, 6 MB, usuario no root, sin shell. Escucha en `0.0.0.0:8080` y guarda la CA y las claves en el volumen `/var/lib/asp`. Hace falta una primera clave y, en producción, Postgres:

```bash
docker run -d --name asp-cp -p 8080:8080 -v asp-cp-state:/var/lib/asp \
  -e ASP_DATABASE_URL=postgres://asp:...@db:5432/asp -e ASP_BOOTSTRAP_API_KEY=... asp-control-plane
```

Los nodos no van en contenedores (necesitan KVM): esto es solo el plano de control, para Kubernetes o para quien prefiera no instalarlo en el host ([ADR-0004](../docs/adr/0004-k8s-scope.md)). Con TLS delante o con `ASP_TLS_CERT`/`ASP_TLS_KEY`, antes de que lo usen nodos de otros hosts.

**Tests contra los tres stores.** Memoria y SQLite corren siempre (SQLite es un fichero del directorio del test: no necesita nada). Con `DATABASE_URL` (un servidor Postgres; el rol necesita `CREATEDB`) cada paquete crea una base propia, la borra al terminar y no pisa a los demás, y se añade Postgres:

- la suite de `internal/api` corre una vez por store (`ASP_TEST_STORE=memory`, `=sqlite` o `=postgres` deja solo una pasada). Un test que falla dice sobre qué store corría;
- `TestPostgresParityWithMemory` y `TestSQLiteParityWithMemory` (`internal/store`) juegan un mismo guion contra `MemoryStore` y el otro store, con todos los métodos de `Store`, y exigen el mismo resultado y la misma clase de error en cada paso. Un método nuevo de `Store` sin paso en el guion hace fallar `TestParityScriptCoversStore`;
- los tests de `internal/store` que solo usan la interfaz (colocación, retención, liveness, claves, enroll…) son funciones compartidas con una variante por store.

Los tres stores son tres implementaciones de un contrato, y nada más los mantiene de acuerdo: lo que un test da por bueno en memoria puede no serlo en Postgres. Una migración de Postgres necesita su gemela en `migrations/sqlite/` (el mismo número; `TestSQLiteMigrationsMatchPostgres`). SQLite guarda los tiempos como texto de un ancho fijo, `2006-01-02T15:04:05.000000Z` en UTC, porque comparar el texto es comparar el instante: un test comprueba cada columna.


**Claves en directorios temporales.** Las rutas por defecto de la CA, la clave OIDC y la de atestación están en `/tmp` o `$TMPDIR`: un reinicio las borra, los certificados de nodo dejan de verificar y los tokens cambian de `kid`. En modo producción (`ASP_DATABASE_URL`, `ASP_TLS_CERT`, `ASP_CLIENT_CA` o `ASP_IDP_REQUIRED=1`) el control plane no arranca (código 2) si `ASP_CA_CERT`, `ASP_CA_KEY`, `ASP_OIDC_KEY` o `ASP_ATTEST_KEY` apuntan a `/tmp`, `/var/tmp`, `/dev/shm` o `$TMPDIR`, y el error nombra cada variable. Apúntalas a almacenamiento persistente (las que falten se crean ahí) o usa `ASP_ALLOW_TMP_KEYS=1`. En lab solo deja un aviso con las rutas.
## Logs y coste por petición

Cada petición deja una línea `request` con método, ruta, estado, bytes, duración y origen. Los sondeos de los nodos (`/work`, heartbeats, claim, status…) y `/healthz` van a nivel Debug: con varios nodos sondeando cada 2 s taparían el resto. El middleware de API keys cuenta las claves como mucho cada 10 s y escribe `last_used_at` como mucho una vez por minuto y clave, en vez de un `count(*)` y un `UPDATE` por petición.

## Parada por inactividad

**Por qué.** Una sesión `asp session` (o un create olvidado) deja la microVM encendida hasta que alguien la pare. El reaper del control-plane marca `stopping` (o `stopped` si nunca se asignó nodo) cuando no hay actividad durante el umbral, y el reconciler del nodo apaga la VM **conservando su disco**: es una parada, no un borrado, y `asp session resume` la trae de vuelta.

**Qué cuenta como actividad.** `last_activity_at` se mueve en: create, transición a `running` (start) y **un exec**: al empezar, cada `min(1 min, timeout/3)` mientras la llamada al node-agent sigue abierta (un stream o una llamada acumulada que espera su respuesta) y al terminar con respuesta correcta (aunque el proceso del guest salga ≠ 0). «Inactiva» es «ningún exec en marcha y ninguno terminado hace N»: el reaper no para una sandbox en mitad de un comando que lleva más que el timeout. No cuentan: GET, heartbeat, sondeos de `/work`, ni el fin de un exec que no llegó a responder.

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

**Un store que guarde el estado es requisito** (Postgres, o SQLite en un host). Con el store en memoria, reiniciar el plano de control olvida las sandboxes paradas y el GC de cada nodo borra sus discos; el plano de control lo avisa al arrancar si hay retención activa. `asp node list` muestra, por nodo, cuántas paradas guardan un disco (`STOPPED (DISKS)`) y el espacio libre que el nodo informa en su heartbeat (`DISK FREE`, de `--disk-dir`).

## Timeouts hacia el node-agent

**Por qué.** El cliente hacia el agente tenía un timeout total de 30 s, y ese timeout cuenta también la lectura del body: un `exec` en streaming (`?stream=1`) o una sesión PTY se cortaba a los 30 s aunque siguiera saliendo salida.

**Qué se limita.** Cada fase de la llamada, igual por HTTP plano (agente en el mismo host) que por mTLS (`https://`):

| Fase | Límite |
|---|---|
| Conexión TCP | 10 s |
| Handshake TLS | 10 s |
| Hasta que el agente empieza a responder (cabeceras) | 30 s |
| `exec` sin `?stream=1`, de principio a fin | `ASP_BUFFERED_EXEC_TIMEOUT` (10 min por defecto; `0`/`off` sin límite) o el `timeout_seconds` de la petición, si es menor |
| `exec/stdin`, de principio a fin | 30 s |

Un stream no tiene límite total: dura lo que el comando. Si el cliente cuelga, el plano de control cancela la llamada al agente. Si el nodo desaparece a mitad de stream, lo detecta el keep-alive de TCP en unos minutos.

**Exec acumulado.** El agente responde a un `exec` acumulado cuando el comando termina. Su límite ya no es un 30 s fijo: lo pone `ASP_BUFFERED_EXEC_TIMEOUT` (10 min por defecto) y una petición puede pedir menos con `timeout_seconds` (`asp sandbox exec|run --exec-timeout`, `asp session exec --buffered --exec-timeout`); nunca más que el tope. El plano de control pasa ese tiempo al node-agent y este al pod-daemon (`timeout_secs`, con su propio tope `--exec-max-timeout-secs`, 3600), de modo que el guest corta el comando a la vez (`exit_code` 124 con lo que llevaba escrito) y el node-agent espera 15 s más antes de rendirse. Un acumulado que el plano de control corta es un **504** que dice el límite y qué hacer (el stream, o subir el tope), no un 502 que culpa al nodo. Para algo más largo que el tope, el stream (`asp session exec` lo usa por defecto).

**Límites.** El node-agent y el plano de control envían las cabeceras del stream en cuanto el comando arranca, así que un comando que no escribe nada no agota los 30 s hasta las cabeceras. Esos 30 s y los de `exec/stdin` no se configuran por entorno. Un stream dura lo que el comando: termina si el cliente se va (el guest mata el comando y su grupo de procesos) y, con `--stream-idle-timeout-secs`, tras ese tiempo sin salida ni entrada.

## IdP (OIDC)

Con `ASP_IDP_ISSUER` el plano de control valida los tokens de un IdP (Keycloak, Entra, Okta…) y saca de ellos el dueño, el rol y el tenant. Guía, con la matriz de roles y un ejemplo con Keycloak: [conectar un IdP](../docs/how-to/idp.md). El Keycloak del laboratorio de los mantenedores: [`docs/lab/idp-keycloak.md`](../docs/lab/idp-keycloak.md).
