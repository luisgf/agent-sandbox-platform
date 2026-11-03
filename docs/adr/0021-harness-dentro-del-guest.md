# ADR-0021: El harness del agente corre dentro del guest: Claude Code en modo headless sobre el exec de la sesión, sin clave de API en el guest

- **Estado:** Propuesta
- **Fecha:** 2026-10-27
- **Implementación:** ninguna. Nada del código depende de esto. Lo que haría falta en ASP está en [Consecuencias](#qué-cambia-en-asp): una variante de la imagen del guest y una medida; el plano de control, el node-agent y la CLI no cambian. Los detalles de Claude Code que este ADR cita (flags del modo headless, `apiKeyHelper`, `headersHelper`, las variables de federación y de tráfico, la ruta de los ajustes gestionados, el instalador y su manifiesto, que el Agent SDK empaqueta el binario) se comprobaron contra su documentación el 2026-10-27; lo que no se encontró documentado está dicho como tal en [Límites honestos](#límites-honestos). Todo ello se mueve con sus versiones.
- **Extiende:** [0009](0009-agent-sessions.md) (el harness deja de ser externo a la sesión), [0019](0019-acceso-a-infraestructura.md) (su Fase 1: el runtime del agente dentro de la sandbox, ahora con un harness concreto)
- **Relacionados:** [0002](0002-networking.md) (el egress solo deja HTTP(S) y DNS, por allowlist), [0003](0003-identity.md) (ningún secreto largo en el guest; el token de workload por vsock 26502), [0007](0007-multi-user-identity.md) (`user_sub` en el token), [0018](0018-browser-desktop.md) (una variante de la imagen, medida), [sesiones](../ops-asp-session.md) (el exec con PTY y stdin), [usar ASP con infrabroker](../how-to/infrabroker.md), [el modelo de seguridad](../concepts/security-model.md#el-guest-y-las-credenciales)
- **No es:** un cambio en el exec API ni en el plano de control, un plugin de Claude Code, un agente alojado por Anthropic (Managed Agents), una clave de API en la imagen, ni el fin del wrapper de OpenCode (la Fase 0 de [0019](0019-acceso-a-infraestructura.md) sigue funcionando tal cual).

## Contexto

[0009](0009-agent-sessions.md) hizo de la sesión el objeto con el que un agente usa el aislamiento, y dejó el harness **fuera** del guest: OpenCode corre en el portátil o en un host, y solo su herramienta de shell entra en la sandbox por `asp session exec` ([sesiones](../ops-asp-session.md)). Es lo que hay hoy y es honesto con lo que protege: **una** herramienta de las que tiene el modelo. Leer y escribir ficheros, editar, traer una página, llamar a un servidor MCP: todo eso lo hace el harness donde él corre, en el host, con la identidad y la red de quien lo lanzó. Si el modelo se deja llevar por lo que lee (una página, un fichero del repositorio, la salida de un comando), lo que haga con esas herramientas no pasa por ninguna frontera de ASP. [0019](0019-acceso-a-infraestructura.md) ya contaba con el paso siguiente («el runtime del agente corre dentro de la sandbox», su Fase 1) sin decir qué runtime.

Lo que se quiere es un harness **pequeño** dentro del guest: que hable con el modelo, ejecute comandos y edite código en `/workspace`, durante horas, y que todo lo que haga lo haga detrás de la frontera que ya existe. Para un harness de Claude hay dos formas aparentes de conseguirlo: el **Agent SDK** (una biblioteca de Python o TypeScript con la que se escribe un programa que lleva el bucle) o la **CLI de Claude Code en modo headless** (`claude -p`, sin terminal, que habla un protocolo de líneas JSON por su entrada y su salida).

Un hecho deshace media disyuntiva: el Agent SDK **empaqueta el mismo binario nativo de Claude Code y lo lanza como subproceso**, hablando con él por ese mismo protocolo (`stream-json` por stdio). No hay dos motores. Hay una pregunta sobre *quién lleva el bucle y por qué canal*: un programa propio dentro del guest, o el driver que ya existe fuera.

Lo que el repositorio ya tiene, y que decide la respuesta:

| Pieza | Qué da | Dónde |
|---|---|---|
| El exec de la sesión con `pty` o `stdin_stream` | Un canal bidireccional hacia un proceso del guest: NDJSON de salida (`ready`, `stdout`, `stderr`, `exit`) y `POST …/exec/stdin` de entrada, sin abrir ningún puerto | [la API](../reference/api.md#execstdin), [sesiones](../ops-asp-session.md) |
| El socket de identidad (vsock 26502) | Un JWT de workload de cinco minutos con `sub=sandbox/{id}`, `tenant_id`, `sandbox_id`, `user_sub` y `groups`; el guest solo elige `aud`; discovery y JWKS publicados | [0003](0003-identity.md), [0007](0007-multi-user-identity.md), [0020](0020-grupos-en-el-token-de-workload.md) |
| El egress | Denegado por defecto; la allowlist del tenant marca nombres por HTTPS; el audit vive en el nodo | [0002](0002-networking.md) |
| La imagen del guest | Los mismos bytes en cada construcción: paquetes de una foto fechada del archivo, imágenes base por digest, crates y módulos por lock file; una variante (`desktop`) ya existe como etapa aparte | [`images/guest/Dockerfile`](../../images/guest/Dockerfile), [0018](0018-browser-desktop.md) |
| El guest | 1 vCPU y 512 MiB por defecto; pod-daemon ejecuta como el dueño de `/workspace` (uid 1000, `sandbox`) o como `sandboxd`, los dos con cuenta y `HOME` | [0018](0018-browser-desktop.md), [sesiones](../ops-asp-session.md) |

## Decisión

### 1. El harness dentro del guest es Claude Code en modo headless, y el driver se queda fuera

En el guest corre el binario nativo de Claude Code como un proceso de la sesión:

```text
claude -p --output-format stream-json --input-format stream-json --verbose [--permission-mode bypassPermissions] [--mcp-config …]
```

Lo lanza el exec de la sesión con stdin reenviado (`stdin_stream`, o `asp session exec --no-pty` con un pipe), y lo que lee su salida y escribe su entrada —la persona, un script, un servidor de chat— **sigue fuera del guest**, hablando con el plano de control como cualquier cliente del exec API. No hay un programa nuevo en el guest, ni un puerto, ni un servicio vsock, ni un cambio en el exec API: el JSONL de Claude Code viaja por el canal que [0009](0009-agent-sessions.md) ya construyó para el shell.

Por qué la CLI y no un programa con el Agent SDK:

- **Es el mismo binario.** El SDK lo trae dentro y lo lanza; lo que añade es un intérprete (Python o Node) y un programa propio que empaquetar, versionar y mantener en la imagen.
- **El canal ya existe.** Un programa del guest necesitaría el suyo, o reimplementaría el `stream-json` que la CLI ya habla.
- **Lo que el SDK da en proceso, la CLI lo da por configuración:** hooks en su `settings.json`, servidores MCP con `--mcp-config`, y una herramienta MCP para resolver permisos en modo no interactivo (`--permission-prompt-tool`).
- **Cuanto menos haya en el guest, mejor:** la imagen es reproducible byte a byte y cada pieza más es una pieza que fijar y medir.

La cardinalidad de [0009](0009-agent-sessions.md) no cambia: una sesión, una sandbox, una corrida de agente. Lo que cambia es que la corrida vive dentro. Las conversaciones de Claude Code quedan en el disco del guest (`~/.claude`), así que sobreviven a `asp session stop` y `resume` ([0012](0012-retained-disks.md)) y una corrida se retoma donde se quedó.

### 2. El binario entra en una variante de la imagen, fijado por versión y comprobado contra su manifiesto

Como el escritorio de [0018](0018-browser-desktop.md): una etapa `agent` del `Dockerfile`, construida con `ASP_GUEST_VARIANT=agent`; la imagen por defecto no lo trae. El instalador nativo acepta una versión (`install.sh … <versión>`) y cada versión publica un `manifest.json` con el SHA-256 de cada binario por plataforma, firmado con GPG (desde 2.1.89). La construcción fija la versión y comprueba el digest, igual que fija la foto del archivo de Debian y los digests de las imágenes base: cambiar de versión es cambiar una línea y mirar el diff. Sin Node ni `npm`: el binario nativo es un fichero.

La variante trae además, en los ajustes gestionados de Claude Code de la imagen (`/etc/claude-code/managed-settings.json`, y `managed-mcp.json` al lado para los servidores MCP), lo que el guest no debe decidir: las variables que apagan el actualizador, la telemetría y el resto del tráfico que no es la API (`DISABLE_AUTOUPDATER`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`), el `apiKeyHelper` del punto 3 y, si el operador lo quiere, el servidor MCP de infrabroker del punto 6. Dos cosas que hay que saber: esas variables se interpretan por *presencia* (un `0` o un `false` también apagan; para encender, hay que quitarlas), y la imagen las pone, no el exec.

### 3. Ninguna credencial de Anthropic entra en el guest: entra el token de workload, y lo que se obtiene con él

[0003](0003-identity.md) sigue en pie: ni en la imagen, ni en el entorno del exec, ni en un fichero del guest hay una clave de API. La CLI obtiene la credencial con `apiKeyHelper`: un script que la imagen trae, cuya salida ella guarda y manda como bearer, y que vuelve a ejecutar cada cinco minutos (`CLAUDE_CODE_API_KEY_HELPER_TTL_MS`), ante un 401 o 403, y cuando lo guardado es un JWT caducado. Ese script pide un token de workload al socket de identidad (vsock 26502, con el `aud` que corresponda) y lo devuelve. Un token de cinco minutos y un helper que se renueva cada cinco minutos encajan sin más.

Adónde va ese bearer:

**El camino de referencia: un gateway fuera del guest y fuera del nodo.** `ANTHROPIC_BASE_URL` apunta a un proxy inverso que vive donde [0019](0019-acceso-a-infraestructura.md) pone el broker (otra máquina, nunca la IP del nodo), que valida el token de workload contra el issuer de ASP (`aud=llm-gateway`), custodia la clave de API del operador, reenvía la petición a `api.anthropic.com` y anota `user_sub`, `sandbox_id` y el gasto de cada llamada. Es la misma figura que infrabroker para la infraestructura: la máquina que ejecuta el código del agente y la que custodia la credencial son distintas, y cada acción lleva el humano dueño de la sandbox. El gateway no existe hoy en ninguno de los dos repositorios; este ADR fija lo que tiene que cumplir (validar el token de ASP, no dejar pasar nada sin él, atribuir por `user_sub` y `sandbox_id`), no dónde se escribe.

**El camino sin código: Workload Identity Federation.** Anthropic acepta un JWT de un issuer OIDC configurado y lo cambia por una credencial corta en su `/v1/oauth/token`; Claude Code lee las variables de federación (`ANTHROPIC_FEDERATION_RULE_ID`, `ANTHROPIC_ORGANIZATION_ID`, `ANTHROPIC_SERVICE_ACCOUNT_ID`, `ANTHROPIC_IDENTITY_TOKEN_FILE`) y hace el intercambio ella misma; el helper solo tiene que dejar el token de workload en ese fichero. ASP ya publica lo que un issuer necesita (discovery y JWKS). Vale cuando el JWKS del plano de control es **alcanzable desde Internet** (Anthropic tiene que leerlo) y cuando la atribución por cuenta de servicio basta: cada regla de federación mapea a **una** cuenta de servicio (los claims deciden si el JWT entra, no a qué cuenta va), el uso se atribuye al workspace de esa cuenta, y separar identidades es tener una regla por cada una. No vale con `--bare` (ese modo no lee la federación).

Los dos caminos cumplen el invariante. El gateway es el de la arquitectura de referencia porque funciona con un plano de control privado y da atribución y gasto por sandbox; la federación es el atajo para quien no quiera una pieza más.

### 4. El egress: un nombre más en la allowlist, y nada de ruido

La allowlist del tenant marca el nombre del gateway (o `api.anthropic.com` con federación), por HTTPS, como cualquier otro destino ([0002](0002-networking.md)). Nada más sale del guest por cuenta de Claude Code: el punto 2 apaga el actualizador y la telemetría para que el deny-by-default no llene el audit del proxy de 403 que no significan nada.

### 5. Los permisos: la frontera es la VM, y lo que necesita una persona va por el broker

El harness corre con `--permission-mode bypassPermissions`. El prompt de permisos de Claude Code existe para proteger la máquina en la que corre; aquí esa máquina es una microVM con el disco del guest, un `/workspace` que el dueño eligió compartir y una red con allowlist, y el modelo de seguridad ya dice qué garantiza cada frontera ([modelo de seguridad](../concepts/security-model.md)). Lo que de verdad debe esperar a una persona —entrar en un host, tocar un cluster— va por infrabroker, cuya política `require_approval` se aplica **en el broker**, fuera del alcance del guest ([0019](0019-acceso-a-infraestructura.md)). Quien quiera además una aprobación fuera del guest sin broker tiene `--permission-prompt-tool`: una herramienta MCP que decide, y que puede vivir en el driver.

### 6. infrabroker, desde dentro: MCP por HTTPS con el token de workload

Con el harness dentro, infrabroker deja de ser un MCP stdio junto al harness (la Fase 0 de [0019](0019-acceso-a-infraestructura.md)) y pasa a ser un servidor MCP por HTTPS (`{"type": "http", "url": "https://…"}`; el `type` es obligatorio) que el guest alcanza por la allowlist, con el token de workload (`aud=infrabroker`) como bearer. El bearer no se escribe en la configuración: el servidor lleva un `headersHelper`, un script que Claude Code ejecuta en cada conexión (al arrancar y al reconectar, sin caché) y otra vez ante un 401 o 403, reconectando y repitiendo la llamada. Ese script pide un token fresco al socket de identidad, igual que el `apiKeyHelper` del punto 3; un token de cinco minutos y una corrida de horas conviven. El servidor va en el `managed-mcp.json` de la imagen, no en un `.mcp.json` del proyecto: en modo headless los helpers de un servidor de proyecto no corren hasta que alguien confía en la carpeta, y en el guest no hay nadie para hacerlo.

Es exactamente la Fase 1 que [0019](0019-acceso-a-infraestructura.md) describía y que [usar ASP con infrabroker](../how-to/infrabroker.md#fase-1-el-runtime-del-agente-dentro-de-la-sandbox) monta; este ADR le pone el harness que faltaba, y resuelve con `headersHelper` el «cliente bearer mínimo en el guest» que la Fase 2 dejaba pendiente para este caso (sigue haciendo falta para scripts que no sean el harness).

## Alternativas consideradas

| Alternativa | Pros | Contras | Decisión |
|---|---|---|---|
| **Un programa con el Agent SDK dentro del guest** | Hooks tipados, herramientas propias en proceso, `canUseTool` | El mismo binario más un intérprete y un programa que empaquetar; necesita su propio canal con el exterior o reimplementa `stream-json` | **No en este corte.** Se reabre cuando hooks, MCP y `--permission-prompt-tool` no expresen lo que hace falta |
| **Seguir con el harness fuera** (Fase 0 de [0019](0019-acceso-a-infraestructura.md)) | Funciona hoy; nada en la imagen | Solo bash está dentro; las demás herramientas actúan en el host | **Se conserva** como camino para quien no quiera el binario en la imagen. No es el producto cuando importan las herramientas que no son bash |
| **Managed Agents** | Nadie mantiene el bucle ni la sandbox | La frontera la aloja Anthropic: es lo contrario de lo que ASP existe para hacer ([0013](0013-kubernetes-reevaluation.md): quien se autoaloja) | **Rechazada** |
| **Una clave de API en el entorno del exec o en la imagen** | Cero piezas | Un secreto de larga duración en el guest, exfiltrable y sin atribución; contradice [0003](0003-identity.md) | **Rechazada** |
| **Node y el paquete de `npm`** en vez del binario nativo | Lo que la documentación muestra primero | Un intérprete más y un lock file de `npm` fuera de la foto del archivo; el instalador nativo da versión y digest | **Rechazada** |
| **Un servicio vsock o un puerto nuevo para hablar con el harness** | Un canal «propio» | El exec con `stdin_stream` ya es ese canal, con la auth y el audit del plano de control | **Rechazada** |
| **Otro harness** (OpenCode en modo servidor, u otro) **dentro del guest** | Varios proveedores | — | **No lo excluye.** El patrón vale para cualquiera que cumpla lo mismo: binario fijado y comprobado, credencial por token de workload, el exec como canal. El que la variante trae en este corte es Claude Code |

## Consecuencias

### Positivas

- Todas las herramientas del modelo quedan detrás de la frontera, no solo el shell: lo que el modelo lea no puede convertirse en una acción en el host.
- Cero código en el plano de control, el node-agent y la CLI. La integración es la que [0009](0009-agent-sessions.md) ya tenía: el exec de la sesión.
- La credencial del modelo sigue la misma regla que la de la infraestructura: fuera del guest y fuera del nodo, con cada llamada atribuida al humano dueño de la sandbox.
- La Fase 1 de [0019](0019-acceso-a-infraestructura.md) deja de ser abstracta: tiene un harness, una configuración y un camino de token.

### Negativas / coste

- Una variante más de la imagen que construir, publicar y medir ([qué cambia en ASP](#qué-cambia-en-asp)). Hasta que [#233](https://github.com/luisgf/agent-sandbox-platform/issues/233) permita elegir la imagen por sandbox, la variante es la imagen del nodo o no lo es.
- Un binario de un tercero en la imagen, con sus versiones: la foto del archivo de Debian cambia cuando se decide; Claude Code cambia cada pocos días. Fijar la versión es fácil; decidir cuándo moverla es un trabajo.
- Memoria. 512 MiB son los del guest de hoy con un shell; un harness derivado de Node pedirá más. No se adivina: se mide como [0018](0018-browser-desktop.md) midió el escritorio, y el valor por defecto de la variante sale de ahí.
- El gateway es una pieza nueva en algún sitio. Con federación no hay pieza, pero hay un JWKS público y una atribución más gruesa.

### Qué cambia en ASP

| Qué | Dónde | Estado |
|---|---|---|
| Etapa `agent` del `Dockerfile`: el binario por versión y digest, el helper del token, la configuración gestionada | `images/guest/` | Pendiente |
| Medir la variante: tamaño, memoria, arranque hasta que `claude -p` contesta, cuántas caben | Como [0018](0018-browser-desktop.md) | Pendiente |
| Una guía: montar el gateway (o la federación), la allowlist, el `--mcp-config` de infrabroker, y lanzarlo con `asp session exec` | `docs/how-to/` | Pendiente |
| Plano de control, node-agent, CLI | — | Nada |

## Límites honestos

- **Nada de esto está probado**: ni la variante construida, ni el canal con `stdin_stream` bajo `claude -p`, ni los dos helpers contra el socket de identidad, ni la federación contra Anthropic, ni en un host con KVM.
- **Lo que la documentación de Claude Code no dice** (leída el 2026-10-27): el esquema exacto de las líneas JSON de entrada de `--input-format stream-json`; cuándo corre `apiKeyHelper` por primera vez (solo están documentados los reintentos); si los servidores de `managed-mcp.json` quedan fuera de la confianza por carpeta que en modo headless bloquea los `headersHelper` de un servidor de proyecto (el ADR lo da por hecho; si no, el token entra por `headers` con una variable de entorno, y entonces el límite de los cinco minutos vuelve); y qué nombres de variable Claude Code lee como vacíos en `url` y `headers` (la lista empieza por `ANTHROPIC_API_KEY` y `ANTHROPIC_AUTH_TOKEN` y no está completa: el token de workload no debe viajar en una variable con nombre de credencial de Anthropic).
- **Con federación, la atribución es por cuenta de servicio.** Si una exchange concreta queda anotada con el `sub` del JWT no está documentado; lo que sí está es que el uso se cuenta en el workspace de la cuenta de servicio de la regla.
- **`bypassPermissions` es lo que dice.** Dentro del guest, el modelo hace lo que el guest puede hacer: todo en `/workspace` (que es un directorio del host si la sesión lo compartió), todo lo que la allowlist deje salir, y todo lo que infrabroker le permita a su dueño. Las garantías son las de las fronteras, no las de un prompt.
- **Con federación, Anthropic ve el issuer.** El plano de control tiene que publicar su JWKS en Internet. Quien no quiera eso, ni la atribución por cuenta de servicio, monta el gateway.
- **La CLI se mueve más deprisa que este ADR.** Las variables y los flags citados son los de hoy; cuando cambien, cambia la variante, no la decisión: el harness dentro, el token de workload como única credencial, el exec como canal.

## Enmiendas

- *(ninguna)*

## Referencias

- [0009](0009-agent-sessions.md) § Enmiendas, [0019](0019-acceso-a-infraestructura.md) § Enmiendas.
- [Usar ASP con infrabroker](../how-to/infrabroker.md): la Fase 1, que este ADR completa.
- [Sesiones](../ops-asp-session.md): el exec con PTY y stdin, y el wrapper de OpenCode que sigue siendo la Fase 0.
- Claude Code: [modo headless](https://code.claude.com/docs/en/headless), [referencia de la CLI](https://code.claude.com/docs/en/cli-reference), [autenticación](https://code.claude.com/docs/en/authentication) (`apiKeyHelper`, federación), [variables de entorno](https://code.claude.com/docs/en/env-vars), [ajustes gestionados](https://code.claude.com/docs/en/managed-settings) (`/etc/claude-code/`), [MCP](https://code.claude.com/docs/en/mcp) (`type: http`, `headers`, `headersHelper`), [instalación](https://code.claude.com/docs/en/setup) (versión y `manifest.json`), [Agent SDK](https://code.claude.com/docs/en/agent-sdk/quickstart) (empaqueta el binario).
- [Workload Identity Federation](https://platform.claude.com/docs/en/manage-claude/wif-reference).
