# ADR-0012: Parar no es borrar — el disco de una sandbox sobrevive a la parada

- **Estado:** Aceptada (2026-10-07). Implementada en [#86](https://github.com/luisgf/agent-sandbox-platform/issues/86): nodo, plano de control y CLI, y retención con visibilidad. Probada en dry-run (smokes), con Postgres real y con tests del reconciler; la prueba con VMs reales en ncc1701d queda para el despliegue.
- **Fecha:** 2026-10-07
- **Extiende:** [0009](0009-agent-sessions.md) (el disco del guest es el workspace de la sesión «mientras vive»; esta ADR fija cuándo deja de vivir), [0011](0011-multi-node.md) (una sandbox parada queda fijada a su nodo)
- **Relacionados:** [`../architecture.md`](../architecture.md), [`../ops-asp-session.md`](../ops-asp-session.md), tarea de calentamiento de sandboxes [#81](https://github.com/luisgf/agent-sandbox-platform/issues/81)

## Contexto

Hoy el disco de una sandbox vive lo mismo que su VM. `asp session stop`, `asp sandbox delete` y el reaper de inactividad acaban en lo mismo: el nodo apaga la VM y borra `rootfs-<id>.img` (`teardownLocal` → `removeRootFS`). En el CLI, `stop`, `destroy` y `rm` son alias.

Para una sesión de agente es el ciclo de vida equivocado. Todo lo que el agente construye fuera de `/workspace` (paquetes instalados, cachés de npm, cargo o pip, compilaciones, su home) se pierde al parar la sesión. Cuando el reaper (`ASP_SANDBOX_IDLE_TIMEOUT`) para una sesión por la noche, a la mañana siguiente el agente reinstala durante minutos, no los 3–5 s de un arranque. La ADR-0009 ya define el disco del guest como el workspace de la sesión mientras vive; falta decidir cuándo muere.

Hay una trampa en el diseño actual: todo da por hecho que un disco sin VM viva es basura. El reaper de arranque del node-agent (`--reap-leftovers`) borra cualquier `rootfs-*.img` sin VM viva antes de registrarse, y la unidad systemd ejecuta lo mismo en `ExecStopPost`. Con discos que se conservan, cada reinicio del agente se los llevaría.

## Decisión

### 1. Dos operaciones distintas: parar y borrar

| Operación | Efecto |
|---|---|
| **Parar** (`POST /v1/sandboxes/{id}/stop`) | La VM se apaga de forma ordenada y el disco **se queda en el nodo**. `stopping → stopped`. Una sandbox nunca reclamada (`requested`) pasa directa a `stopped`. |
| **Reanudar** (`POST /v1/sandboxes/{id}/start`) | `stopped → requested → starting → running`, **en el mismo nodo**, con el mismo id, dueño, tenant, imagen, CPU, memoria y workspace. VM nueva, arranque nuevo, **mismo disco**. |
| **Borrar** (`DELETE /v1/sandboxes/{id}`) | `deleting → deleted`. El nodo apaga la VM si la hay y borra el disco. |
| **Reaper de inactividad** | Ahora **para** (`stop_reason=idle_timeout`): la sesión se puede reanudar. |
| **Retención** | Una sandbox parada más de `ASP_STOPPED_SANDBOX_TTL` se borra (`stop_reason=retention_expired`). |

`asp session stop` deja de borrar; el comando que borra es `asp session rm`. `asp sandbox run` sigue destruyendo al salir.

### 2. Qué persiste y qué no

Persiste el **disco raíz**, incluido `/tmp` mientras la imagen lo tenga en el rootfs (Debian no monta tmpfs en `/tmp` por defecto). No persiste: procesos, `/run`, memoria, CID de vsock, direcciones de la TAP (se reasignan), el enlace de red local (se retira al parar; `asp session local-net up` otra vez tras reanudar) ni la versión de la política de egress (se reaplica desde la política vigente antes del primer `exec`).

### 3. Estados

Se añaden `deleting` y `deleted`; `stopped` deja de ser final.

```text
requested → starting → running → stopping → stopped ──start──▶ requested
    ↘ stopped (parar antes del claim)            ↘ deleting → deleted
  cualquier estado salvo deleted ──DELETE──▶ deleting (si hay VM o disco en un nodo) | deleted (si no)
  failed (terminal: error de arranque, node_lost, node_agent_restarted) ──DELETE──▶ deleted
```

- Un informe tardío del nodo no puede resucitar nada: de un nodo, `stopped` solo admite `stopped`; `deleting` solo admite `deleted` o `failed`; `deleted` es final; `deleted` solo se acepta viniendo de `deleting`.
- La reanudación es una transición del **plano de control**, no un informe del nodo.
- `deleting` **no ocupa capacidad** en el nodo pero **necesita acción del nodo**.
- **Las filas nunca se borran.** `sandbox_events` cuelga de `sandboxes` con `ON DELETE CASCADE`: un `DELETE FROM` físico borraría la auditoría. `deleted` es un estado; `GET /v1/sandboxes` lo oculta salvo con `?include_deleted=1`.
- Una sandbox `failed` se borra directamente (`deleted`): su VM ya no existe, y si quedó algún disco lo recoge el GC del nodo (apartado 5).

### 4. Reanudar: fijada a su nodo

Una sandbox parada ocupa **disco, no CPU ni memoria**. Está fijada a su nodo: la reanudación usa el planificador con `PinnedNodeID`, así que un nodo cordonado, caído o revocado responde **409** y un nodo sin hueco **503** con los motivos, igual que un `create` fijado. Si el nodo se pierde, el disco se pierde con él (`node_lost`). Un disco que falte al reanudar deja la sandbox en `failed` con `disk_lost`, nunca en un disco nuevo y vacío sin avisar. Un arranque que falla al reanudar la devuelve a `stopped` (con el motivo en `status_detail`), para que se pueda reintentar sin perder el disco.

### 5. Quién borra discos: un GC en el nodo, no el reaper de arranque

El plano de control manda al nodo, en cada `/work`, la lista `retained` (ids de sandboxes `stopped` en ese nodo, cuyos discos deben quedarse). El nodo borra `rootfs-<id>.img` solo si el id no está ni en `assigned` ni en `retained`, ni lo está arrancando un worker, **solo tras un sondeo correcto** y como mucho una vez por minuto. El reaper de arranque y `--reap-only` dejan de tocar discos (siguen limpiando procesos, sockets, TAPs y túneles). Un autoaislamiento del nodo (la VM que el plano de control ya no le asigna, o un informe `running` rechazado porque la sandbox se paró mientras arrancaba) tampoco borra el disco si el plano de control conserva discos: solo lo borran `deleting`, el GC y un primer arranque fallido. Con un plano de control antiguo (sin `retained`) el nodo conserva el comportamiento anterior: parar borra el disco.

### 6. Parada ordenada

`vm.delete` y matar el proceso es una parada brusca. El botón de apagado ACPI (`vm.power-button`) **no funciona** con esta imagen: sin `systemd-logind` ni `dbus` nadie escucha la tecla (comprobado en ncc1701d). El nodo pide el apagado al propio guest con `sync; systemctl poweroff --no-block` por el pod-daemon y espera hasta `--stop-grace` (15 s) a que Cloud Hypervisor salga; si no sale, la parada brusca de siempre, con un aviso en el log. Medido: el proceso sale en menos de un segundo y el ext4 queda `clean`.

### 7. Retención y espacio

- `ASP_STOPPED_SANDBOX_TTL` (por defecto **7 días**; `0`/`off` las conserva hasta que se borren), contado desde `stopped_at`. Un barrido cada `ASP_RETENTION_SWEEP` (1 min) las borra con `stop_reason=retention_expired`.
- `ASP_MAX_STOPPED_PER_TENANT` (por defecto sin límite): el mismo barrido borra las más antiguas del tenant que pasen del tope (`tenant_cap`), con un aviso en el log. No se aplica en el instante de parar, sino en el siguiente barrido.
- Una guardia de espacio libre en el nodo (`--disk-min-free-mib`) rechaza clonar o reanudar cuando `--disk-dir` está casi lleno. El nodo informa de su espacio libre en cada heartbeat, y `asp node list` lo muestra junto a cuántas sandboxes paradas guardan un disco en él.
- Un disco retenido guarda lo que escribió el agente, tokens incluidos, hasta el TTL. Los discos viven en `--disk-dir` (root, `0700`, ficheros `0600`).

### 8. Postgres es requisito

La retención solo significa algo si el plano de control recuerda las sandboxes paradas tras reiniciarse. Con el store en memoria un reinicio las olvida y el GC del nodo borra sus discos. El store en memoria queda para tests y smokes; el plano de control avisa al arrancar si lo usa con un TTL activo. ncc1701d pasa a Postgres (guía en [`../bare-metal-ch.md`](../bare-metal-ch.md) § 4.1).

## Alternativas consideradas

1. **Solo una imagen «dev» con las herramientas ya instaladas.** Reduce lo que hay que reinstalar y es barata, pero no conserva el estado del propio agente (cachés, compilaciones, su home). Se hace aparte; no sustituye esto.
2. **Un directorio de caché persistente por sesión, montado por virtiofs.** Conserva cachés y dotfiles sin tocar la máquina de estados, pero no los paquetes instalados en `/usr`. Mismo veredicto.
3. **Pausa y snapshot de la VM (`vm.pause`, `vm.snapshot`).** Conservaría también procesos y memoria. Es mucho más trabajo (snapshot por nodo, restauración, versiones de CH) y es el siguiente paso natural de #81; esta ADR deja el estado `paused` como está.
4. **Parar sigue borrando; el reaper deja de actuar.** Sin cambios de diseño, pero las sesiones olvidadas ocupan CPU y memoria para siempre.
5. **Apagado ACPI en vez de `systemctl poweroff`.** No funciona sin `logind`; instalar `dbus` y `logind` en la imagen engorda la superficie del guest por una función que se resuelve con una orden.

## Consecuencias

### Positivas

- Las sesiones sobreviven a la inactividad y a las paradas voluntarias sin perder lo instalado.
- El reaper por fin es seguro de activar: parar es reversible.
- La auditoría de una sandbox sobrevive a su borrado.

### Negativas / coste

- **Cambios incompatibles.** `stop` ya no borra (hay que usar `rm`) y `DELETE` acaba en `deleted`, no en `stopped`. Hay que actualizar tres smokes, los textos del CLI y las instrucciones del kit de OpenCode.
- **Orden de actualización.** Primero los nodos, después el plano de control. Un nodo antiguo no conoce `deleting` (esas sandboxes se quedan en `deleting`) y su reaper borra discos retenidos.
- Una sandbox parada fija disco y nodo; sin límites se acumulan (de ahí el TTL, el tope por tenant y la guardia de espacio).
- Un disco carga los módulos del kernel con el que se construyó la imagen. Si cambia el `vmlinux` del host, la VM reanudada puede no cargar `vsock.ko`: la reanudación falla tras `--guest-ready-timeout` y vuelve a `stopped`.

### Límites honestos

- Un reinicio del node-agent sigue dejando sus sandboxes `running` en `failed` (`node_agent_restarted`), y el GC borra sus discos. Recuperarlas como `stopped` es una mejora posterior.
- Un fallo de energía deja el ext4 con el diario por reproducir; ext4 lo reproduce al montar y se pueden perder las últimas escrituras.
- No hay borrado seguro: borrar un disco es `unlink`.
- No se pueden cambiar CPU ni memoria al reanudar, ni mover un disco a otro nodo.
