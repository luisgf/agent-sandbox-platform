# Por qué / Qué ganamos — Sesiones de agente

Dirección: [`adr/0009-agent-sessions.md`](adr/0009-agent-sessions.md) (aceptada; seguimiento 2026-10-03 en el mismo ADR).  
Contrato CLI: [`ops-asp-session.md`](ops-asp-session.md).  
La primitiva que **no** es el producto: [`ops-asp-agent-runner.md`](ops-asp-agent-runner.md) (`asp sandbox run`).  
Identidad del dueño: [`adr/0007-multi-user-identity.md`](adr/0007-multi-user-identity.md).

## Por qué

Un agente es un proceso largo. OpenCode (y cualquier harness parecido) no lanza un comando y se va: llama al shell decenas de veces, con el resultado anterior todavía en disco, durante horas.

`asp sandbox run` hace create → exec → destroy. Sirve para un one-liner de CI o de ops. Como forma de «usar el aislamiento» dentro del bucle del agente, falla:

1. Cada tool paga el arranque de la microVM. Con Cloud Hypervisor eso es la latencia. Con FakeVMM el coste se esconde y el diseño parece inocuo.
2. El guest muere entre medias. No hay workspace acumulado **dentro** del guest: lo que el comando N instaló no existe en el N+1.
3. El harness, si el sandbox es caro, ejecuta el shell en el host. El producto deja de existir justo cuando el código no es de confianza.

La sesión (`asp session`) es el objeto con el que el agente usa esa frontera. El one-shot se queda para CI y para un comando que debe morir con la VM.

Tres fricciones del primer corte ya no se pueden dejar en «después» si el harness es real:

- Un solo `~/.cache/asp/session.json` hace que dos agentes en el mismo `$HOME` se pisen.
- El exec devolvía un JSON al terminar. El tool no veía nada hasta el `exit`.
- «Workspace» se leía como el checkout del host. No lo era.

## Qué ganamos

- **Un objeto claro:** la sesión. Un agente, un sandbox, mientras dure el trabajo. Varios agentes locales = varios nombres (`--name`), no un fichero compartido.
- **Identidad estable:** `owner_sub` lo pone el CP desde el JWT del IdP al hacer `start`. No viaja en el JSON de sesión y el guest no lo elige.
- **Disco del guest** entre execs (ficheros, procesos, `/tmp`). Eso sigue siendo el workspace que de verdad persiste.
- **Spec de directorio del host:** `asp session start --workspace /ruta/absoluta` guarda `workspace_host_path` en el sandbox. El nodo lo copia al `MicroVMConfig`. FakeVMM lo registra. El punto de montaje previsto en el guest es `/workspace` (tag virtiofs `workspace`).
- **Egress de esa microVM** durante toda la sesión, no rearmado por comando.
- **Fin explícito o por idle:** `asp session stop`, o el reaper del CP si `ASP_SANDBOX_IDLE_TIMEOUT` está encendido (lab: `2h`; el binario por defecto lo tiene **apagado**). El reloj inicial lo pone el `start` (create y paso a `running`). Después, **solo un exec que el CP proxyó bien** lo refresca — JSON acumulado o stream NDJSON terminado. `status`, `GET`, heartbeat y renew **no**.
- **Salida mientras el comando corre:** `asp session exec` (sin `--buffered`) imprime stdout/stderr según llegan líneas NDJSON. `--buffered` y `--json` conservan el cuerpo JSON de siempre, que es el que usan los smokes.
- **Enganche del harness sin plugin:** el shell del tool apunta a `asp session exec --name …`. No hay binario de OpenCode en este repo.
- **La primitiva one-shot no se tira.** CI, smokes y un comando que debe morir con la VM siguen en `asp sandbox run`.

```text
start --name (una vez) → tools horas vía exec (stream) → stop --name  |  idle reap
         ▲                              │
         owner_sub, egress, disco guest ┘
         workspace_host_path = spec, no un mount KVM
```

## Qué no ganamos (límites honestos)

- **KVM no monta el workspace.** El control-plane persiste la ruta y el reconciler la anota. **No arranca virtiofsd** y el `vm.create` de Cloud Hypervisor **no lleva dispositivo `fs`** salvo que alguien rellene `WorkspaceFSSocket` a mano (el reconciler no lo hace). FakeVMM y dry-run no crean un directorio dentro de un guest. Decir que `--workspace` «ya comparte el repo» sería mentira.
- **No hay plugin de OpenCode.** Hay un wrapper de ejemplo en [`ops-asp-session.md`](ops-asp-session.md) (`--name` + exec en streaming). Hay que cablearlo fuera de este repo.
- **El exec sigue sin ser un PTY.** No hay stdin interactivo, ni TTY, ni websocket. El stream es NDJSON de stdout/stderr por el mismo `POST /exec`. Un tool que espera un terminal se sigue rompiendo.
- **Si el guest es un pod-daemon viejo** (sin `?stream=1`), el node-agent convierte el JSON final en un único burst NDJSON. Eso no es byte a byte; el camino vivo es el pod-daemon nuevo (chunked).
- **Idle global y opcional.** Sin `ASP_SANDBOX_IDLE_TIMEOUT`, una sesión olvidada no se apaga sola. No hay umbral por sesión. No añadimos heartbeat.
- **Estado compartido es también el riesgo:** un tool deja basura o un proceso para el siguiente. Quien necesite borrar eso usa la primitiva one-shot.
- **Dry-run no demuestra KVM.** Los tests de sesión y de stream son `httptest` (y `cargo test` del pod-daemon). `make smoke-asp` sigue cubriendo `sandbox run` contra FakeVMM, no esta sesión.

Detalle normativo y alternativas: el ADR-0009, sección de seguimiento.
