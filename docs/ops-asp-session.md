# Ops — Sesión de agente `asp session` (shell del harness)

**Esta es la superficie de integración.** Un agente largo se engancha a una sesión (un sandbox: `owner_sub`, disco del guest, egress, idle) y llama a `exec` durante horas. Create→exec→destroy por comando **no** es el producto. Dirección: [ADR-0009](adr/0009-agent-sessions.md) · [por qué](why-agent-sessions.md).

Cómo un harness tipo [OpenCode](https://github.com/sst/opencode) apunta su herramienta de shell a ese sandbox ya creado, en lugar de ejecutar cada comando en el host.

La primitiva one-shot (CI / un comando) sigue existiendo y no es esta página: [`ops-asp-agent-runner.md`](ops-asp-agent-runner.md). CLI base: [`why-cli-asp.md`](why-cli-asp.md). Identidad del dueño: [ADR-0007](adr/0007-multi-user-identity.md) (`owner_sub` sale del JWT, no del guest). El exec API es el dataplane **dentro** de la sesión, no el contrato que el harness debe diseñar.

## Por qué

`asp sandbox run` es la primitiva correcta cuando el proceso **es** un solo comando (CI, ops) y puede esperar el ciclo completo. No es el contrato del bucle del agente ([ADR-0009](adr/0009-agent-sessions.md)):

1. `POST /v1/sandboxes`
2. poll hasta `running`
3. `POST /v1/sandboxes/{id}/exec`
4. `DELETE`

Eso aísla cada invocación (el sandbox muere con el proceso), pero un bucle de agente —decenas de llamadas a `bash`— paga arranque de microVM cada vez. En dry-run (FakeVMM) el coste es bajo; con Cloud Hypervisor el boot domina la latencia del tool.

El contrato del agente es **una sesión con nombre**: un JSON local (id + URL del control-plane, sin secreto) y un binario que el harness pone en el sitio del shell. El one-shot no se elimina; deja de ser la historia principal.

## Qué ganamos

| Pieza | Comportamiento |
|---|---|
| `asp session start --name NOMBRE` | Mismos flags de create que `sandbox run` (`--tenant`, `--image`, `--cpu-millis`, `--memory-mib`, `--node-id`, `--vmm-profile`, `--timeout`, `--cp-url`, auth) más `--workspace`. Espera `running`. Escribe `NOMBRE.json`. Imprime **solo el id** en stdout. Nombre omitido = `default`. |
| Estado local | Directorio `~/.cache/asp/sessions/` (o `ASP_SESSION_DIR` / `--session-dir`), modo `0700`. Un fichero por nombre, `0600`: `<nombre>.json`. Campos: `name`, `sandbox_id`, `cp_url`, `tenant_id`, `image_ref`, `workspace`, `created_at`. **Sin** token ni API key. |
| Fichero explícito | `--session-file` o `ASP_SESSION_FILE` sigue eligiendo **un** path y no usa `--name`. Sirve para tests y para migrar el `session.json` viejo. El default ya no es `~/.cache/asp/session.json`. |
| `asp session exec --name` | Mismo id. Por defecto **stream NDJSON**: stdout/stderr se imprimen al llegar. Exit code del proceso = `exit_code` del evento `exit`. `--cmd '…'` **o** `-- argv…`. `--buffered` o `--json` piden el JSON acumulado de siempre (smokes). |
| `asp session status --name` | Lee ese fichero y hace `GET`. `--json` opcional. Si no hay fichero, error claro (exit 1). |
| `asp session stop --name` | `DELETE` + borra ese fichero. 404 (ya destruido) limpia el fichero igual. Otro error HTTP **conserva** el fichero para reintentar. |
| Auth | Igual que el resto del CLI: Bearer IdP automático (`asp auth` / `ASP_ID_TOKEN` / cache). No se copia al fichero de sesión. |
| `--force` en start | Si ya hay sesión **con ese nombre**, intenta destruir el sandbox anotado y empieza otro. Sin `--force`, start falla y no crea un segundo sandbox a ciegas. |
| `--workspace /ruta` | Ruta **absoluta** de un directorio que debe existir en la máquina del CLI. Viaja como `workspace_host_path` en `POST /v1/sandboxes` y se guarda en el JSON local. Ver límites: no es un mount de KVM. |
| `--local` en stop | Borra solo el fichero. El sandbox **sigue vivo** en el CP. Escape de ops, no el camino normal. |

Dos nombres son dos sandboxes. No comparten disco ni egress.

## Qué no ganamos (límites reales)

| Límite | Realidad |
|---|---|
| No es un plugin de OpenCode | No registra tools ni habla el protocolo del harness. Es un binario que el harness **exec**. El wrapper de abajo es un ejemplo, no se instala solo. |
| El nombre es local | Otro host, otro contenedor o un `HOME` distinto no ve el directorio. El CP sí sigue teniendo el sandbox. El nombre no es un id global. |
| `--workspace` no monta en KVM | El CP guarda la ruta (migración `009`, campo `workspace_host_path`). El reconciler la copia a `MicroVMConfig.WorkspaceHostPath`. FakeVMM la recuerda. **No** se lanza `virtiofsd`. `vm.create` de Cloud Hypervisor **no** incluye `fs` mientras `WorkspaceFSSocket` esté vacío, y el reconciler lo deja vacío a propósito: un `fs` sin socket haría fallar el boot. El tag previsto, el día que exista el daemon, es `workspace` y el mount del guest sería `/workspace`. Hoy el guest **no** ve ese directorio. Dry-run tampoco finge un árbol dentro de la VM. |
| La ruta es la del nodo | El CLI comprueba que el path exista en **su** máquina. Si el node-agent corre en otro host, la cadena guardada puede no existir allí. El CP no hace `stat`. |
| Exec no es un PTY | No hay stdin interactivo ni TTY. El stream son líneas NDJSON (`type=stdout\|stderr\|exit`) sobre el mismo `POST /v1/sandboxes/{id}/exec?stream=1`, proxy CP → node-agent `POST /v1/internal/exec?stream=1` → pod-daemon `POST /v1/exec?stream=1` con `Transfer-Encoding: chunked`. |
| Guest viejo | Si el pod-daemon responde 404 a `?stream=1`, el node-agent hace el exec JSON y lo reescribe como un solo burst NDJSON al final. No es streaming real. Hace falta el pod-daemon de este cambio dentro de la imagen. |
| Binario en el stream | Los trozos pasan por JSON string (UTF-8 con reemplazo). No es un pipe de bytes opacos. El JSON acumulado tampoco lo era (`read_to_string`). |
| GC solo en el CP, y solo si está encendido | Este CLI **no** apaga sandboxes por su cuenta. El control-plane puede hacerlo con `ASP_SANDBOX_IDLE_TIMEOUT` (recomendado `2h`; default del proceso **off**). Si el reaper paró el sandbox, `session status` y `session exec` lo dicen (`idle timeout` / `idle_reaped=true`, exit 1) y **no** borran el JSON. `start --force` (mismo `--name`) crea otro. |
| No aísla herramientas entre sí | Todos los `exec` de esa sesión comparten un guest. Eso es la ventaja y el riesgo. |
| Auth sigue siendo por invocación | Cada `exec` resuelve el Bearer de nuevo. Si el token caduca, el siguiente comando falla con 401. El sandbox no se entera. |
| `owner_sub` | Lo pone el CP desde el JWT ([ADR-0007](adr/0007-multi-user-identity.md)). La sesión no acepta un owner de mentira en el JSON local. |
| Dry-run ≠ KVM | Tests de sesión y de stream = `httptest`. El pod-daemon se cubre con `cargo test`. No hace falta ncc1701d para eso. Aislamiento real solo con CH + KVM, y aun así **sin** el share del workspace. |

## Alternativas que descartamos (y la consecuencia)

1. **Seguir solo con `asp sandbox run`.** Máximo aislamiento por comando y cero estado local. Consecuencia: cada tool espera create+boot+destroy. Sigue siendo el comando de CI. No lo quitamos. Su exec sigue siendo el JSON acumulado (no hace falta stream para un one-shot que igual espera al final).
2. **Plugin OpenCode que hable HTTP con el CP.** Evitaría el wrapper shell. Consecuencia: duplicar auth, wait y el cliente; este repo no mantiene ese plugin. La CLI es el contrato estable. El ejemplo de abajo es el sustituto.
3. **PTY / SSH largo al guest.** Un shell de verdad (stdin, TTY). Consecuencia: otra superficie de auth. El stream NDJSON no lo sustituye. Sigue diferido.
4. **Un solo `session.json`.** Menos flags. Consecuencia: dos agentes se pisan. Por eso el directorio y `--name`. El fichero explícito queda como override, no como default.
5. **Guardar el JWT en el JSON.** Arranque más simple sin `asp auth`. Consecuencia: un fichero robado es un token. Prohibido.
6. **Fingir virtiofs en FakeVMM o meter `fs` en CH sin virtiofsd.** El dry-run «pasaría» y el boot real fallaría, o al revés. El spec se guarda; el dispositivo no se inventa.

## Flujo

```text
asp session start --name agente
    │  create (workspace_host_path opcional) → wait running
    ▼
~/.cache/asp/sessions/agente.json    mode 0600
    │
    ├─ asp session exec --name agente --cmd '…'
    │     POST /v1/sandboxes/{id}/exec?stream=1
    │     NDJSON: {"type":"stdout","data":"…"}\n
    │             {"type":"stderr","data":"…"}\n
    │             {"type":"exit","exit_code":N}\n
    ├─ asp session exec --name agente --buffered --cmd '…'
    │     POST sin stream → {"stdout","stderr","exit_code"}
    ├─ asp session status --name agente
    └─ asp session stop --name agente    DELETE, luego borra el JSON
```

Diagnóstico (ids, transiciones, errores) va a **stderr**. El id de `start`/`stop` y la salida del comando van a **stdout**, para no mezclarlos si el harness captura stdout como resultado del tool. En el camino stream, stdout del guest y el id no se mezclan porque el id solo lo imprime `start`/`stop`.

## Cómo lo apuntaría OpenCode

OpenCode (y harnesses parecidos) suelen lanzar la herramienta bash como `sh -c "<comando del modelo>"` en el host. Para que ese tool entre al sandbox hay que **sustituir el shell del tool**. No hay plugin en este repo.

Wrapper (cópialo fuera de git si lo modificas; este no lleva secretos). Usa el nombre de la sesión y el exec en streaming (el default: no pongas `--buffered` si quieres ver la salida según sale):

```bash
#!/bin/sh
# asp-session-shell.sh — la sesión "opencode" ya tiene que estar arrancada.
#   asp session start --name opencode --workspace /ruta/absoluta/del/repo …
# Caso típico del tool:  asp-session-shell.sh -c "echo hello"
NAME="${ASP_SESSION_NAME:-opencode}"
if [ "$1" = "-c" ]; then
  shift
  exec asp session exec --name "$NAME" --cmd "$*"
fi
exec asp session exec --name "$NAME" -- "$@"
```

Secuencia de operador / agente:

```bash
export ASP_CP_URL=http://127.0.0.1:8080   # dry-run local; lab: http://127.0.0.1:18112
# lab IdP: export ASP_IDP_REQUIRED=1  y secretos en ~/.secrets/ (ver ops-asp-agent-runner.md)
asp session start --name opencode --workspace /ruta/absoluta/del/repo \
  --tenant=tenant-demo --node-id=dev-node --timeout=120s
# stdout: <sandbox id>
# El guest NO verá /ruta/… hasta que exista virtiofsd. Ver límites.

asp session exec --name opencode --cmd 'echo hello-from-session'
asp session exec --name opencode -- echo hello --flag

asp session status --name opencode
asp session stop --name opencode
```

En el lab Keycloak el tenant de ejemplo de los one-liners existentes es `--tenant=default` (no `tenant-demo`, que es el default del flag). El Bearer se adjunta solo; no hace falta meter el JWT en el wrapper.

Si el harness no puede cambiar el binario del shell y solo puede prefijar un comando:

```text
asp session exec --name opencode --cmd '<comando>'
```

`--cmd` es **un** argumento. Sin comillas, el resto de argv pisa a `--cmd`. Ante la duda, usa `--`.

`session exec` **sin** fichero para ese nombre termina con exit 1 y el texto `no active session` en stderr. No crea un sandbox implícito.

Para forzar el JSON de una pieza (el contrato viejo, el de los smokes): `asp session exec --name opencode --buffered --cmd '…'`. Con `--json`, stdout es el objeto `{stdout,stderr,exit_code}` y no el texto del guest.

## Parada por inactividad (reaper del CP)

**Por qué.** `session start` sin `stop` deja la microVM. El plano de control puede pararla solo cuando lleva demasiado tiempo sin actividad.

**Umbral.** `ASP_SANDBOX_IDLE_TIMEOUT` (duración: `2h` recomendado). `0`, `off`, `false` o `disabled` lo apagan. Si la variable **no está**, el reaper no corre. El lab systemd exporta `2h`. Flag del binario: `-idle-timeout 2h`. Intervalo: `ASP_SANDBOX_IDLE_SWEEP` (default `1m`). **No se ha cambiado esa semántica.** No hay idle distinto por `--name`.

**Qué cuenta como actividad.**

| Hecho | ¿Mueve `last_activity_at`? |
|---|---|
| `session start` (create, y el paso a `running`) | Sí. Es el reloj inicial. No es un heartbeat nuevo: el create y la transición a `running` ya lo escribían. |
| Exec que el CP proxyó bien (JSON acumulado, o stream NDJSON copiado hasta el final), aunque el guest salga ≠ 0 | Sí. **Esto es lo único que refresca el reloj después del start.** |
| `session status`, `GET`, heartbeat del nodo, renew del lease | No. Si contaran, el reconciler impediría el idle para siempre. |
| Exec que falla antes del guest (red, 502, stream cortado a medias) | No. |

**Qué hace el reaper.** Pasa el sandbox a `stopping` (el node-agent lo destruye) o a `stopped` si nunca tuvo nodo. `stop_reason=idle_timeout`. Evento `sandbox.idle_reaped`.

**Qué ve esta CLI.** `asp session status --name …` imprime `idle_reaped=true` (y `--json` el booleano) y sale **1**. `asp session exec` no llama al exec proxy si el GET ya trae `stop_reason=idle_timeout`. El fichero de ese nombre no se borra solo.

**Límites.** Un umbral de proceso para todos los sandboxes. Filas ya existentes al aplicar la migración `008` empezaron el reloj en ese momento, no en el `created_at` histórico.

## Variables y flags

| Nombre | Rol |
|---|---|
| `--name` | Nombre de la sesión. Default `default`. Un segmento `[A-Za-z0-9][A-Za-z0-9._-]{0,63}`. |
| `ASP_SESSION_DIR` / `--session-dir` | Directorio de los `<nombre>.json`. Default `~/.cache/asp/sessions`. |
| `ASP_SESSION_FILE` / `--session-file` | Path de un solo JSON. Si está, **ignora** el nombre para elegir fichero. Ya no es el default. |
| `--workspace` | Solo `start`. Directorio absoluto existente en el host del CLI. Se persiste; no se monta en CH. |
| `--buffered` | Solo `exec`. JSON acumulado, sin NDJSON. |
| `ASP_CP_URL` / `--cp-url` | En `start`, la URL que se guarda. En `exec`/`status`/`stop`, si **no** pasas `--cp-url`, se usa la URL guardada. |
| Resto `ASP_IDP_*`, `ASP_ID_TOKEN`, `ASP_API_KEY` | Igual que [`ops-asp-agent-runner.md`](ops-asp-agent-runner.md). |
| `--force` | Solo `start`. Destruye el id anotado en ese nombre (404 = ya no está) y crea otro. |
| `--local` | Solo `stop`. No llama al CP. |
| `--keep` | **No** existe en `session`. El sandbox vive hasta `stop` o el reaper. |

Migrar el fichero único antiguo:

```bash
mkdir -p ~/.cache/asp/sessions
mv ~/.cache/asp/session.json ~/.cache/asp/sessions/default.json
```

## Fallos concretos

| Síntoma | Causa probable |
|---|---|
| `no active session` | No hubo `start` de ese `--name`, otro `HOME`, u otro `--session-dir`. |
| `active session …` en start | Ya hay JSON para ese nombre. `stop --name` o `start --force --name`. |
| `invalid session name` | El nombre tiene `/`, espacios o `..`. |
| exec HTTP 404 | Alguien borró el sandbox y el JSON sigue. `stop` (limpia en 404) o `start --force`. |
| `idle timeout` / `idle_reaped=true` | El reaper paró el sandbox. El JSON local sigue. `asp session start --force --name …`. |
| exec 401 | Token caducado o `ASP_IDP_REQUIRED` sin secretos. `asp auth status`. |
| stdout vacío y exit ≠ 0 | El guest falló sin stdout; el código es el `exit_code`. El error del CLI (red, 500, stream sin evento `exit`) es exit **1**, no el código del guest. |
| `exec stream: missing exit event` | El proxy cortó el NDJSON. No hubo `exit_code`. La actividad **no** se refresca. |
| `state file kept` | `DELETE` falló (no 404). El JSON sigue para reintentar `stop`. |
| El guest no ve `--workspace` | Esperable en CH y en FakeVMM. La ruta está en el spec (`status --json` → `workspace` / `live.workspace_host_path`), no en el disco del guest. |
| Salida de golpe al final | `--buffered`, `--json`, o un pod-daemon que no habla `?stream=1` (el node-agent emite un burst). |

## Tests

```bash
cd cli && go test ./...
cd control-plane && go test ./...
cd node-agent && go test ./...
cd pod-daemon && cargo test
```

Cubren el directorio de sesiones (nombres distintos, modo `0600`, rechazo de `../`), el CLI contra `httptest` (start→exec reutilizando el id, `--buffered`, stream que entrega el primer chunk **antes** de que el servidor cierre el body, `--workspace` en el POST), el proxy del CP y del node-agent con el mismo patrón, el fallback 404→JSON, FakeVMM guardando la ruta, y que `vm.create` **no** lleve `fs` si no hay socket de virtiofsd. El `cargo test` del pod-daemon comprueba el `POST /v1/exec?stream=1` chunked y que el `POST /v1/exec` sin query sigue devolviendo JSON. No requieren ncc1701d. El camino FakeVMM de `sandbox run` sigue siendo `make smoke-asp`.

## Referencias

- [ADR-0009](adr/0009-agent-sessions.md) — dirección y seguimiento
- [`why-agent-sessions.md`](why-agent-sessions.md)
- [`ops-asp-agent-runner.md`](ops-asp-agent-runner.md) — primitiva one-shot + IdP
- [`bare-metal-ch.md`](bare-metal-ch.md) — CH real, sin virtiofs de workspace
- [`why-cli-asp.md`](why-cli-asp.md)
- [`roadmap.md`](roadmap.md)
- [ADR-0007](adr/0007-multi-user-identity.md)
