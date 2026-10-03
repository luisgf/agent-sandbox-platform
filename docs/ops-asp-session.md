# Ops — Sesión reutilizable `asp session` (shell de agente)

Cómo un harness tipo [OpenCode](https://github.com/sst/opencode) puede apuntar su herramienta de shell a un sandbox **ya creado**, en lugar de ejecutar cada comando en el host o pagar un create→destroy por invocación.

Contrato one-shot (sigue existiendo): [`ops-asp-agent-runner.md`](ops-asp-agent-runner.md). CLI base: [`why-cli-asp.md`](why-cli-asp.md). Identidad del dueño: [ADR-0007](adr/0007-multi-user-identity.md) (`owner_sub` sale del JWT, no del guest).

## Por qué

`asp sandbox run` es el contrato correcto cuando el agente lanza **un** comando y puede esperar el ciclo completo:

1. `POST /v1/sandboxes`
2. poll hasta `running`
3. `POST /v1/sandboxes/{id}/exec`
4. `DELETE`

Eso aísla cada invocación (el sandbox muere con el proceso), pero un bucle de agente —decenas de llamadas a `bash`— paga arranque de microVM cada vez. En dry-run (FakeVMM) el coste es bajo; con Cloud Hypervisor el boot domina la latencia del tool.

Hace falta un segundo contrato: **una sesión local** que recuerde el id y la URL del control-plane, y un binario que el harness pueda poner en el sitio del shell.

## Qué ganamos

| Pieza | Comportamiento |
|---|---|
| `asp session start` | Mismos flags de create que `sandbox run` (`--tenant`, `--image`, `--cpu-millis`, `--memory-mib`, `--node-id`, `--vmm-profile`, `--timeout`, `--cp-url`, auth). Espera `running`. Escribe el estado. Imprime **solo el id** en stdout. |
| Estado local | `~/.cache/asp/session.json` (o `ASP_SESSION_FILE` / `--session-file`), directorio `0700`, fichero `0600`. Campos: `sandbox_id`, `cp_url`, `tenant_id`, `image_ref`, `created_at`. **Sin** token ni API key. |
| `asp session exec` | Reutiliza ese id contra el exec API ya existente. Stdout/stderr del guest; **exit code del proceso = `exit_code`**. `--cmd '…'` **o** `-- argv…`. |
| `asp session status` | Lee el fichero y hace `GET`. `--json` opcional. Si no hay fichero, error claro (exit 1). |
| `asp session stop` | `DELETE` + borra el fichero. 404 (ya destruido) limpia el fichero igual. Otro error HTTP **conserva** el fichero para reintentar. |
| Auth | Igual que el resto del CLI: Bearer IdP automático (`asp auth` / `ASP_ID_TOKEN` / cache). No se copia al fichero de sesión. |
| `--force` en start | Si ya hay sesión, intenta destruir el sandbox anotado y empieza otro. Sin `--force`, start falla y no crea un segundo sandbox a ciegas. |
| `--local` en stop | Borra solo el fichero. El sandbox **sigue vivo** en el CP. Escape de ops, no el camino normal. |

## Qué no ganamos (límites reales)

| Límite | Realidad |
|---|---|
| No es un plugin de OpenCode | No registra tools, no habla el protocolo del harness, no hay TUI ni PTY. Es un binario que el harness **exec**. Hay que cablearlo fuera de este repo. |
| Un fichero = una sesión | Dos agentes en la misma `$HOME` comparten (y se pisan) `session.json`. Paralelo ⇒ un `ASP_SESSION_FILE` distinto por agente, y un sandbox distinto. No hay sesiones con nombre. |
| El fichero es local a la máquina del CLI | Otro host, otro contenedor o un `HOME` distinto no ve la sesión. El CP sí sigue teniendo el sandbox. |
| No hay sync de workspace | virtiofs / copia del árbol del proyecto **no** se resuelve aquí. `exec` corre en el filesystem del guest. Editar ficheros en el host no los hace aparecer dentro de la microVM, y al revés. `--cwd` solo cambia el directorio **dentro del guest** si esa ruta existe. |
| Exec no es stream | El CP responde JSON acumulado (`stdout` / `stderr` / `exit_code`). El CLI imprime eso al terminar. No hay byte-a-byte ni stdin interactivo. |
| GC solo en el CP, y solo si está encendido | Este CLI **no** apaga sandboxes por su cuenta. El control-plane puede hacerlo con `ASP_SANDBOX_IDLE_TIMEOUT` (recomendado `2h`; default del proceso **off** para no romper smokes). Si el reaper paró el sandbox, `session status` y `session exec` lo dicen (`idle timeout` / `idle_reaped=true`, exit 1) y **no** borran el JSON. `start --force` crea otro. `--local` en stop sigue sin llamar al CP. |
| No aísla herramientas entre sí | Todos los `exec` de esa sesión comparten un guest: ficheros, procesos y `/tmp` persisten entre llamadas. Eso es la ventaja y el riesgo (un comando deja estado para el siguiente). |
| Auth sigue siendo por invocación | Cada `exec` resuelve el Bearer de nuevo. Si el token caduca a mitad de sesión, el siguiente comando falla con 401 hasta `asp auth login` o el auto-fetch. El sandbox no se entera. |
| `owner_sub` | Lo pone el CP desde el JWT ([ADR-0007](adr/0007-multi-user-identity.md)). La sesión no acepta un owner “de mentira” en el JSON local. |
| Dry-run ≠ KVM | Los tests de este cambio usan `httptest` (y el stack local puede usar FakeVMM). No hace falta ncc1701d para `go test`. Aislamiento real solo con CH + KVM. |

## Alternativas que descartamos (y la consecuencia)

1. **Seguir solo con `asp sandbox run`.** Máximo aislamiento por comando y cero estado local. Consecuencia: cada tool del agente espera create+boot+destroy. Sigue siendo el comando adecuado para one-liners y CI. No lo quitamos.
2. **Plugin OpenCode que hable HTTP con el CP.** Evitaría el wrapper shell y podría modelar tools nativos. Consecuencia: duplicar auth, wait y el cliente en el lenguaje del harness; este repo no mantiene ese plugin. La CLI es el contrato estable.
3. **SSH largo al guest.** Un shell de verdad (PTY, stdin). Consecuencia: otra superficie, claves, y no reutiliza el exec proxy (CP → node-agent → pod-daemon) que ya autoriza con el mismo Bearer. Fuera de este cambio.
4. **Varias sesiones con nombre en un directorio.** Útil para agentes paralelos. Consecuencia: más CLI y más formas de filtrar el id equivocado. Hoy el override es un path (`ASP_SESSION_FILE`).
5. **Guardar el JWT en `session.json`.** Arranque más simple sin `asp auth`. Consecuencia: un fichero de sesión robado es un token. Prohibido; el fichero solo tiene id y URL.

## Flujo

```text
asp session start
    │  create → wait running
    ▼
~/.cache/asp/session.json   { sandbox_id, cp_url, … }   mode 0600
    │
    ├─ asp session exec --cmd '…'   ──POST /v1/sandboxes/{id}/exec──► CP
    ├─ asp session exec -- argv…
    ├─ asp session status
    └─ asp session stop             ──DELETE──► CP, luego borra el JSON
```

Diagnóstico (ids, transiciones, errores) va a **stderr**. El id de `start`/`stop` y la salida del comando van a **stdout**, para no mezclarlos si el harness captura stdout como resultado del tool.

## Cómo lo apuntaría OpenCode

OpenCode (y harnesses parecidos) suelen lanzar la herramienta bash como un shell: `sh -c "<comando del modelo>"`, en el host. Para que ese tool entre al sandbox hay que **sustituir el shell del tool**, no parchear OpenCode dentro de este repo.

Ejemplo de wrapper (no se instala solo; cópialo donde quieras, fuera de git si lleva secretos — este no lleva):

```bash
#!/bin/sh
# asp-session-shell.sh — invocar con la sesión ya arrancada.
# Caso típico del tool:  asp-session-shell.sh -c "echo hello"
if [ "$1" = "-c" ]; then
  shift
  exec asp session exec --cmd "$*"
fi
# Caso argv explícito: asp-session-shell.sh echo hello
exec asp session exec -- "$@"
```

Secuencia de operador / agente:

```bash
export ASP_CP_URL=http://127.0.0.1:8080   # dry-run local; lab: http://127.0.0.1:18112
# lab IdP: export ASP_IDP_REQUIRED=1  y secretos en ~/.secrets/ (ver ops-asp-agent-runner.md)
asp session start --tenant=tenant-demo --node-id=dev-node --timeout=120s
# stdout: <sandbox id>

# Lo que el tool debería acabar ejecutando:
asp session exec --cmd 'echo hello-from-session'
asp session exec -- echo hello --flag

asp session status
asp session stop
```

En el lab Keycloak el tenant de ejemplo de los one-liners existentes es `--tenant=default` (no `tenant-demo`, que es el default del flag). El Bearer se adjunta solo; no hace falta meter el JWT en el wrapper.

Si el harness no puede cambiar el binario del shell y solo puede prefijar un comando, el modelo (o la plantilla del tool) tiene que llamar literalmente:

```text
asp session exec --cmd '<comando>'
```

`--cmd` es **un** argumento. Sin comillas, `asp session exec --cmd echo hello` ejecuta solo lo que sobre como positional (`hello`), porque los args tras los flags pisan a `--cmd`. Ante la duda, usa `--`.

`session exec` **sin** fichero termina con exit 1 y el texto `no active session` en stderr. No crea un sandbox implícito: un tool silencioso no debe arrancar microVMs.

## Parada por inactividad (reaper del CP)

**Por qué.** `session start` sin `stop` deja la microVM. El plano de control puede pararla solo cuando lleva demasiado tiempo sin actividad, para no pagar CPU/RAM de un agente que se fue.

**Umbral.** Variable `ASP_SANDBOX_IDLE_TIMEOUT` (duración: `2h` recomendado, `1h` también válido, `90m`, …). `0`, `off`, `false` o `disabled` lo apagan. Si la variable **no está**, el reaper no corre: los smokes y `go test` no tienen que acordarse de desactivarlo. El lab systemd sí exporta `2h`. Flag del binario del CP: `-idle-timeout 2h` (pisa el env). Intervalo del bucle: `ASP_SANDBOX_IDLE_SWEEP` (default `1m`).

**Qué es actividad.** Create, el paso a `running` (el start terminó) y un exec que el CP proxyó bien. Renovar el lease del nodo, el heartbeat o un `GET` **no** mantienen viva la VM: si lo hicieran, el reconciler impediría el idle para siempre.

**Qué hace el reaper.** Pasa el sandbox a `stopping` (el node-agent lo destruye) o a `stopped` si nunca tuvo nodo. `stop_reason=idle_timeout`. Evento `sandbox.idle_reaped`.

**Qué ve esta CLI.** `asp session status` imprime `idle_reaped=true` (y `--json` el booleano) y sale **1**. `asp session exec` no llama al exec proxy si el GET ya trae `stop_reason=idle_timeout`; si el CP responde 409 con `idle timeout`, el mensaje es el mismo. El fichero de sesión no se borra solo.

**Límites.** No hay idle “por sesión” distinto del umbral global del CP. Dos agentes con dos sandboxes comparten el mismo timeout. Un exec que falla antes de llegar al guest (red, 502) no cuenta como actividad. Filas ya existentes al aplicar la migración `008` empiezan el reloj en ese momento (`last_activity_at=now()`), no en el `created_at` histórico.

## Variables y flags

| Nombre | Rol |
|---|---|
| `ASP_SESSION_FILE` / `--session-file` | Ruta del JSON. Default `~/.cache/asp/session.json`. |
| `ASP_CP_URL` / `--cp-url` | En `start`, la URL que se guarda. En `exec`/`status`/`stop`, si **no** pasas `--cp-url`, se usa la URL guardada (el default del flag no pisa el fichero). |
| Resto `ASP_IDP_*`, `ASP_ID_TOKEN`, `ASP_API_KEY` | Igual que [`ops-asp-agent-runner.md`](ops-asp-agent-runner.md). |
| `--force` | Solo `start`. Destruye el id anotado (404 = ya no está) y crea otro. |
| `--local` | Solo `stop`. No llama al CP. |
| `--keep` | **No** existe en `session`. El sandbox vive hasta `stop`. |

## Fallos concretos

| Síntoma | Causa probable |
|---|---|
| `no active session` | No hubo `start`, otro `HOME`, u otro `--session-file`. |
| `active session …` en start | Ya hay JSON. `stop` o `start --force`. |
| exec HTTP 404 | Alguien borró el sandbox y el JSON sigue. `stop` (limpia en 404) o `start --force`. |
| `idle timeout` / `idle_reaped=true` | El reaper del CP paró el sandbox por inactividad (`ASP_SANDBOX_IDLE_TIMEOUT`). El JSON local sigue. `asp session start --force`. Ver control-plane README. |
| exec 401 | Token caducado o `ASP_IDP_REQUIRED` sin secretos. `asp auth status`. |
| stdout vacío y exit ≠ 0 | El guest falló sin stdout; el código es el `exit_code`. El error del CLI (red, 500) es exit **1**, no el código del guest. |
| `state file kept` | `DELETE` falló (no 404). El JSON sigue para poder reintentar `stop`. |
| Comandos ven un FS vacío | No hay virtiofs/copia. Esperable. |

## Tests

```bash
cd cli && go test ./...
```

Cubren el fichero (round-trip, modo `0600`, JSON sin secretos, ausencia) y el CLI contra `httptest`: start→exec reutilizando el mismo id (un solo `POST /v1/sandboxes`), `--` argv, rechazo si no hay sesión, `--force`, y `stop` que no borra el JSON si el CP responde 500. No requieren ncc1701d ni FakeVMM levantado: el HTTP está simulado. El camino FakeVMM real sigue siendo `make smoke-asp` para `sandbox run`, no para esta sesión.

## Referencias

- [`ops-asp-agent-runner.md`](ops-asp-agent-runner.md) — one-shot + IdP
- [`why-cli-asp.md`](why-cli-asp.md)
- [`roadmap.md`](roadmap.md) — fase 2f y nota de sesión
- [ADR-0007](adr/0007-multi-user-identity.md)
