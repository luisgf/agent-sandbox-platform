# ADR-0009: Sesiones de agente como forma primaria de aislamiento

- **Estado:** Propuesta / **Aceptada como dirección**
- **Fecha:** 2026-10-03
- **Relacionados:** [0003](0003-identity.md) (secretos fuera del guest), [0004](0004-k8s-scope.md) (el sandbox no es un Pod), [0007](0007-multi-user-identity.md) (`owner_sub` desde el IdP), [`../ops-asp-session.md`](../ops-asp-session.md) (CLI actual), [`../ops-asp-agent-runner.md`](../ops-asp-agent-runner.md) (primitiva one-shot + auth), [`../why-agent-sessions.md`](../why-agent-sessions.md), [`../why-cli-asp.md`](../why-cli-asp.md), [`../roadmap.md`](../roadmap.md)
- **No es:** un plugin de OpenCode, un PTY, ni virtiofs funcionando en KVM. El seguimiento de abajo sí cambia el dataplane (NDJSON) y el spec del workspace; no finge el mount.

## Seguimiento implementado (2026-10-03)

La decisión de arriba sigue en pie: la sesión es el objeto primario y `asp sandbox run` es la primitiva interna. Este seguimiento cierra tres huecos que el texto original listaba como ausentes, y deja escrito lo que **no** se cerró.

### Por qué

Un harness real choca con tres cosas del primer corte. Un único `session.json` mezcla agentes en el mismo `$HOME`. El JSON acumulado esconde la salida hasta el `exit`, así que un tool largo parece colgado. Y llamar «workspace» al disco del guest invita a creer que el checkout del host ya está dentro.

### Qué ganamos

| Pieza | Qué hay en el código | Qué no hay que leer de más |
|---|---|---|
| Sesiones con nombre | `~/.cache/asp/sessions/<nombre>.json` (`ASP_SESSION_DIR` / `--session-dir`, `--name`, default `default`). `start`, `exec`, `status`, `stop`. `--session-file` / `ASP_SESSION_FILE` sigue siendo un path explícito, ya no el default. | El nombre no es una capability ni viaja al CP. Sigue sin token. |
| Exec en stream | Mismo `POST /v1/sandboxes/{id}/exec`. Con `?stream=1` (o `Accept: application/x-ndjson`) la respuesta es NDJSON y el CP hace flush al copiar el cuerpo del node-agent. El node-agent habla `?stream=1` con el pod-daemon. El pod-daemon, si ve el query, responde chunked. Sin query, el JSON `{stdout,stderr,exit_code}` no cambia: lo usan `asp sandbox run` y `asp session exec --buffered`. El CLI de sesión imprime cada chunk al llegar. Un exec stream copiado entero cuenta como actividad, igual que el JSON. | No es PTY, ni stdin, ni websocket. Un pod-daemon antiguo (404 en `?stream=1`) se degrada a un burst NDJSON al final. |
| Workspace en el spec | `asp session start --workspace /ruta` → `workspace_host_path` en el create (migración `009`). El reconciler lo pone en `MicroVMConfig`. FakeVMM lo guarda. Tag previsto `workspace`, mount previsto `/workspace`. | **Cloud Hypervisor no monta nada.** El reconciler no arranca virtiofsd y deja `WorkspaceFSSocket` vacío. `vm.create` omite `fs` en ese caso (un socket ausente rompería el boot). El campo en el CH client solo se rellena si alguien pone el socket; nadie en este repo lo pone. |

### Sigue abierto

- PTY, stdin interactivo, TTY.
- Plugin de OpenCode dentro del repo. El contrato es el wrapper de [`ops-asp-session.md`](../ops-asp-session.md) (`--name` + exec en stream).
- virtiofsd por sandbox y el `mount -t virtiofs` en el guest. Hasta entonces `--workspace` es un spec honesto, no un directorio visible.
- Idle por sesión. El umbral sigue siendo el del proceso (`ASP_SANDBOX_IDLE_TIMEOUT`, lab `2h`, default off). No hay heartbeat nuevo: después del `start`, **solo el exec proxyado bien** refresca `last_activity_at`.
- ADR-0008 (atribución de flujos).

El resto de este ADR describe la dirección original. Donde diga «no hay sesiones con nombre», «el exec no es stream» o «no hay virtiofs», léase con esta sección: lo primero y lo segundo ya están en la forma de arriba; lo tercero está solo como spec.

## Contexto

Un agente (OpenCode y harnesses del mismo tipo) **no es un comando**. Es un proceso largo: razona, llama a un shell, lee la salida, llama otra vez, durante minutos u horas. El aislamiento solo sirve si ese bucle vive **dentro** de una frontera que no se paga de nuevo en cada tool.

La plataforma creció al revés, por necesidad de CI:

1. `POST /v1/sandboxes`
2. poll hasta `running`
3. `POST /v1/sandboxes/{id}/exec`
4. `DELETE`

Eso está empaquetado en `asp sandbox run`. Es un ciclo cerrado, fácil de testear, y aísla *esa* invocación porque la microVM muere con el proceso. Documentado hasta ahora como «el one-liner del agente» ([`ops-asp-agent-runner.md`](../ops-asp-agent-runner.md), fase 2f del roadmap).

Ese relato es el problema de producto. Si cada tool del harness hace create → boot → exec → destroy:

- Con Cloud Hypervisor el **boot domina** la latencia del tool. En FakeVMM el coste se esconde y el diseño parece barato.
- El filesystem del guest **no sobrevive** entre tools: el agente no puede `npm install` y luego correr lo que instaló.
- Identidad, egress y attest se rearman en cada comando sin ganar aislamiento útil (el threat model del agente es la sesión de trabajo, no cada `echo`).
- El camino fácil del harness es ejecutar el shell **en el host** y saltarse ASP.

Ya existe un segundo contrato, construido como añadido post-2f y descrito como tal en el roadmap:

| Pieza en código hoy | Qué es de verdad |
|---|---|
| `asp session start\|exec\|status\|stop` | Puntero local a **un** sandbox ya creado |
| `~/.cache/asp/session.json` (0600) o `ASP_SESSION_FILE` | `sandbox_id`, `cp_url`, `tenant_id`, `image_ref`, `created_at`. **Sin** token ni API key |
| `POST /v1/sandboxes/{id}/exec` | Dataplane: CP → node-agent → pod-daemon (vsock **26500**). JSON acumulado `stdout` / `stderr` / `exit_code` |
| `owner_sub` (ADR-0007) | Lo sella el CP desde el JWT del IdP en el create. El JSON local no puede elegir dueño |
| Egress del sandbox | TAP + allowlist del tenant + nft `asp_egress` de **esa** microVM, durante toda su vida |
| `ASP_SANDBOX_IDLE_TIMEOUT` | Reaper del CP. Default del proceso **apagado** (`0` / unset) para no romper smokes. El unit de lab exporta `2h`. Actividad = create, paso a `running`, exec que el CP proxyó bien. Heartbeat, renew de lease y `GET` **no** cuentan |

Este ADR no inventa ese código. **Cambia el orden de lectura:** la sesión es el objeto primario con el que un agente usa el aislamiento. El one-shot es una primitiva interna (CI, smokes, ops de un solo comando, y el mecanismo que `session start` / `session exec` ya llaman por debajo).

## Decisión

**El objeto primario es una sesión: un agente de larga duración atado a exactamente un sandbox.**

La sesión queda definida por cuatro cosas, no por un fichero:

1. **Identidad.** `owner_sub` (y el tenant) que el control-plane estampa al crear el sandbox, a partir del Bearer del IdP (ADR-0007). `actor_sub` de un exec posterior es quién llamó *ese* comando; el dueño estable de la sesión es `owner_sub`. El guest no elige ninguno de los dos.
2. **Workspace.** El filesystem **del guest** de ese sandbox mientras la sesión vive: ficheros, procesos y `/tmp` persisten entre tools. Eso es el workspace de la sesión **hoy**. El árbol del proyecto en el host **no** forma parte de la sesión: no hay virtiofs ni copia (ver límites). `--cwd` solo cambia el directorio dentro del guest si esa ruta existe.
3. **Egress.** La política de red de ese sandbox (allowlist del tenant, TAP, modo nft) durante toda la sesión. No se renegocia por comando. La atribución de flujos a `owner_sub` sigue siendo ADR-0008 (evaluación, no implementada): la sesión no la crea.
4. **Idle timeout.** `ASP_SANDBOX_IDLE_TIMEOUT` en el CP, umbral **global del proceso**, no un knob por sesión. La sesión termina por `asp session stop` **o** por el reaper (`stop_reason=idle_timeout`). Si el umbral está apagado, no hay fin implícito.

**El harness se engancha a la sesión**, no a `asp sandbox run`. Durante la vida del agente, su shell y el resto de tools que deban estar aislados corren **dentro del guest**. Hoy ese enganche es externo a este repo: sustituir el shell del tool por `asp session exec` (wrapper documentado en [`ops-asp-session.md`](../ops-asp-session.md)). No hay plugin de OpenCode in-tree.

**El exec API sigue siendo el dataplane dentro de la sesión**, no la superficie de integración. Un integrador correcto llama exec muchas veces contra el mismo id. Un integrador incorrecto presenta «usar ASP» como create-exec-destroy por tool, aunque por debajo el byte que corre el comando sea el mismo POST.

**`asp sandbox run` se conserva** como primitiva interna y de ops:

- smokes y CI (`make smoke-asp`);
- un comando suelto de operador que sí quiere la VM muerta al salir;
- implementación: `session start` es create + wait `running`; `session exec` es el mismo proxy de exec.

No se borra, no se esconde del `--help`, y no se reimplementa el dataplane «porque ahora hay sesiones». Deja de ser la historia que se cuenta primero.

Cardinalidad aceptada: **1 sesión de agente ↔ 1 sandbox ↔ 1 corrida de agente.** Dos agentes paralelos son dos sesiones (hoy: dos `ASP_SESSION_FILE` y dos sandboxes). Un sandbox compartido por agentes que no son la misma corrida mezcla workspace, procesos y egress; queda rechazado.

## Ciclo de vida

```text
Bearer IdP
    │  CP deriva owner_sub (el cliente no lo manda como autoridad)
    ▼
session start
    POST /v1/sandboxes → poll running
    escribe ~/.cache/asp/session.json   (puntero; mode 0600; sin secreto)
    stdout = sandbox id
    │
    │  el agente trabaja horas
    │     tool  →  asp session exec  →  POST /v1/sandboxes/{id}/exec
    │                                  →  node-agent → pod-daemon :26500
    │     mismo guest, mismo egress, mismo owner_sub
    │     cada exec OK refresca last_activity_at
    │
    ├── stop explícito
    │     DELETE + borra el puntero
    │     404 (ya destruido) también borra el puntero
    │     otro error HTTP conserva el puntero (reintentar)
    │
    └── idle reap (solo si ASP_SANDBOX_IDLE_TIMEOUT está encendido)
          CP → stopping (el node-agent destruye) o stopped si nunca hubo nodo
          stop_reason=idle_timeout, evento sandbox.idle_reaped
          el puntero local NO se borra
          status/exec fallan cerrados (idle_reaped / "idle timeout", exit 1)
          otra sesión = start --force
```

Estados que no son fin de sesión:

| Acción | Efecto real |
|---|---|
| `session stop --local` | Borra solo el puntero. La microVM **sigue**. Escape de ops, no el camino del agente |
| `GET`, heartbeat, renew de lease | No son actividad. Si lo fueran, el reconciler impediría el idle para siempre |
| Exec que falla antes del guest (red, 502) | No refresca actividad |
| Caducidad del JWT | El siguiente `exec` recibe 401. El sandbox no se entera. El puntero sigue. Nuevo Bearer y se continúa |
| Borrar el JSON a mano | El CP sigue teniendo la VM hasta stop, `--force` o el reaper |

## Flujos

### A) Producto — agente largo (OpenCode u otro harness)

```text
operador / arranque del agente
  asp auth …                    # Bearer; no se copia al puntero
  asp session start --tenant=… --node-id=…

harness, durante horas
  sh del tool  :=  asp session exec --cmd '<comando del modelo>'
                   o  asp session exec -- argv…

fin
  asp session stop
  # o nadie llama stop y el reaper del CP (lab: 2h sin actividad) apaga la VM
```

El harness **no** habla con Cloud Hypervisor, no abre vsock y no elige `owner_sub`. Habla con el CP igual que cualquier otro cliente, pero el ciclo que repite es solo exec.

### B) Primitiva interna — un comando (CI / ops)

```text
asp sandbox run --cmd '…'
  create → wait running → exec → destroy
  sin puntero en disco
```

Sigue siendo el contrato correcto cuando el proceso que llama **es** el comando y la VM debe morir con él. No es el contrato de un bucle de tools.

### C) Dataplane (dentro de A o de B; no es la integración)

```text
POST /v1/sandboxes/{id}/exec
  Authorization: Bearer (resuelto en cada invocación del CLI)
  → control-plane (authz, actor_sub, last_activity_at si el proxy fue bien)
    → node-agent
      → pod-daemon HTTP JSON por vsock 26500
  ← { stdout, stderr, exit_code }   # buffer completo, no stream
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
- Un PTY o un `docker exec -it` (el exec no lo es).
- Compartir el checkout del host por virtiofs (no está).
- Varias sesiones con nombre en un directorio (no está; hay un fichero).
- Atribuir cada SYN al `actor_sub` del último exec (ADR-0008: el dueño del egress, cuando exista, es `owner_sub`).

## Alternativas consideradas

| Alternativa | Pros | Contras | Decisión |
|---|---|---|---|
| **One-shot por comando como producto** (`asp sandbox run` en cada tool) | Aislamiento máximo entre tools; cero estado local; fácil de testear | Boot por tool; el agente no acumula workspace; empuja a ejecutar en el host | **Rechazada como superficie.** Conservada como primitiva de CI/ops |
| **Solo plugin OpenCode que hable HTTP** | Tools nativos, sin wrapper `sh -c` | Duplica auth, wait y cliente en el lenguaje del harness; este repo no lo mantiene; un plugin mal hecho puede volver al one-shot | **Diferida.** Cuando exista, debe implementar *esta* sesión (start una vez, exec muchas), no un ciclo nuevo |
| **SSH / PTY largo al guest** | Shell de verdad, stdin, stream | Otra superficie de auth; no reutiliza el exec proxy que ya autoriza con el mismo Bearer; el confirm SSH (ADR-0003/0007) no es un canal de tools | **Diferida.** No sustituye al exec como dataplane de la sesión |
| **Directorio de sesiones con nombre** | Agentes paralelos sin pisarse el JSON | Más CLI, más formas de adjuntar el id equivocado | **Hecho en el seguimiento** (`--name`, `ASP_SESSION_DIR`). `ASP_SESSION_FILE` queda como override de un solo fichero |
| **Guardar el JWT en `session.json`** | El exec no depende de `asp auth` | Un puntero robado es un token | **Rechazada.** El fichero solo tiene id y URL |
| **Un sandbox por tenant (o por `owner_sub`) compartido por todos sus agentes** | Menos VMs | Mezcla workspace, procesos y egress de corridas distintas; un agente ve el trabajo del otro | **Rechazada.** 1 sesión ↔ 1 sandbox |
| **Job/Pod de Kubernetes por comando** | Ecosistema conocido | ADR-0004: el sandbox no es un Pod. Además repite el anti-patrón de boot por tool | **Rechazada** |
| **GC solo en el CLI** (borrar el JSON «apaga» la sesión) | No toca el CP | La VM vive en el nodo. Borrar el puntero no libera CPU/RAM (`--local` ya documenta ese pie) | **Rechazada como autoridad.** El reaper es del CP. El CLI informa `idle_reaped` y no finge haber parado la VM |
| **Idle por sesión, distinto del umbral global** | Un agente interactivo y un batch no comparten `2h` | Otro campo, otra política, otra mentira si el default sigue off | **Fuera de este ADR.** Hoy un umbral de proceso |
| **Stream/PTY como condición para declarar la sesión «de verdad»** | Encaja con TUI y con tools que leen stdin | El PTY sigue sin existir; el stream no debía bloquear la dirección | **El PTY sigue diferido.** El NDJSON del seguimiento no es la condición de la sesión: es el dataplane de stdout/stderr. El JSON acumulado sigue válido |

## Consecuencias

### Positivas

- La latencia del tool pasa a ser la del comando, no la del boot, en cuanto el harness deja de llamar a `sandbox run`.
- El agente puede encadenar comandos con estado (instaló, compiló, dejó un fichero en `/tmp`) **dentro** de la microVM.
- Identidad (`owner_sub`), egress y la frontera de la VM son estables durante horas: misma historia de auditoría que ADR-0007 para create y para cada exec.
- El reaper (`ASP_SANDBOX_IDLE_TIMEOUT`) acota el coste de una sesión olvidada **cuando ops lo enciende** (lab: `2h`). El diseño de producto asume ese fin; el default off es un límite de compatibilidad con smokes, no una invitación a VMs eternas.
- Los integradores tienen un no-objetivo explícito: no diseñar la integración como una VM por comando.
- No hace falta un protocolo nuevo: el puntero local y el exec API ya existen.

### Negativas / coste

- **Estado compartido entre tools** de la misma sesión: un comando hostil o simplemente sucio deja procesos, ficheros y credenciales de workload para el siguiente. Es el riesgo que se compra a cambio de un workspace. El one-shot no desaparece para quien necesite borrar ese estado (CI, comando no confiable suelto).
- El puntero local puede **divergir** del CP (reaper, DELETE externo, otro host). `exec`/`status` tienen que fallar cerrado; no recrear el sandbox en silencio. Hoy lo hacen (`no active session` solo si no hay fichero; si el CP mató la VM, el mensaje es idle o 404).
- Un solo `session.json` por defecto: dos agentes en el mismo `$HOME` se pisan. Paralelo = otro path y otra VM.
- Sin virtiofs, «el agente trabaja en el sandbox» **no** significa «el agente ve el repo del host». Quien lea solo el eslogan se llevará un filesystem vacío. Hay que decirlo al lado de la decisión, no en un apéndice.
- El exec no es interactivo. Tools que asumen TTY, streaming o stdin se van a ver rotos aunque la sesión sea la correcta.
- Auth por invocación: una sesión de horas cruza el `exp` del access token. El operador (o el auto-fetch) renueva el Bearer; no hay grant atado a la sesión.
- Dirección aceptada **sin** borrar ejemplos de `sandbox run`. Durante un tiempo los dos contratos conviven y un lector perezoso puede seguir copiando el one-liner. Los docs de entrada (README, roadmap, las dos ops) tienen que nombrar la sesión primero.

### Qué no cambia este ADR (alcance)

- No renombra rutas HTTP ni el binario `asp`.
- El seguimiento de 2026-10-03 añade sesiones con nombre, NDJSON y el spec de workspace. No añade plugin, PTY, virtiofsd en KVM ni idle por sesión.
- No mueve la autoridad de `owner_sub` al cliente.
- No implementa ADR-0008.
- El cambio que acompaña a este texto es de **documentación**: el código de `asp session` y del reaper ya estaba.

## Límites honestos (gaps reales, no roadmap disfrazado)

Estos límites están en el código de hoy. Aceptar la dirección **no** los cierra.

| Gap | Realidad |
|---|---|
| **Workspace compartido con el host** | El spec existe (`workspace_host_path`, `--workspace`, FakeVMM lo registra). **KVM no lo monta:** no hay virtiofsd y CH no recibe `fs`. El disco del guest sigue siendo el único filesystem que el exec ve. Virtiofs del SSH agent sigue siendo otra cosa (fase 2e) |
| **Plugin de OpenCode** | No existe en este repo. No registra tools ni habla el protocolo del harness. El enganche es un binario que el harness hace `exec`. Hay que cablearlo fuera |
| **Directorio local, no un registro del CP** | Hay nombres (`sessions/<nombre>.json`). Sigue siendo local al `$HOME` del CLI; otro host no lo ve. El CP no conoce el nombre. Modo `0600`, directorio `0700`. `--session-file` sigue existiendo como override |
| **Exec no es PTY** | Hay stream NDJSON (stdout/stderr) en el mismo POST, y el JSON acumulado sigue para smokes (`--buffered`, `sandbox run`). No hay stdin ni TTY. Exit code del guest cuando llegó el evento `exit` o el JSON; errores del CLI son exit 1 |
| **Idle apagado por defecto** | Sin `ASP_SANDBOX_IDLE_TIMEOUT` (o con `0` / `off` / `false` / `disabled`) el reaper no corre. Una sesión sin `stop` vive hasta que alguien la borre. El lab systemd usa `2h`; intervalo `ASP_SANDBOX_IDLE_SWEEP` default `1m`. No hay timeout distinto por sesión. Filas viejas al aplicar la migración `008` empiezan el reloj en `now()`, no en el `created_at` histórico |
| **Dry-run ≠ aislamiento** | Tests de sesión = `httptest`. `make smoke-asp` cubre `sandbox run`, no esta sesión, contra FakeVMM. KVM + CH es el único aislamiento real |
| **`--local`** | No llama al CP. No es stop |
| **RBAC** | Quien tenga derecho a exec puede usar el id si lo conoce. Eso no transfiere `owner_sub`. El puntero no es una capability: cada `exec` lleva su propio Bearer |
| **Egress atribuido al humano en el wire** | Sigue abierto (ADR-0008). La sesión fija *qué sandbox* es el del agente; no etiqueta todavía cada flujo |

## Criterio para trabajo futuro (sin hacerlo aquí)

Una integración de harness está alineada con esta dirección solo si:

1. Arranca **una** sesión y enruta los tools aislados a su exec.
2. No llama a create/destroy por comando de shell.
3. No escribe `owner_sub` ni el JWT en el puntero.
4. No crea un sandbox implícito cuando falta el puntero (hoy: `no active session`, exit 1). Un tool silencioso no debe encender microVMs.
5. Trata virtiofs, PTY y el plugin como decisiones aparte. No se bloquea la sesión en ellas y no se finge que ya están.

## Referencias

- Narrativa corta: [`../why-agent-sessions.md`](../why-agent-sessions.md)
- Contrato CLI de la sesión: [`../ops-asp-session.md`](../ops-asp-session.md)
- Primitiva one-shot y Bearer: [`../ops-asp-agent-runner.md`](../ops-asp-agent-runner.md)
- Identidad: [0007](0007-multi-user-identity.md)
- CLI demo: [`../why-cli-asp.md`](../why-cli-asp.md)
- Roadmap (2f, sesión, idle): [`../roadmap.md`](../roadmap.md)
