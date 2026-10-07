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
| `asp session exec --name` | Mismo id. Por defecto **stream NDJSON**: stdout/stderr se imprimen al llegar. Exit code del proceso = `exit_code` del evento `exit`. `--cmd '…'` **o** `-- argv…`. `--buffered` o `--json` piden el JSON acumulado de siempre (smokes). Solo un sandbox `running` acepta exec: en `requested`/`starting` el CP responde 409 (`sandbox is starting; wait until it is running`) sin llamar al nodo, y también 409 si el node-agent ya no tiene la VM. |
| `asp session status --name` | Lee ese fichero y hace `GET`. `--json` opcional. Si no hay fichero, error claro (exit 1). |
| `asp session stop --name` | `POST …/stop`: el nodo apaga el guest y **conserva el disco** ([ADR-0012](adr/0012-retained-disks.md)). Espera `stopped` (`--no-wait` no espera). **El fichero se queda**: la sesión sigue existiendo, parada. 404 y 409 (failed, deleting) lo explican y no tocan el fichero. |
| `asp session resume --name` | `POST …/start`: arranca la sandbox parada **en su nodo** con su disco (`boot_count` + 1) y espera `running`. 503 si ese nodo no tiene hueco, 409 si está en cordon, caído o la sandbox no está parada. Un arranque que falla la deja `stopped` con el motivo (`status_detail`); un disco que falta la deja `failed` (`disk_lost`). Si la sesión tenía `--local-net`, el túnel se retiró al parar: `session local-net up` otra vez. |
| `asp session rm --name` | `DELETE` + borra ese fichero: la sandbox y su disco se van (`deleting` → `deleted`). 404 (ya borrada) limpia el fichero igual. Otro error HTTP **conserva** el fichero para reintentar. Es lo que hacía `stop` antes de ADR-0012. |
| Auth | Igual que el resto del CLI: Bearer IdP automático (`asp auth` / `ASP_ID_TOKEN` / cache). No se copia al fichero de sesión. |
| `--force` en start | Si ya hay sesión **con ese nombre**, **borra** el sandbox anotado (y su disco) y empieza otro. Sin `--force`, start falla y no crea un segundo sandbox a ciegas; el mensaje dice si lo que quieres es `resume` o `rm`. |
| `--workspace /ruta` | Ruta **absoluta** de un directorio que debe existir en la máquina del CLI y, en el arranque real, en el nodo. Viaja como `workspace_host_path`. Si no está vacío, el node-agent arranca `virtiofsd` y Cloud Hypervisor recibe `fs` con tag `workspace`. Una imagen guest construida con este corte lo monta sola en `/workspace` (`workspace-virtiofs.service`). Una imagen anterior sigue necesitando el `mount` a mano. |
| `--local` en rm | Borra solo el fichero. El sandbox y su disco **siguen** en el CP. Escape de ops, no el camino normal. |

Dos nombres son dos sandboxes. No comparten disco ni egress.

## Qué no ganamos (límites reales)

| Límite | Realidad |
|---|---|
| La CLI no es un plugin de OpenCode | No registra tools ni habla el protocolo del harness. Es un binario que el harness **exec**. El wrapper y el plugin de [`integrations/opencode/`](../integrations/opencode/) se instalan aparte. |
| El nombre es local | Otro host, otro contenedor o un `HOME` distinto no ve el directorio. El CP sí sigue teniendo el sandbox. El nombre no es un id global. |
| Imagen vieja no auto-monta | El dispositivo sí se crea cuando hay workspace y `virtiofsd` está en el nodo. El tag es `workspace` y el punto de montaje es `/workspace`. La imagen **nueva** lo monta al boot (`workspace-virtiofs.service`, oneshot, sale 0 si el tag no está). Una imagen construida antes de esa unidad no ejecuta el `mount`: el exec sigue viendo solo el disco del guest hasta el comando manual, o hasta reconstruir el rootfs. |
| La ruta es la del nodo | El CLI comprueba que el path exista en **su** máquina. Si el node-agent corre en otro host, la cadena guardada puede no existir allí. El CP no hace `stat`, y con varios nodos el planificador no sabe en cuáles existe: fija el nodo con `--node-id` o comparte la ruta en todos ([`ops-multi-node.md`](ops-multi-node.md)). |
| PTY con límites | `asp session exec` (sin `--buffered`) pide PTY en el guest salvo `--no-pty`. Si stdout local es una TTY, el CLI pasa a raw y reenvía stdin. Un pipe también se reenvía. El protocolo sigue siendo NDJSON (`ready`, `stdout`, `stderr`, `exit`) más `POST .../exec/stdin`. No es un SSH ni un websocket. Ver la sección de virtiofs y PTY. |
| Guest viejo | Si el pod-daemon responde 404 a `?stream=1`, el node-agent hace el exec JSON y lo reescribe como un solo burst NDJSON al final. No es streaming real. Hace falta el pod-daemon de este cambio dentro de la imagen. |
| Binario en el stream | Los trozos pasan por JSON string (UTF-8 con reemplazo). No es un pipe de bytes opacos. El JSON acumulado tampoco lo era (`read_to_string`). |
| GC solo en el CP, y solo si está encendido | Este CLI **no** apaga sandboxes por su cuenta. El control-plane puede hacerlo con `ASP_SANDBOX_IDLE_TIMEOUT` (recomendado `2h`; default del proceso **off**). Si el reaper paró el sandbox, `session status` y `session exec` lo dicen (`idle timeout` / `idle_reaped=true`, exit 1) y **no** borran el JSON. `start --force` (mismo `--name`) crea otro. |
| No aísla herramientas entre sí | Todos los `exec` de esa sesión comparten un guest. Eso es la ventaja y el riesgo. |
| Auth sigue siendo por invocación | Cada `exec` resuelve el Bearer de nuevo. Si el token caduca, el siguiente comando falla con 401. El sandbox no se entera. |
| `owner_sub` | Lo pone el CP desde el JWT ([ADR-0007](adr/0007-multi-user-identity.md)). La sesión no acepta un owner de mentira en el JSON local. |
| Dry-run ≠ KVM | Tests de sesión y de stream = `httptest`. El pod-daemon se cubre con `cargo test`. El auto-mount se cubre leyendo la unidad y el helper (sale 0 si `mount` falla). No hace falta ncc1701d para eso. Aislamiento real y el share de verdad solo con CH + KVM y un rootfs que lleve la unidad. |

## Alternativas que descartamos (y la consecuencia)

1. **Seguir solo con `asp sandbox run`.** Máximo aislamiento por comando y cero estado local. Consecuencia: cada tool espera create+boot+destroy. Sigue siendo el comando de CI. No lo quitamos. Su exec sigue siendo el JSON acumulado (no hace falta stream para un one-shot que igual espera al final).
2. **Plugin OpenCode que hable HTTP con el CP.** Evitaría el wrapper shell. Consecuencia: duplicar auth, wait y el cliente; este repo no mantiene ese plugin. La CLI es el contrato estable. El plugin de `integrations/opencode/` no habla con el CP: solo ajusta cómo OpenCode llama al wrapper y lo que le cuenta al modelo.
3. **SSH largo al guest como sustituto del exec.** Un PTY sobre el exec NDJSON evita abrir otra superficie de auth. Consecuencia: no es un terminal completo (sin SIGWINCH, stderr mezclado en el PTY, EOF por Ctrl-D). SSH al guest sigue fuera.
4. **Un solo `session.json`.** Menos flags. Consecuencia: dos agentes se pisan. Por eso el directorio y `--name`. El fichero explícito queda como override, no como default.
5. **Guardar el JWT en el JSON.** Arranque más simple sin `asp auth`. Consecuencia: un fichero robado es un token. Prohibido.
6. **Meter `fs` en CH sin un `virtiofsd` vivo, o fingir el directorio dentro de FakeVMM.** Un socket vacío rompe el boot. Sin workspace el `vm.create` sigue sin `fs`. Con workspace y sin binario, el sandbox pasa a `failed` en lugar de arrancar «a medias».

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
    ├─ asp session stop --name agente    el nodo apaga el guest; el disco y el JSON se quedan
    ├─ asp session resume --name agente  arranca la sandbox parada en su disco, en su nodo
    └─ asp session rm --name agente      DELETE (VM y disco), luego borra el JSON
```

Diagnóstico (ids, transiciones, errores) va a **stderr**. El id de `start`/`stop` y la salida del comando van a **stdout**, para no mezclarlos si el harness captura stdout como resultado del tool. En el camino stream, stdout del guest y el id no se mezclan porque el id solo lo imprime `start`/`stop`.

## Cómo lo apuntaría OpenCode

OpenCode lanza cada llamada a la herramienta bash como `<shell> -c "<comando del modelo>"` en el host, y la opción `"shell"` de `opencode.json` elige ese binario (ruta absoluta). Para que el tool entre al sandbox hay que **sustituir ese shell** por un wrapper. El kit está en [`integrations/opencode/`](../integrations/opencode/), y la guía paso a paso (en inglés) en [README § Using ASP with OpenCode](../README.md#using-asp-with-opencode):

- `asp-opencode-shell`: el wrapper. Pasa el comando a `asp session exec --no-pty -- /bin/sh -c` y devuelve el exit code del guest. El pod-daemon del guest ejecuta como el dueño de `/workspace` (o como `sandboxd` si el workspace es de root), para que los ficheros nuevos conserven el uid del host: virtiofs no traduce ids. `ASP_GUEST_AS_ROOT=1` pide root (`asp session exec --root`). Una imagen anterior ejecuta todo como root e ignora la petición.
- `plugins/asp-sandbox.ts`: un plugin de OpenCode. Se niega a ejecutar si falta el wrapper (OpenCode caería sin avisar al shell del host), traduce `workdir` a un directorio del guest y le dice al modelo que bash corre en Linux dentro del sandbox.
- `instructions/` y `opencode.*.json`: instrucciones para el modelo y configuración de ejemplo para dos montajes. En **solo bash**, OpenCode corre en otra máquina y sus herramientas de ficheros quedan denegadas. En **mismo host**, OpenCode corre en el nodo y edita el repo compartido por virtiofs.

**`--cmd` no es un shell.** Solo separa palabras con comillas simples y dobles; no interpreta `|`, `&&`, `;`, redirecciones ni variables. El modelo genera esas construcciones todo el rato, así que el wrapper pasa el comando a `/bin/sh -c` **dentro del guest**. Un wrapper con `--cmd "$*"` rompe cualquier tubería.

Lo que no arregla el kit:

- **Mismo exit code.** OpenCode junta stdout y stderr, y un error de `asp` (`no active session`, 401, red) sale con 1, igual que un comando que falla. El modelo distingue el caso por el mensaje. Los errores de uso del wrapper salen con 125 y los de ssh con 255.
- **Ficheros sin compartir.** Sin virtiofs entre la máquina de OpenCode y el nodo, las herramientas de ficheros no pueden ver el repo del guest. Por eso el montaje solo bash las deniega.
- **`opencode run` sin terminal.** Espera EOF en stdin. En scripts hay que añadir `< /dev/null`.

Secuencia de operador / agente:

```bash
export ASP_CP_URL=http://127.0.0.1:8080   # dry-run local; lab: http://127.0.0.1:18112
# lab IdP: export ASP_IDP_REQUIRED=1  y secretos en ~/.secrets/ (ver ops-asp-agent-runner.md)
asp session start --name opencode --workspace /ruta/absoluta/del/repo \
  --tenant=tenant-demo --timeout=120s
# el plano de control elige un nodo con hueco; --node-id=… lo fija
# stdout: <sandbox id>
# Imagen nueva: /workspace ya está montado al boot (tag workspace).
# Imagen anterior a workspace-virtiofs.service, dentro del guest:
#   mkdir -p /workspace && mount -t virtiofs workspace /workspace

asp session exec --name opencode --cmd 'echo hello-from-session'
asp session exec --name opencode -- echo hello --flag

asp session status --name opencode
asp session stop --name opencode      # conserva el disco
asp session resume --name opencode    # lo arranca otra vez
asp session rm --name opencode        # borra sandbox, disco y JSON
```

En el lab Keycloak el tenant de ejemplo de los one-liners existentes es `--tenant=default` (no `tenant-demo`, que es el default del flag). El Bearer se adjunta solo; no hace falta meter el JWT en el wrapper.

Si el harness no puede cambiar el binario del shell y solo puede prefijar un comando:

```text
asp session exec --name opencode --cmd '<comando>'
```

`--cmd` es **un** argumento y no pasa por un shell (ver arriba). Sin comillas, el resto de argv pisa a `--cmd`. Para tuberías o `&&`: `asp session exec --name opencode -- /bin/sh -c '<comando>'`.

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
| `session status`, `GET`, heartbeat del nodo, sondeo de `/work` | No. Si contaran, el reconciler impediría el idle para siempre. |
| Exec que falla antes del guest (red, 502, stream cortado a medias) | No. |

**Qué hace el reaper.** **Para** el sandbox: `stopping` (el node-agent apaga el guest y conserva el disco) o `stopped` si ningún nodo la había reclamado todavía. No borra nada: `asp session resume` la trae de vuelta. `stop_reason=idle_timeout`. Evento `sandbox.idle_reaped`.

**Qué ve esta CLI.** `asp session status --name …` imprime `idle_reaped=true` (y `--json` el booleano), dice que el disco se conserva y sale **1**. `asp session exec` no llama al exec proxy si el GET ya trae `stop_reason=idle_timeout`. El fichero de ese nombre no se borra solo.

**Límites.** Un umbral de proceso para todos los sandboxes. Filas ya existentes al aplicar la migración `008` empezaron el reloj en ese momento, no en el `created_at` histórico.

## Variables y flags

| Nombre | Rol |
|---|---|
| `--name` | Nombre de la sesión. Default `default`. Un segmento `[A-Za-z0-9][A-Za-z0-9._-]{0,63}`. |
| `ASP_SESSION_DIR` / `--session-dir` | Directorio de los `<nombre>.json`. Default `~/.cache/asp/sessions`. |
| `ASP_SESSION_FILE` / `--session-file` | Path de un solo JSON. Si está, **ignora** el nombre para elegir fichero. Ya no es el default. |
| `--workspace` | Solo `start`. Directorio absoluto. El nodo lo exporta con virtiofsd si el binario existe. La imagen nueva monta el tag al boot; la vieja, a mano. |
| `--buffered` | Solo `exec`. JSON acumulado, sin PTY y sin stream. Un stdin que no sea TTY se manda en el campo `stdin` (hasta 1 MiB). |
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
| `active session …` en start | Ya hay JSON para ese nombre. `resume --name` si está parada, `rm --name` para borrarla, o `start --force --name`. |
| `invalid session name` | El nombre tiene `/`, espacios o `..`. |
| exec HTTP 404 | Alguien borró el sandbox y el JSON sigue. `rm` (limpia en 404) o `start --force`. |
| `idle timeout` / `idle_reaped=true` | El reaper paró el sandbox y **conservó su disco**. El JSON local sigue. `asp session resume --name …`. |
| `sandbox is stopped; its disk is kept` | Alguien paró la sesión. `asp session resume --name …`. |
| `no capacity on the node that holds the disk` al reanudar (503) | El nodo del disco está lleno. La sandbox sigue parada: reintenta, o libera hueco en ese nodo. No se puede mover el disco a otro. |
| `disk_lost` al reanudar | Falta el disco en el nodo (alguien lo borró, o el plano de control olvidó la sandbox y el GC del nodo lo recogió). No hay vuelta atrás: `asp session rm` y `start`. |
| `no capacity: …` en start (503) | Ningún nodo tiene hueco; el mensaje cuenta por qué se descartó cada uno (`max_sandboxes`, `insufficient_memory`, `cordoned`, `stale`…). Reintenta, o que un admin añada nodos o haga `uncordon` (`asp node list`). |
| `node pin rejected: …` en start (409) | `--node-id` apunta a un nodo desconocido, caído, revocado o en cordon. Quita el pin o revisa ese nodo. |
| `lost_with_node=true` / `sandbox was lost with its node` | Estaba `failed` porque su nodo dejó de dar señales (`node_lost`): el disco vivía en ese servidor, `asp session start --force --name …`. |
| `was stopped when the node agent restarted` / `…when its node stopped responding` | Es una sandbox `stopped` (`node_agent_restarted`, o `node_lost` si se estaba parando): su VM murió pero **el disco se conserva** en el nodo. `asp session resume` (cuando el nodo vuelva, si es `node_lost`). `start --force` la borraría: pide `--yes` a propósito. |
| exec 401 | Token caducado o `ASP_IDP_REQUIRED` sin secretos. `asp auth status`. |
| stdout vacío y exit ≠ 0 | El guest falló sin stdout; el código es el `exit_code`. El error del CLI (red, 500, stream sin evento `exit`) es exit **1**, no el código del guest. |
| `exec stream: missing exit event` | El proxy cortó el NDJSON. No hubo `exit_code`. La actividad **no** se refresca. |
| `state file kept` | `DELETE` falló (no 404). El JSON sigue para reintentar `rm`. |
| El guest no ve `/workspace` | Imagen nueva: `systemctl status workspace-virtiofs` en el guest. Si el tag no estaba, la unidad sale 0 y no hay mount (sandbox sin workspace, o `virtiofsd` no arrancó). Imagen vieja, sin esa unidad: `mkdir -p /workspace && mount -t virtiofs workspace /workspace`. Si el start falló con `virtiofsd`, el binario no está en el nodo (`--virtiofsd-bin` / `VIRTIOFSD_BIN`). |
| Salida de golpe al final | `--buffered`, `--json`, o un pod-daemon que no habla `?stream=1` (el node-agent emite un burst). |


## Virtiofs y PTY — qué aterrizó

### Por qué

El spec `workspace_host_path` sin daemon era una etiqueta: el guest no veía el checkout y un `fs` con socket vacío habría tumbado el boot. El stream NDJSON sin stdin tampoco servía para un shell. Este corte cierra las dos piezas que se pueden probar sin KVM (el dispositivo en el payload, el protocolo) y arranca el daemon de verdad cuando el nodo lo tiene.

### Qué ganamos

| Pieza | Comportamiento |
|---|---|
| `virtiofsd` por sandbox | Solo si `workspace_host_path` no está vacío. Socket `virtiofs-{id}.sock` bajo el directorio de sockets del nodo (`--ch-socket-dir`, default `/run/asp`). Argumentos: `--socket-path`, `--shared-dir`, `--cache never`, `--sandbox none`. Binario: `--virtiofsd-bin` o `VIRTIOFSD_BIN` (default `virtiofsd`, CLI Rust). |
| `vm.create` | `fs: [{ "tag": "workspace", "socket": "…" }]`. Sin workspace, o con socket vacío, el campo `fs` no va en el JSON. |
| Fallo cerrado | Workspace pedido y `virtiofsd` ausente, o el directorio no existe en **el nodo**: el sandbox pasa a `failed` y no se llama al VMM. Sin workspace, no se busca el binario. |
| Mount del guest | Automático en la imagen de este corte: `workspace-virtiofs.service` ejecuta `mkdir -p /workspace` y `mount -t virtiofs workspace /workspace`. Si el tag no está, el helper sale 0 y el boot sigue. Imágenes ya desplegadas (p. ej. la de ncc1701d) **no** lo hacen hasta reconstruir el rootfs; ahí sigue valiendo el comando a mano. |
| PTY | `asp session exec` manda `"pty":true` con `rows`/`cols` de la TTY local (si las hay). El pod-daemon abre `/dev/ptmx`, hace `setsid` + `TIOCSCTTY` y el slave es stdin/stdout/stderr. El primer evento del stream es `{"type":"ready","exec_id":"…"}`. |
| Stdin | `POST /v1/sandboxes/{id}/exec/stdin` con `{"exec_id","data","close","rows","cols"}`. El CP lo proxya a `POST /v1/internal/exec/stdin` y eso al guest `POST /v1/exec/stdin`. Un `close` en pipe cierra el write end (EOF real). En PTY escribe dos Ctrl-D (modo canónico). Cada POST de stdin refresca `last_activity_at`. |
| JSON acumulado | `POST /exec` sin `?stream=1` sigue devolviendo `{stdout,stderr,exit_code}`. `--buffered` / `--json` no piden PTY. Si hay stdin por pipe, viaja en el campo `stdin`. |

### Qué no ganamos

- El auto-mount vive en la **imagen**, no en el nodo. Un rootfs anterior no tiene `workspace-virtiofs.service`: hasta reconstruirlo, `/workspace` no aparece solo. Sin tag (no hubo `--workspace`) la unidad no falla el boot; el directorio se crea vacío y no es el checkout del host.
- `--sandbox none` no mete a virtiofsd en un user namespace. La superficie es el directorio pedido y los privilegios del node-agent.
- Hace falta el `virtiofsd` Rust en el nodo. El helper C viejo (`-o source=`) no vale. Sin KVM estos tests no arrancan la VM: comprueban socket y tag.
- El PTY mezcla stderr en el master. No hay evento `stderr` separado en ese modo.
- No hay `SIGWINCH`. El tamaño se fija al empezar; un `rows`/`cols` posterior en `/exec/stdin` sí hace `TIOCSWINSZ`, pero el CLI no lo manda al cambiar la ventana.
- El EOF del PTY es Ctrl-D, no un hangup. Un programa en raw mode no ve fin de stdin. Para un pipe de verdad: `--no-pty`.
- Los bytes van en un string JSON (UTF-8 con reemplazo). No es un pipe opaco.
- El stream no tiene límite de tiempo en ningún tramo: CLI, plano de control ([timeouts hacia el node-agent](../control-plane/README.md#timeouts-hacia-el-node-agent)), node-agent y pod-daemon lo dejan durar lo que el comando. Si el CLI se va, el guest mata el comando. `--exec-timeout-secs` (default 30) solo limita el exec acumulado (`--buffered`, `--json`); `--stream-idle-timeout-secs` en la imagen añade un límite por inactividad (sin salida ni stdin). Cada POST de stdin sí usa el cliente JSON con su timeout.
- El reaper sigue contando el exec al terminar el stream. Las teclas refrescan actividad; un PTY en silencio, no.
- Hace falta el pod-daemon de este corte. Uno viejo ignora `pty` y no emite `ready`: el stdin no se reenvía y el stream se degrada al burst NDJSON.
- FakeVMM no crea el árbol dentro de un guest. Registra la ruta y, si el test (o un launcher) rellena el socket, el tag. No bootea KVM.

## Tests

```bash
cd cli && go test ./...
cd control-plane && go test ./...
cd node-agent && go test ./...
cd pod-daemon && cargo test
```

Cubren el directorio de sesiones (nombres distintos, modo `0600`, rechazo de `../`), el CLI contra `httptest` (start→exec reutilizando el id, `--buffered`, stream que entrega el primer chunk **antes** de que el servidor cierre el body, `--workspace` en el POST, PTY+stdin: el cliente manda bytes **después** del evento `ready`), el proxy del CP y del node-agent con el mismo patrón, el fallback 404→JSON, FakeVMM **sin** `fs` cuando no hay workspace, y que con workspace el config lleva socket y tag `workspace` aunque aquí no haya KVM. El `cargo test` del pod-daemon comprueba el JSON acumulado, el stream chunked, un pipe de stdin y un PTY real (`/bin/sh -c cat`). No requieren ncc1701d. El camino FakeVMM de `sandbox run` sigue siendo `make smoke-asp`.

## Referencias

- [ADR-0009](adr/0009-agent-sessions.md) — dirección y seguimiento
- [`why-agent-sessions.md`](why-agent-sessions.md)
- [`ops-asp-agent-runner.md`](ops-asp-agent-runner.md) — primitiva one-shot + IdP
- [`bare-metal-ch.md`](bare-metal-ch.md) — CH real; el nodo arranca virtiofsd si hay workspace
- [`why-virtiofs-pty.md`](why-virtiofs-pty.md) — por qué este corte
- [`why-cli-asp.md`](why-cli-asp.md)
- [`roadmap.md`](roadmap.md)
- [ADR-0007](adr/0007-multi-user-identity.md)
