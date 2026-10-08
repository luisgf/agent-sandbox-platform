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
- **Tests sin un host KVM.** Socket y tag en el config de FakeVMM; `httptest` del protocolo `ready` + `/exec/stdin`; `cargo test` con un PTY de verdad contra `/bin/sh -c cat`.

## Auto-mount en la imagen

El dispositivo lo pone el nodo. El `mount` lo pone el guest, y solo si la imagen lo trae.

**Por qué.** Dejar el `mount` como comando de ops hacía que el exec viera el disco del guest aunque `vm.create` ya llevara `fs`. El harness no tiene un paso fiable para entrar y montar antes del primer tool. Meter el mount en el nodo no se puede: el namespace es el de la VM.

**Qué hay.** `images/guest/systemd/workspace-virtiofs.service` (oneshot, `WantedBy=multi-user.target`, antes de `pod-daemon`). El helper `images/guest/helpers/mount-virtiofs-workspace.sh`:

1. `mkdir -p /workspace`
2. `mount -t virtiofs workspace /workspace` (unos reintentos cortos, por si el driver aparece un poco después de `local-fs`)
3. Sale **0** si el tag no está o el `mount` falla. Un sandbox sin workspace tiene que arrancar igual. La unidad no es `RequiredBy` de ningún target: no bloquea el boot. `TimeoutStartSec=15`.

El `Dockerfile` de `images/guest` copia la unidad y el helper y la habilita en `multi-user.target.wants`. Ejemplo OpenRC: `images/guest/openrc/workspace-virtiofs`.

**Imágenes viejas.** Un rootfs ya desplegado (u otro construido antes de esta unidad) no la tiene. Hasta reconstruirlo con `./scripts/build-guest-image.sh` (o instalar una versión con `asp image pull`) y apuntar `/opt/sandbox/rootfs.img` al nuevo fichero, dentro del guest sigue haciendo falta:

```sh
mkdir -p /workspace
mount -t virtiofs workspace /workspace
```

## Qué no ganamos

- Sin tag (no hubo `--workspace`, o `virtiofsd` no está y el start ni siquiera llegó a `running`) `/workspace` es un directorio vacío del disco del guest. El helper no falla el boot por eso.
- El rootfs que **ya** está arrancado no se reescribe. Hasta una imagen nueva (`build-guest-image.sh`, `asp image pull`) y un symlink nuevo, el comando manual sigue siendo el camino.
- `virtiofsd` va con `--sandbox none` y `--cache never`. No hay user namespace. El binario esperado es el Rust (`--socket-path` / `--shared-dir`), no el helper C.
- El PTY no es un terminal de producto: stderr mezclado, sin SIGWINCH desde el CLI, sin bytes opacos, y la sesión dura lo que el proceso: sin timeout total (solo el opcional `--stream-idle-timeout-secs` del pod-daemon); si el cliente se va, el guest mata el proceso.
- El reaper no entiende «hay un PTY abierto» salvo por los POST de stdin que sí refrescan actividad, y por el final del stream.
