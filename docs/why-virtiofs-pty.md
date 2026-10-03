# Por qué virtiofs del workspace y PTY en el exec

Nota corta del corte que sigue a las sesiones con nombre. Contrato operativo: [`ops-asp-session.md`](ops-asp-session.md). Decisión de producto: [ADR-0009](adr/0009-agent-sessions.md).

## Por qué

Dos mentiras cómodas se habían quedado en el código a propósito, escritas como límite:

1. `--workspace` guardaba una ruta y el reconciler **no** arrancaba `virtiofsd`, así que Cloud Hypervisor no recibía `fs`. Decir que el agente «ve el repo» era falso. Meter el dispositivo sin socket habría roto el boot.
2. El exec en NDJSON no tenía stdin ni TTY. Un `cat` o una shell no podían leer al usuario. Un SSH aparte habría sido otra auth.

Hacía falta el daemon de verdad y un protocolo de stdin que los `httptest` puedan probar sin KVM.

## Qué ganamos

- **Share real cuando el nodo tiene `virtiofsd`.** Un proceso por sandbox, socket `virtiofs-{id}.sock`, tag `workspace`. `vm.create` incluye `fs` solo entonces. Sin workspace no se busca el binario y no hay `fs` (FakeVMM y CH iguales).
- **Fallo visible.** Si el workspace está pedido y el binario no está, o el directorio no existe en el nodo, el sandbox queda `failed`. No arrancamos una VM que finja el directorio.
- **PTY en el guest** cuando `asp session exec` no lleva `--buffered` ni `--no-pty`. El pod-daemon abre el PTY. El CLI, si su stdout es una TTY, se pone en raw y reenvía el teclado. Un pipe también entra: con PTY el `close` son dos Ctrl-D; con `--no-pty` es EOF de pipe.
- **El JSON de siempre sigue.** `sandbox run` y `--buffered` no cambian de forma. El campo opcional `stdin` mete un blob en ese camino.
- **Tests sin ncc1701d.** Socket y tag en el config de FakeVMM; `httptest` del protocolo `ready` + `/exec/stdin`; `cargo test` con un PTY de verdad contra `/bin/sh -c cat`.

## Qué no ganamos

- El guest **no** ejecuta el mount. Hay que hacerlo dentro:

  ```sh
  mkdir -p /workspace
  mount -t virtiofs workspace /workspace
  ```

- `virtiofsd` va con `--sandbox none` y `--cache never`. No hay user namespace. El binario esperado es el Rust (`--socket-path` / `--shared-dir`), no el helper C.
- El PTY no es un terminal de producto: stderr mezclado, sin SIGWINCH desde el CLI, sin bytes opacos, y el proceso muere a los `--exec-timeout-secs` del pod-daemon (default 30). Una sesión interactiva larga exige subir ese timeout en la imagen.
- El reaper no entiende «hay un PTY abierto» salvo por los POST de stdin que sí refrescan actividad, y por el final del stream.
