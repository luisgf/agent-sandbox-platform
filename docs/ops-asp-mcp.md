# Ops — Servidor MCP `asp mcp` (experimental)

`asp mcp` es un servidor [Model Context Protocol](https://modelcontextprotocol.io) por stdio. Sus herramientas actúan dentro del sandbox de una sesión ([`ops-asp-session.md`](ops-asp-session.md)). Cualquier harness que hable MCP (Claude Code, OpenCode, Codex, Cursor…) puede trabajar dentro de la microVM sin wrapper de shell propio.

Va dentro del CLI y no añade dependencias. Implementa la parte del protocolo que necesita un servidor de solo herramientas:

- `initialize`, que negocia la revisión 2025-06-18, 2025-03-26 o 2024-11-05;
- `tools/list`;
- `tools/call`, con llamadas concurrentes;
- `notifications/cancelled`;
- `notifications/progress`;
- `ping`.

## Herramientas

| Herramienta | Qué hace | Cómo |
|---|---|---|
| `asp_exec` | Ejecuta un comando con `/bin/sh -c` (tuberías, `&&` y redirecciones funcionan). Devuelve el código de salida, stdout y stderr. Admite `cwd`, `env` y `timeout_seconds`. | Exec en streaming: sin el tope de 30 s del exec con buffer. Mientras corre, avisa cada 5 s con `notifications/progress` si el cliente lo pidió. Al vencer el timeout el comando se para. La salida conserva cabeza y cola, 30 000 bytes por flujo por defecto (`--max-output`). |
| `asp_read` | Lee un fichero de texto con líneas numeradas (estilo `cat -n`), con `offset` y `limit`. | Un script fijo con argumentos posicionales: las rutas nunca pasan por un shell. |
| `asp_write` | Crea o sobrescribe un fichero con el contenido exacto y crea los directorios padre. | El contenido viaja en base64 por el stdin del exec, así que no hay escapes ni comillas. Hasta 512 KiB: el control plane lee como mucho 1 MiB de cuerpo. |
| `asp_edit` | Reemplaza un texto exacto: único, salvo `replace_all`. | Lee en base64, sustituye en el CLI y reescribe. Ficheros de hasta 512 KiB. |
| `asp_list` | Árbol de un directorio: tipo, tamaño y ruta, sin `.git`. | `find -printf` (GNU, el del guest Debian). |
| `asp_grep` | Busca con `grep -rnE`, con `include`, `ignore_case` y `max_results`. | |
| `asp_session_info` | Id, estado, nodo, imagen, recursos y workspace del sandbox. | `GET /v1/sandboxes/{id}` |

Las rutas relativas parten de `/workspace` si la sesión tiene workspace y de `/` si no. Cada llamada crea un cliente nuevo y vuelve a resolver el Bearer, porque un servidor dura más que un access token.

## Arranque

```bash
asp mcp --name agente                       # sesión ya arrancada con asp session start
asp mcp --name agente --start --workspace /ruta/del/repo --stop-on-exit
```

- `--start`: si la sesión no existe o su sandbox terminó, hace `asp session start` (con `--force` en el segundo caso). Usa `--workspace`, `--image`, `--tenant` y `--cp-url` si se pasan.
- `--stop-on-exit`: para la sesión cuando el cliente cierra stdin. Con `--start` el sandbox vive lo que vive el harness.
- `--exec-timeout` (por defecto `2m`) es el timeout de `asp_exec` cuando la llamada no pide otro; el máximo es 1 h.
- Solo los mensajes MCP salen por stdout. Los logs y el progreso del arranque van a stderr.

## Configuración por harness

En todos, conviene desactivar las herramientas nativas de shell y ficheros: si no, el modelo puede seguir usando el host.

**Claude Code** (probado):

```bash
claude mcp add asp -- asp mcp --name agente --start --workspace "$PWD" --stop-on-exit
claude --allowedTools mcp__asp --disallowedTools "Bash,Read,Write,Edit,MultiEdit,Glob,Grep,NotebookEdit"
```

**OpenCode** (`opencode.json`, no probado):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "asp": { "type": "local", "command": ["asp", "mcp", "--name", "opencode", "--start", "--workspace", "/ruta/del/repo", "--stop-on-exit"] }
  },
  "tools": { "bash": false, "write": false, "edit": false }
}
```

**Codex** (`~/.codex/config.toml`, no probado):

```toml
[mcp_servers.asp]
command = "asp"
args = ["mcp", "--name", "codex", "--start", "--stop-on-exit"]
```

**Control plane sin acceso directo** (como en el lab, donde solo escucha en `127.0.0.1` del nodo): el servidor puede correr en el nodo detrás de `ssh`, porque stdio atraviesa SSH sin cambios:

```json
{ "mcpServers": { "asp": { "command": "ssh", "args": ["-o", "BatchMode=yes", "usuario@nodo",
  "ASP_CP_URL=http://127.0.0.1:18112 ASP_IDP_REQUIRED=1 exec asp mcp --name agente --start --stop-on-exit"] } } }
```

## Límites

| Límite | Realidad |
|---|---|
| Sin salida en vivo | El modelo recibe el resultado al terminar la llamada. El progreso solo mantiene viva la llamada (OpenCode no lo muestra). |
| Permisos por herramienta | El harness aprueba o deniega `asp_exec` entero. No hay patrones como `Bash(git *)`. |
| Herramientas nuevas | Una herramienta MCP no puede llamarse `bash`. El modelo tiene que usar `asp_*`; las instrucciones del servidor se lo dicen. |
| Tamaños | Escritura y edición hasta 512 KiB. La salida de `asp_exec` se recorta a cabeza y cola. Los binarios no: el exec devuelve texto UTF-8 con reemplazo. |
| Ficheros del host | Lo que se escribe desde el guest aparece en el workspace del host como `root:root` (exec como root, virtiofs sin traducción de uid). |
| Concurrencia | Las llamadas corren en paralelo. Un cliente que encadene escritura y edición sin esperar la respuesta puede desordenarlas. |
| PTY | No hay herramienta interactiva; para eso sigue `asp session exec`. |

## Probado

Probado el 6 de octubre de 2026 en ncc1701d (KVM, Keycloak obligatorio):

- Un cliente MCP secuencial cubrió `--start` y `--stop-on-exit`, las siete herramientas, progreso en un comando de 7 s, timeout que corta el comando y una salida de 1,2 MB recortada.
- Contenido con líneas `#`, comillas y `$((…))` se escribió y editó byte a byte.
- Claude Code headless, con solo las herramientas `asp_*`, hizo de punta a punta la misma tarea que antes se hacía con un wrapper `sbx` por SSH. El servidor MCP se arrancó por SSH con `--start --stop-on-exit`. La tarea era escribir `analyze.sh` en sh y awk, ejecutarlo sobre un `access.log`, dejar `REPORT.md` con pruebas del entorno y verificar las cifras por otra vía. El sandbox nació al conectarse el agente y se paró al terminar. Con las dos vías los resultados coinciden con los esperados:

  | | Wrapper `sbx` (shell por SSH) | `asp mcp` |
  |---|---|---|
  | Duración | 47 s | 37 s |
  | Llamadas | 7 | 7 (5 `asp_exec`, 1 `asp_write`, 1 descubrimiento) |
  | Comandos bloqueados por el harness | 2: heredocs con líneas `#` dentro de un argumento | 0: el script va en `asp_write` |
  | Coste del modelo | 0,41 $ | 0,34 $ |

Los tests de unidad cubren el protocolo (`cli/internal/mcp`) y las herramientas contra un control plane falso que ejecuta los scripts de verdad (`cli/cmd/asp/mcp_test.go`).
