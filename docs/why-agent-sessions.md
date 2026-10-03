# Por qué / Qué ganamos — Sesiones de agente

Dirección: [`adr/0009-agent-sessions.md`](adr/0009-agent-sessions.md) (**propuesta / aceptada como dirección**).  
Contrato CLI de hoy: [`ops-asp-session.md`](ops-asp-session.md).  
La primitiva que **no** es el producto: [`ops-asp-agent-runner.md`](ops-asp-agent-runner.md) (`asp sandbox run`).  
Identidad del dueño: [`adr/0007-multi-user-identity.md`](adr/0007-multi-user-identity.md).

## Por qué

Un agente es un proceso largo. OpenCode (y cualquier harness parecido) no lanza un comando y se va: llama al shell decenas de veces, con el resultado anterior todavía en disco, durante horas.

`asp sandbox run` hace create → exec → destroy. Sirve para un one-liner de CI o de ops. Como forma de «usar el aislamiento» dentro del bucle del agente, falla:

1. Cada tool paga el arranque de la microVM. Con Cloud Hypervisor eso es la latencia. Con FakeVMM el coste se esconde y el diseño parece inocuo.
2. El guest muere entre medias. No hay workspace acumulado: lo que el comando N instaló no existe en el N+1.
3. El harness, si el sandbox es caro, ejecuta el shell en el host. El producto deja de existir justo cuando el código no es de confianza.

La plataforma ya tenía la pieza (`asp session` + reaper `ASP_SANDBOX_IDLE_TIMEOUT`) escrita como contrato *adicional*. El relato de producto seguía siendo el one-shot. Hay que invertirlo.

## Qué ganamos

- **Un objeto claro:** la sesión. Un agente, un sandbox, mientras dure el trabajo.
- **Identidad estable:** `owner_sub` lo pone el CP desde el JWT del IdP al hacer `start`. No viaja en `session.json` y el guest no lo elige.
- **Workspace de la sesión = disco del guest** entre execs (ficheros, procesos, `/tmp`). Es lo que permite encadenar tools *dentro* de la frontera.
- **Egress de esa microVM** (allowlist del tenant, TAP, nft) durante toda la sesión, no rearmado por comando.
- **Fin explícito o por idle:** `asp session stop`, o el reaper del CP si `ASP_SANDBOX_IDLE_TIMEOUT` está encendido (lab: `2h`; el binario por defecto lo tiene **apagado** para no romper smokes). Actividad = create, paso a `running`, exec proxyado bien.
- **Enganche del harness sin protocolo nuevo:** el shell del tool apunta a `asp session exec`. El POST de exec sigue siendo el dataplane, no la integración.
- **La primitiva one-shot no se tira.** CI, smokes y un comando que debe morir con la VM siguen en `asp sandbox run`.

```text
start sesión (una vez) → tools horas vía exec → stop  |  idle reap
         ▲                              │
         owner_sub, egress, disco guest ┘
```

## Qué no ganamos (límites honestos)

- **No hay share del workspace del host.** virtiofs / copia del repo no está. El agente aislado no ve el checkout de fuera, ni al revés.
- **No hay plugin de OpenCode.** Hay que cablear el binario fuera de este repo.
- **Un solo fichero local** (`~/.cache/asp/session.json`, o `ASP_SESSION_FILE`). Dos agentes, un `$HOME`, se pisan. El fichero es un puntero, no una capability y no lleva el token.
- **El exec no es un PTY ni un stream.** JSON acumulado al terminar. Sin stdin interactivo.
- **Idle global y opcional.** Sin la variable, una sesión olvidada no se apaga sola. No hay umbral por sesión. `GET` y el heartbeat no mantienen la VM viva.
- **Estado compartido es también el riesgo:** un tool deja basura o un proceso para el siguiente. Quien necesite borrar eso usa la primitiva one-shot, no niega la sesión.
- **Dry-run no demuestra KVM.** Los tests de sesión son `httptest`.

Detalle normativo, alternativas (plugin, SSH/PTY, sandbox compartido, JWT en el puntero, GC solo en el CLI) y consecuencias: el ADR-0009.
