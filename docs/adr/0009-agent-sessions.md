# ADR-0009: Sesiones de agente como forma primaria de aislamiento

- **Estado:** Aceptada
- **Fecha:** 2026-10-03
- **Implementación:** hecha. Sesiones con nombre, exec en stream con PTY y stdin, workspace del host por virtiofs, y parar que conserva el disco (`asp session stop`, `resume`, `rm`). No hay plugin de OpenCode en este repositorio.
- **Enmendada por:** [0012](0012-retained-disks.md): parar ya no borra. `asp session stop` y el idle reap conservan el disco (`asp session resume` lo arranca otra vez) y `asp session rm` borra la sandbox; `--local` pasó de `stop` a `rm`.
- **Relacionados:** [0003](0003-identity.md) (secretos fuera del guest), [0004](0004-k8s-scope.md) (el sandbox no es un Pod), [0007](0007-multi-user-identity.md) (`owner_sub` desde el IdP), [0010](0010-on-demand-local-net.md) (LAN del usuario, solo si la sesión lo pide), [`../ops-asp-session.md`](../ops-asp-session.md) (la CLI de la sesión), [`../ops-asp-agent-runner.md`](../ops-asp-agent-runner.md) (la primitiva one-shot y la autenticación), [`../reference/cli.md`](../reference/cli.md), [`../history.md`](../history.md)
- **No es:** un plugin de OpenCode, ni un SSH (el exec con PTY y stdin no es una shell remota), ni un puente de bytes opacos.

## Contexto

Un agente (OpenCode y harnesses del mismo tipo) **no es un comando**. Es un proceso largo: razona, llama a un shell, lee la salida, llama otra vez, durante minutos u horas. El aislamiento solo sirve si ese bucle vive **dentro** de una frontera que no se paga de nuevo en cada tool.

La plataforma creció al revés, por necesidad de CI:

1. `POST /v1/sandboxes`
2. poll hasta `running`
3. `POST /v1/sandboxes/{id}/exec`
4. `DELETE`

Eso está empaquetado en `asp sandbox run`. Es un ciclo cerrado, fácil de testear, y aísla *esa* invocación porque la microVM muere con el proceso. Se documentó durante un tiempo como «el one-liner del agente».

Ese relato es el problema de producto. Si cada tool del harness hace create → boot → exec → destroy:

- Con Cloud Hypervisor el **boot domina** la latencia del tool. En FakeVMM el coste se esconde y el diseño parece barato.
- El filesystem del guest **no sobrevive** entre tools: el agente no puede `npm install` y luego correr lo que instaló.
- Identidad, egress y attest se rearman en cada comando sin ganar aislamiento útil (el threat model del agente es la sesión de trabajo, no cada `echo`).
- El camino fácil del harness es ejecutar el shell **en el host** y saltarse ASP.

Lo que ya había, construido como añadido después del one-shot:

| Pieza | Qué es |
|---|---|
| `asp session start\|exec\|status\|stop\|resume\|rm` | Un puntero local a **un** sandbox, y los comandos que lo usan |
| `~/.cache/asp/sessions/<nombre>.json` (0600; `ASP_SESSION_DIR`, `--name`, `--session-file`) | `sandbox_id`, `cp_url`, `tenant_id`, `image_ref`, `created_at`. **Sin** token ni API key. Un nombre por agente: dos agentes en el mismo `$HOME` no se pisan |
| `POST /v1/sandboxes/{id}/exec` | El dataplane: plano de control → node-agent → pod-daemon (vsock **26500**). Con `?stream=1` la respuesta es NDJSON; sin él, un JSON `{stdout, stderr, exit_code}` |
| `owner_sub` ([0007](0007-multi-user-identity.md)) | Lo sella el plano de control desde el JWT del IdP en el create. El JSON local no puede elegir dueño |
| Egress del sandbox | TAP + allowlist del tenant + nft `asp_egress` de **esa** microVM, durante toda su vida |
| `ASP_SANDBOX_IDLE_TIMEOUT` | El reaper del plano de control. Por defecto del proceso **apagado**; el unit de lab lo enciende a `2h`. Actividad = create, paso a `running`, un exec que el plano de control proxyó bien (JSON acumulado o stream terminado) y un stdin bien proxyado. Heartbeat, sondeo de `/work`, `GET` y `status` **no** cuentan |

Este ADR no inventa ese código. **Cambia el orden de lectura:** la sesión es el objeto primario con el que un agente usa el aislamiento. El one-shot es una primitiva interna (CI, smokes, ops de un solo comando, y el mecanismo que `session start` / `session exec` ya llaman por debajo).

## Decisión

**El objeto primario es una sesión: un agente de larga duración atado a exactamente un sandbox.**

La sesión queda definida por cuatro cosas, no por un fichero:

1. **Identidad.** `owner_sub` (y el tenant) que el control-plane estampa al crear el sandbox, a partir del Bearer del IdP (ADR-0007). `actor_sub` de un exec posterior es quién llamó *ese* comando; el dueño estable de la sesión es `owner_sub`. El guest no elige ninguno de los dos.
2. **Workspace.** El disco del guest de ese sandbox mientras la sesión vive: ficheros, procesos y `/tmp` persisten entre tools, y **el disco sobrevive a una parada** ([0012](0012-retained-disks.md)); los procesos y la memoria, no. Además, `asp session start --workspace /ruta/absoluta` comparte un directorio del host por virtiofs: el nodo arranca `virtiofsd`, Cloud Hypervisor recibe el tag `workspace`, y la imagen del guest lo monta en `/workspace` al arrancar. Sin `--workspace` no hay directorio del host: `/workspace` es un directorio vacío del disco del guest.
3. **Egress.** La política de red de ese sandbox (allowlist del tenant, TAP, modo nft) durante toda la sesión. No se renegocia por comando. La atribución de flujos a `owner_sub` es ADR-0008 (parcial). Llegar a la LAN del usuario **no** forma parte de este egress: es ADR-0010, apagado por defecto, y solo con `asp session start --local-net`.
4. **Fin de la sesión.** `asp session stop` (o el reaper, `stop_reason=idle_timeout`) **para** la VM y **conserva el disco**; `asp session resume` la arranca otra vez en el mismo nodo y sobre el mismo disco. `asp session rm` borra la sandbox y su disco. El umbral del reaper (`ASP_SANDBOX_IDLE_TIMEOUT`) es **global del proceso**, no un ajuste por sesión; si está apagado, no hay parada implícita, y una sandbox parada caduca a los 7 días (`ASP_STOPPED_SANDBOX_TTL`, ADR-0012).

**El harness se engancha a la sesión**, no a `asp sandbox run`. Durante la vida del agente, su shell y el resto de tools que deban estar aislados corren **dentro del guest**. Hoy ese enganche es externo a este repo: sustituir el shell del tool por `asp session exec` (el wrapper está en [`ops-asp-session.md`](../ops-asp-session.md)). No hay plugin de OpenCode in-tree.

**El exec API sigue siendo el dataplane dentro de la sesión**, no la superficie de integración. Un integrador correcto llama exec muchas veces contra el mismo id. Un integrador incorrecto presenta «usar ASP» como create-exec-destroy por tool, aunque por debajo el byte que corre el comando sea el mismo POST.

**`asp sandbox run` se conserva** como primitiva interna y de ops:

- smokes y CI (`make smoke-asp`);
- un comando suelto de operador que sí quiere la VM muerta al salir;
- implementación: `session start` es create + wait `running`; `session exec` es el mismo proxy de exec.

No se borra, no se esconde del `--help`, y no se reimplementa el dataplane «porque ahora hay sesiones». Deja de ser la historia que se cuenta primero.

Cardinalidad aceptada: **1 sesión de agente ↔ 1 sandbox ↔ 1 corrida de agente.** Dos agentes paralelos son dos sesiones (dos nombres, `--name`, y dos sandboxes). Un sandbox compartido por agentes que no son la misma corrida mezcla workspace, procesos y egress; queda rechazado.

## Ciclo de vida

```text
Bearer IdP
    │  el plano de control deriva owner_sub (el cliente no lo manda como autoridad)
    ▼
session start [--name N] [--workspace /ruta] [--local-net] [--node-id ID]
    POST /v1/sandboxes → el plano de control la coloca en un nodo con hueco → poll running
    escribe ~/.cache/asp/sessions/N.json   (puntero; modo 0600; sin secreto)
    stdout = sandbox id
    │
    │  el agente trabaja horas
    │     tool  →  asp session exec  →  POST /v1/sandboxes/{id}/exec[?stream=1]
    │                                  →  node-agent → pod-daemon :26500
    │     mismo guest, mismo egress, mismo owner_sub
    │     un exec o un stdin bien proxyados refrescan last_activity_at
    │
    ├── stop explícito (asp session stop)
    │     POST /v1/sandboxes/{id}/stop → stopping → stopped      el disco se conserva
    │     el puntero local se queda: asp session resume lo arranca de nuevo (boot_count 2…)
    │
    ├── idle reap (solo si ASP_SANDBOX_IDLE_TIMEOUT está encendido)
    │     igual que un stop: stop_reason=idle_timeout, evento sandbox.idle_reaped
    │     status/exec fallan cerrados («sandbox is stopped; its disk is kept»)
    │
    └── rm (asp session rm)
          DELETE → deleting → deleted: se borra la sandbox y su disco
          404 (ya borrada) también limpia el puntero; otro error HTTP lo conserva (reintentar)
```

Lo que **no** es el fin de una sesión:

| Acción | Efecto real |
|---|---|
| `session rm --local` | Borra solo el puntero. La microVM **sigue**, con su disco. Escape de ops, no el camino del agente |
| `GET`, heartbeat, sondeo de `/work`, `status` | No son actividad. Si lo fueran, el reconciler impediría el idle para siempre |
| Exec que falla antes del guest (red, 502) | No refresca actividad |
| Caducidad del JWT | El siguiente `exec` recibe 401. El sandbox no se entera. El puntero sigue. Nuevo Bearer y se continúa |
| Borrar el JSON a mano | El plano de control sigue teniendo la VM hasta que alguien la pare, la borre o caduque |
| Perder el nodo | La sandbox pasa a `failed` (`lost_with_node`) y su disco vivía en ese servidor: `asp session start --force` ([0011](0011-multi-node.md)) |

## Flujos

### A) Producto — agente largo (OpenCode u otro harness)

```text
operador / arranque del agente
  asp auth …                    # Bearer; no se copia al puntero
  asp session start --name agente-1 --workspace /ruta/del/proyecto

harness, durante horas
  sh del tool  :=  asp session exec --name agente-1 --cmd '<comando del modelo>'
                   o  asp session exec --name agente-1 -- argv…

fin
  asp session stop --name agente-1     # o nadie la para y el reaper del plano de control la para
  asp session resume --name agente-1   # si el trabajo sigue mañana
  asp session rm --name agente-1       # cuando ya no hace falta
```

El harness **no** habla con Cloud Hypervisor, no abre vsock y no elige `owner_sub`. Habla con el plano de control igual que cualquier otro cliente, pero el ciclo que repite es solo exec.

### B) Primitiva interna — un comando (CI / ops)

```text
asp sandbox run --cmd '…'
  create → wait running → exec → destroy
  sin puntero en disco
```

Sigue siendo el contrato correcto cuando el proceso que llama **es** el comando y la VM debe morir con él. No es el contrato de un bucle de tools.

### C) Dataplane (dentro de A o de B; no es la integración)

```text
POST /v1/sandboxes/{id}/exec[?stream=1]
  Authorization: Bearer (resuelto en cada invocación del CLI)
  → plano de control (authz, actor_sub, last_activity_at si el proxy fue bien)
    → node-agent
      → pod-daemon HTTP por vsock 26500
  ← sin ?stream: { stdout, stderr, exit_code }   # JSON acumulado (--buffered, --json, sandbox run)
  ← con ?stream=1: NDJSON según llega; con PTY, un primer evento ready con exec_id,
                   y el stdin va por POST /v1/sandboxes/{id}/exec/stdin
```

Misma llamada en `sandbox run` y en `session exec`. La diferencia de producto es **quién crea y quién destruye**, y si el guest conserva estado entre llamadas.

## Qué no es la idea principal

**Create → exec → destroy por cada comando de shell.**

Consecuencias si se vuelve a tomar como producto:

- El agente paga boot de microVM por tool (CH) o se autoengaña con FakeVMM.
- No hay directorio de trabajo acumulado dentro del aislamiento: cada comando nace en un guest nuevo.
- La documentación, los ejemplos y los one-liners empujan al integrador al anti-patrón. Este ADR existe para invertir ese orden.
- «Más aislamiento» por comando es aislamiento del estado **útil** (el trabajo del agente) sin añadir frontera nueva: la frontera ya es la microVM de la sesión. Un comando hostil dentro de la sesión puede dejar procesos y ficheros para el siguiente; eso se acepta como riesgo de la sesión, no se «arregla» volviendo al one-shot como UX por defecto.

Tampoco es la idea principal:

- Un plugin de harness como requisito para usar ASP (no existe; la CLI es el contrato estable).
- Un SSH o una shell remota. El exec con PTY existe para que `cat`, una shell o un REPL lean al usuario, no para sustituir a SSH (otra autenticación, otra superficie).
- Atribuir cada SYN al `actor_sub` del último exec (ADR-0008: el dueño del egress, cuando exista, es `owner_sub`).

## Alternativas consideradas

| Alternativa | Pros | Contras | Decisión |
|---|---|---|---|
| **One-shot por comando como producto** (`asp sandbox run` en cada tool) | Aislamiento máximo entre tools; cero estado local; fácil de testear | Boot por tool; el agente no acumula workspace; empuja a ejecutar en el host | **Rechazada como superficie.** Conservada como primitiva de CI/ops |
| **Solo plugin OpenCode que hable HTTP** | Tools nativos, sin wrapper `sh -c` | Duplica auth, wait y cliente en el lenguaje del harness; este repo no lo mantiene; un plugin mal hecho puede volver al one-shot | **Diferida.** Cuando exista, debe implementar *esta* sesión (start una vez, exec muchas), no un ciclo nuevo |
| **SSH al guest** | Shell de verdad | Otra superficie de auth; no reutiliza el exec proxy que ya autoriza con el mismo Bearer; el confirm SSH (ADR-0003/0007) no es un canal de tools | **Rechazada.** El exec con PTY y stdin cubre lo que el harness necesita, sobre la misma autenticación |
| **Directorio de sesiones con nombre** | Agentes paralelos sin pisarse el JSON | Más CLI, más formas de adjuntar el id equivocado | **Hecho** (`--name`, `ASP_SESSION_DIR`). `ASP_SESSION_FILE` queda como override de un solo fichero |
| **Guardar el JWT en el puntero** | El exec no depende de `asp auth` | Un puntero robado es un token | **Rechazada.** El fichero solo tiene id y URL |
| **Un sandbox por tenant (o por `owner_sub`) compartido por todos sus agentes** | Menos VMs | Mezcla workspace, procesos y egress de corridas distintas; un agente ve el trabajo del otro | **Rechazada.** 1 sesión ↔ 1 sandbox |
| **Job/Pod de Kubernetes por comando** | Ecosistema conocido | ADR-0004: el sandbox no es un Pod. Además repite el anti-patrón de boot por tool | **Rechazada** |
| **GC solo en el CLI** (borrar el JSON «apaga» la sesión) | No toca el plano de control | La VM vive en el nodo. Borrar el puntero no libera CPU/RAM (`rm --local` ya documenta ese pie) | **Rechazada como autoridad.** El reaper es del plano de control. El CLI informa y no finge haber parado la VM |
| **Idle por sesión, distinto del umbral global** | Un agente interactivo y un batch no comparten `2h` | Otro campo, otra política, otra mentira si el default sigue off | **Fuera de este ADR.** Hoy un umbral de proceso |
| **Stream y PTY como condición para declarar la sesión «de verdad»** | Encaja con TUI y con tools que leen stdin | Retrasaba la dirección | **Hechos después**, sin ser la condición: el NDJSON es el dataplane de stdout/stderr, y el JSON acumulado sigue válido (`--buffered`, `--json`) |

## Consecuencias

### Positivas

- La latencia del tool pasa a ser la del comando, no la del boot, en cuanto el harness deja de llamar a `sandbox run`.
- El agente puede encadenar comandos con estado (instaló, compiló, dejó un fichero en `/tmp`) **dentro** de la microVM, y retomar al día siguiente sobre el mismo disco (`resume`).
- Identidad (`owner_sub`), egress y la frontera de la VM son estables durante horas: misma historia de auditoría que ADR-0007 para create y para cada exec.
- El reaper (`ASP_SANDBOX_IDLE_TIMEOUT`) acota el coste de una sesión olvidada **cuando ops lo enciende**, y lo que hace es parar, no destruir: el trabajo no se pierde. El default off es un límite de compatibilidad con smokes, no una invitación a VMs eternas.
- Los integradores tienen un no-objetivo explícito: no diseñar la integración como una VM por comando.
- El agente ve el proyecto del host sin copiarlo (`--workspace`) y la salida del comando según llega.

### Negativas / coste

- **Estado compartido entre tools** de la misma sesión: un comando hostil o simplemente sucio deja procesos, ficheros y credenciales de workload para el siguiente. Es el riesgo que se compra a cambio de un workspace. El one-shot no desaparece para quien necesite borrar ese estado (CI, comando no confiable suelto).
- El puntero local puede **divergir** del plano de control (reaper, borrado externo, otro host). `exec` y `status` tienen que fallar cerrado; no recrear el sandbox en silencio. Hoy lo hacen (`no active session` solo si no hay fichero; si la VM está parada o ya no existe, el mensaje lo dice).
- Auth por invocación: una sesión de horas cruza el `exp` del access token. El operador (o el auto-fetch) renueva el Bearer; no hay grant atado a la sesión.
- Una sandbox **parada** sigue ocupando disco en su nodo hasta que se borra o caduca, y solo puede reanudarse **en ese nodo**: si está lleno, `resume` da 503 y la sandbox sigue parada ([0012](0012-retained-disks.md)).
- Dirección aceptada **sin** borrar ejemplos de `sandbox run`. Durante un tiempo los dos contratos conviven y un lector perezoso puede seguir copiando el one-liner. Los docs de entrada (README, roadmap, las dos ops) nombran la sesión primero.

### Qué no cambia este ADR (alcance)

- No renombra rutas HTTP ni el binario `asp`.
- No mueve la autoridad de `owner_sub` al cliente.
- No implementa ADR-0008.

## Límites honestos

Estos límites están en el código de hoy. Aceptar la dirección **no** los cierra.

| Gap | Realidad |
|---|---|
| **Imágenes anteriores a `workspace-virtiofs.service`** | El nodo arranca `virtiofsd` y CH recibe `fs` tag `workspace` cuando el path no está vacío, pero un rootfs anterior no monta solo: `mount -t virtiofs workspace /workspace` hasta reconstruirlo. Sin el binario en el nodo, el start falla. El virtiofs del agente SSH es otra cosa (ADR-0006) |
| **Plugin de OpenCode** | No existe en este repo. No registra tools ni habla el protocolo del harness. El enganche es un binario que el harness hace `exec`. Hay que cablearlo fuera |
| **Directorio local, no un registro del plano de control** | Hay nombres (`sessions/<nombre>.json`), pero siguen siendo locales al `$HOME` del CLI; otro host no los ve y el plano de control no conoce el nombre. Modo `0600`, directorio `0700`. `--session-file` sigue existiendo como override |
| **PTY acotado** | stderr mezclado en el master, el cierre de stdin son dos Ctrl-D (no EOF de un programa en modo raw), sin SIGWINCH desde el CLI, y los bytes viajan como un string JSON (no un pipe opaco). `--no-pty` da un pipe con EOF real. Un pod-daemon antiguo no emite `ready`: el stdin no viaja y el stream degrada a un único burst. Un fallo del propio CLI (red, 500, stream sin evento `exit`) es exit 1, no el código del guest |
| **Idle apagado por defecto** | Sin `ASP_SANDBOX_IDLE_TIMEOUT` (o con `0` / `off`) el reaper no corre: una sesión sin `stop` vive hasta que alguien la pare o la borre. El intervalo es `ASP_SANDBOX_IDLE_SWEEP` (`1m`). No hay timeout distinto por sesión |
| **Dry-run ≠ aislamiento** | Los tests de sesión son `httptest` y `cargo test`; `make smoke-asp` cubre `sandbox run` contra FakeVMM. KVM + CH es el único aislamiento real (`make e2e-kvm` recorre el ciclo) |
| **`rm --local`** | No llama al plano de control. No es un borrado |
| **RBAC** | Quien tenga derecho a exec puede usar el id si lo conoce. Eso no transfiere `owner_sub`. El puntero no es una capability: cada `exec` lleva su propio Bearer |
| **Egress atribuido al humano en el wire** | Parcial (ADR-0008): la sesión fija *qué sandbox* es del agente, y el audit del proxy trae su `sandbox_id`, pero no `owner_sub` |
| **LAN del usuario** | ADR-0010, apagado por defecto. `asp session start --local-net` pide el túnel completo de esa sesión (no una allowlist de CIDR); el nodo y el CLI lanzan `wg` si hay las herramientas y `CAP_NET_ADMIN` |

## Criterio para trabajo futuro

Una integración de harness está alineada con esta dirección solo si:

1. Arranca **una** sesión y enruta los tools aislados a su exec.
2. No llama a create/destroy por comando de shell.
3. No escribe `owner_sub` ni el JWT en el puntero.
4. No crea un sandbox implícito cuando falta el puntero (hoy: `no active session`, exit 1). Un tool silencioso no debe encender microVMs.
5. No se bloquea en el plugin, ni finge que ya existe.

## Enmiendas

- **2026-10-03 (seguimiento):** sesiones con nombre (`~/.cache/asp/sessions/<nombre>.json`, `--name`; antes, un solo `session.json` que mezclaba agentes en un mismo `$HOME`), exec en NDJSON (antes, el JSON acumulado escondía la salida hasta el `exit`), PTY y stdin (`ready` + `POST …/exec/stdin`), y `--workspace` por virtiofs con el montaje automático en la imagen (antes, «workspace» era solo el disco del guest, y llamarlo así invitaba a creer que el checkout del host ya estaba dentro).
- **2026-10 (#86, [0012](0012-retained-disks.md)):** parar conserva el disco. `stop` ya no hace `DELETE`: `asp session stop` y el idle reap paran, `resume` arranca otra vez, `rm` borra. `--local` pasó de `stop` a `rm`.
- **2026-10 (#118, #119):** una VM que muere sola se detecta y se informa `stopped` con el motivo (`vmm_exited`); un arranque cuyo guest no responde falla en vez de informar `running`.

## Referencias

- Contrato CLI de la sesión: [`../ops-asp-session.md`](../ops-asp-session.md)
- Primitiva one-shot y Bearer: [`../ops-asp-agent-runner.md`](../ops-asp-agent-runner.md)
- Todas las órdenes y sus opciones: [`../reference/cli.md`](../reference/cli.md)
- Identidad: [0007](0007-multi-user-identity.md) · discos: [0012](0012-retained-disks.md) · LAN del usuario: [0010](0010-on-demand-local-net.md)
- Historia (2f, sesión, idle): [`../history.md`](../history.md)
