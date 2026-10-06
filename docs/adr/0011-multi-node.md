# ADR-0011: Varios nodos — identidad, canal plano de control ↔ nodo, colocación por capacidad y nodos caídos

- **Estado:** Aceptada (2026-10-05). Identidad, canal, colocación y nodos caídos implementados; sin lab KVM con varios servidores reales.
- **Fecha:** 2026-10-05
- **Extiende:** [0005](0005-fase-2d-hardening.md) (identidad de nodo = certificado cliente mTLS), [0004](0004-k8s-scope.md) (el planificador de capacidad vive en nuestro plano de control)
- **Relacionados:** [0010](0010-on-demand-local-net.md) (`Node.agent_endpoint`), [`../architecture.md`](../architecture.md), [`../bare-metal-ch.md`](../bare-metal-ch.md)

## Contexto

El modelo ya tenía nodos: enroll con certificado (CN = node id, OU `nodes`), register, heartbeat, `node_id` en cada sandbox y una cola de trabajo por nodo. En la práctica solo funcionaba con **un** servidor, y con el plano de control en la misma máquina:

1. **El `exec` iba por HTTP sin autenticar.** El plano de control llamaba a `agent_endpoint` (por defecto `http://127.0.0.1:9100`) con un cliente HTTP sin TLS ni certificado. Para alcanzar un nodo en otra máquina había que abrir ese puerto a la red, y quien llegara a él podía ejecutar comandos en cualquier sandbox del nodo y reescribir su política de egress (el cuerpo del `exec` la lleva).
2. **La identidad del nodo no se comprobaba.** El middleware solo exigía que hubiera *un* certificado cliente válido. Cualquier nodo enrolado podía reclamar, renovar, cambiar el estado, atestar, pedir tokens OIDC o publicar claves de local-net de sandboxes de otro nodo. Con `register` podía además cambiar el `agent_endpoint` de otro nodo y recibir su tráfico de `exec`.
3. **Un nodo revocado volvía solo a `ready`** con el siguiente heartbeat o register.
4. **`GET /v1/nodes` devolvía `fence_token`**, que puede ser la contraseña del BMC o del IPMI del servidor.

Con un solo nodo en loopback estos huecos no se notaban. Con varios servidores son la frontera.

## Decisión

### 1. La identidad del nodo sale del certificado, en todas las rutas de nodo

Con `ASP_CLIENT_CA` configurado, el middleware toma el CN del certificado cliente (que debe llevar OU `nodes`) y lo deja en el contexto de la petición. Cada ruta de nodo lo compara con el nodo para el que actúa:

| Ruta | Debe coincidir con el CN |
|---|---|
| `POST /v1/nodes/register` | `id` del cuerpo (vacío → el CN) |
| `POST /v1/nodes/{id}/heartbeat`, `GET /v1/nodes/{id}/work` | `{id}` |
| `claim`, `renew-lease` | `node_id` del cuerpo (vacío → el CN) |
| `status`, `POST /v1/internal/oidc/token`, `local-net/node-public` | `node_id` de la sandbox |
| `attest` | `node_id` de la sandbox y `statement.node_id` |

Si no coincide: **403**. En el lab abierto o con API key (sin certificado verificado) el comportamiento no cambia. Dentro de un nodo, qué sandbox pide un token OIDC lo decide el node-agent por la conexión vsock del guest ([0003](0003-identity.md) § 2). Los certificados existentes ya llevan CN = node id y OU `nodes`: no hace falta re-enrolar para esto.

Un nodo revocado recibe **409** en heartbeat y register hasta que vuelve a enrolar (ADR-0005: revocar exige re-enroll).

### 2. Canal plano de control → nodo con mTLS

El node-agent gana un segundo listener, `--agent-tls-listen` (p. ej. `0.0.0.0:9443`):

- Sirve **solo** `exec`, `exec/stdin` y `healthz`. Las rutas de operador (`ssh-agent/approve`, `egress-check`) siguen únicamente en el listener de loopback.
- Usa el certificado del nodo como certificado de servidor. Los certificados de nodo pasan a llevar los dos usos (cliente y servidor) y SAN = node id + hosts del `agent_endpoint`.
- Exige certificado cliente de la CA de enrollment y acepta **solo** la identidad del plano de control: CN `asp-control-plane`, OU `control-plane`. El certificado de otro nodo, de la misma CA, no entra.

El plano de control:

- Se emite su certificado cliente desde su CA, en memoria, con 30 días de vida y renovación a los dos tercios.
- Para endpoints `https://` confía solo en su CA y pone **`ServerName = node id`**. Así comprueba que contesta el nodo al que quiere llegar, aunque alguien registre un endpoint que apunte a otro nodo, y sin depender del nombre de host del endpoint (cambiar de IP no exige certificado nuevo).
- Mantiene un cliente por nodo y endpoint, para reutilizar conexiones.

### 3. HTTP plano solo en loopback

- El plano de control rechaza (400 en register/enroll, 502 en `exec`) endpoints `http://` fuera de loopback, salvo `ASP_INSECURE_AGENT_HTTP=1`.
- El node-agent no arranca con `--agent-listen` fuera de loopback, salvo `--insecure-agent-listen`.

Las dos salidas existen para laboratorios. Dejan el `exec` sin autenticar y se avisan en el log.

### 4. Secretos de fencing

`fence_token` y `fence_endpoint` no se serializan nunca. Siguen en el store para el fencing. `GET /v1/nodes` exige rol admin u operador cuando hay principal de IdP.

### 5. Enroll contra un plano de control remoto

- `--control-plane-ca`: CA del certificado TLS del plano de control, para enroll y llamadas.
- `--enroll-url`: con `ASP_MTLS_STRICT` el enroll vive en otro listener.
- Sin `--node-id`, un nodo enrolado usa el CN de su certificado, porque el plano de control rechaza cualquier otro id.
- **Actualizado 2026-10:** para un servidor nuevo, un admin pide un token de un solo uso con `asp node enroll-token --node-id node2` y el agente lo usa con `--enroll-token`. El bootstrap token compartido ya no re-enrola un nodo con certificado vigente: así nadie que lo tenga suplanta a un nodo ni revoca su certificado ([ADR-0005](0005-fase-2d-hardening.md) § 1). Un agente reiniciado con `--enroll` sigue con el certificado que ya tiene.

### 6. Colocación por capacidad, en el plano de control

El plano de control elige el nodo **al crear** la sandbox (ADR-0004: el planificador vive aquí):

- **Filtros**, en este orden: sin `agent_endpoint` (fila stub), revocado, `offline`, sin señales desde hace más de `ASP_NODE_STALE_AFTER` (90 s), sin `--reconcile` (`accepts_work`), en `cordon`, sin el `vmm_profile` pedido, sin CPU, sin memoria, sin hueco (`max_sandboxes`).
- **Capacidad:** el nodo declara cores, MiB y tope de sandboxes; `0` significa que esa dimensión no se limita. La CPU se sobresuscribe `ASP_SCHED_CPU_OVERCOMMIT` veces (4 por defecto: las sesiones de agentes esperan al modelo casi siempre); `cpu_millis` es un peso para colocar, y la VM recibe `max(1, cpu_millis/1000)` vCPU. La memoria no se sobresuscribe nunca: cada sandbox cuenta su `memory_mib` (64 MiB como mínimo) más `ASP_SCHED_VM_OVERHEAD_MIB` (64 por defecto) por el propio VMM.
- **Uso:** suma de las sandboxes del nodo en `requested`…`stopping`. Se calcula de las propias filas, sin contadores que se desincronicen.
- **Política** (`ASP_SCHED_POLICY`): `spread` (por defecto) elige el nodo menos cargado tras colocar; `binpack`, el más cargado que aún cabe. La carga es la utilización dominante entre las dimensiones que se limitan. Desempate: menos sandboxes (o más, en `binpack`) y luego el id del nodo.
- **Sin hueco: rechazo inmediato.** `503` con `Retry-After` y el recuento de motivos. Un pin (`node_id`) a un nodo desconocido o que no admite sandboxes da `409`; a un nodo lleno, `503`.
- **Atomicidad:** en memoria se decide e inserta bajo el mismo lock. En Postgres, un `pg_advisory_xact_lock` serializa las colocaciones, también entre réplicas, y nodos y uso se leen dentro de la transacción.
- **Solo el nodo elegido** ve la sandbox en `/work` y puede reclamarla. El claim ya no recupera leases caducados de otro nodo.
- **Cordon:** `POST /v1/nodes/{id}/cordon|uncordon` (admin). Un nodo que se vuelve a registrar no levanta el cordon.
- **Local-net:** el grant devuelve el `local_net_dial` del nodo de la sandbox y, si no tiene, `ASP_LOCAL_NET_DIAL`.
- Destruir una sandbox que ningún nodo ha reclamado la deja en `stopped` al momento: antes del claim no existe VM.

### 7. Nodos caídos: detectar, aislar y fallar (sin mover)

- **Señales de vida:** heartbeat (30 s) y cada sondeo de trabajo (~2 s, escritura limitada a una cada 5 s).
- **Monitor de nodos** en el plano de control, cada `ASP_NODE_MONITOR_INTERVAL` (15 s):
  - sin señales más de `ASP_NODE_STALE_AFTER` (90 s) → el nodo pasa a `offline` (el planificador ya lo había descartado);
  - sin señales más de `ASP_NODE_FAILOVER_AFTER` (5 min; `0`/`off` lo desactiva) → **fencing** del nodo si tiene sandboxes (una vez por caída, `FenceProvider`) y sus sandboxes pasan a `failed` con `stop_reason=node_lost` (`stopping` → `stopped`);
  - un nodo **revocado** se trata como perdido al momento.
- El silencio se mide desde lo más reciente entre la última señal del nodo y el arranque del monitor: reiniciar el plano de control tras una parada no tumba todas las sandboxes.
- El store vuelve a comprobar que el nodo sigue perdido al bloquear las filas: un heartbeat que llega antes gana.
- **Las sandboxes se fallan, no se mueven:** el disco del guest vive en su servidor. El usuario abre una sesión nueva; el CLI lo explica.
- **Autodefensa del nodo:** si renovar el lease o reportar `running` devuelve 409, el node-agent para la VM local sin reportar estado. Un nodo que vuelve tras una partición no deja copias vivas.
- **Transiciones validadas:** `stopped` es final, `failed` solo va a `stopped`, `stopping` solo termina. Un informe tardío no resucita nada.
- **Reinicio del agente:** cada proceso envía un `agent_instance_id` aleatorio. Si cambia, las sandboxes `running`/`paused` del nodo fallan con `node_agent_restarted` (el agente no adopta VMs); `requested` y `starting` las arranca el proceso nuevo.
- **Lo que deja un agente anterior se borra:** antes de registrarse, el proceso nuevo para los `cloud-hypervisor` y `virtiofsd` de su `--ch-socket-dir` (por el socket de su argv) y borra sus TAPs, túneles local-net, sockets y copias del rootfs. Así, cuando el plano de control da una sandbox por fallida, su VM ya no corre. La unit de systemd (`KillMode=control-group`) las para incluso antes, con el agente. Un `flock` en el directorio de sockets impide que un segundo agente tome por restos las VMs de uno vivo ([bare-metal §5.6](../bare-metal-ch.md#56-servicio-systemd-y-reinicios-del-agente)).
- **Filas antiguas** `requested` sin nodo (de antes de la colocación al crear) fallan como `unscheduled`.

## Alternativas consideradas

- **Comprobar el nombre de host del endpoint en vez del node id.** Ata el certificado a una IP o nombre concretos; cambiar de dirección exigiría re-emitir. Además no prueba *qué* nodo contesta si dos nodos comparten nombre. Rechazada.
- **Pasar `--agent-listen` a HTTPS.** Rompe a los operadores que llaman a `approve` y `egress-check` con `curl http://127.0.0.1:9100` en el propio nodo, y al plano de control en la misma máquina. Un listener aparte, restringido a lo que necesita el plano de control, es más pequeño de auditar.
- **Token compartido en cabecera en vez de mTLS.** Un secreto más que repartir y rotar, y no autentica al servidor. La PKI de enrollment ya existe.
- **Cola de sandboxes pendientes** cuando no hay hueco. Esconde la falta de capacidad tras un `asp session start` que espera hasta su timeout. Se prefirió fallar rápido con un motivo claro.
- **Que los nodos se repartan el trabajo reclamando.** Era el modelo anterior: todos veían las sandboxes sin asignar y competían. No respeta la capacidad y deja que un nodo se quede con trabajo de otro.
- **Contadores de uso por nodo.** Se desincronizan con cada caída o reintento. Sumar las filas cuesta una consulta indexada (`sandboxes_node_state_idx`).
- **Failover por caducidad del lease de cada sandbox.** Las renovaciones comparten el tick del reconciler, y un arranque de VM puede bloquearlo hasta 30 s: se fallarían VMs sanas. La señal de vida es del nodo (heartbeat y sondeos).
- **Recolocar en otro nodo las sandboxes `requested` de un nodo perdido.** No se sabe si el usuario fijó ese nodo; fallar es honesto y el cliente puede reintentar.
- **Adoptar las VMs vivas tras reiniciar el agente** (reengancharse al socket de API de cada CH y conservar el `agent_instance_id`). Aplazada. `/work` no devuelve las sandboxes `running` sin local-net. Con el id conservado, una VM que no sobrevivió al reinicio seguiría `running` en el plano de control sin que nadie la falle, porque el lease caducado no falla sandboxes. Haría falta que el registro declare qué sandboxes sigue llevando el agente, y que el plano de control falle el resto. Además, el handle se reconstruye con estado que CH no guarda: el `owner_sub` del SSH agent, la política de egress, los listeners guest→host (mueren con el proceso) y la reserva de CID y /30. Con la unit de systemd (`KillMode=control-group`) ninguna VM sobrevive al agente, así que adoptar solo serviría con `KillMode=process`.

## Consecuencias

### Positivas

- Un nodo comprometido ya no puede actuar en nombre de otro, ni desviar su tráfico de `exec`.
- El plano de control puede vivir en otra máquina sin abrir un `exec` sin autenticar a la red.
- Un nodo revocado se queda revocado.
- Añadir un servidor añade capacidad sin configurar nada en el plano de control, y nunca se llena un nodo por encima de lo que declara.
- Un servidor caído deja de recibir trabajo en 90 s, y en 5 min sus sesiones se dan por perdidas en vez de quedarse colgadas con 502.

### Negativas / coste

- **Cambio que rompe (seguridad):** quien hoy alcance un agente por `http://` desde otra máquina debe pasar a `--agent-tls-listen` o activar las salidas inseguras.
- Los nodos enrolados antes de este cambio tienen certificados solo de cliente: para usar `--agent-tls-listen` hay que re-enrolar (`--enroll`) o `rotate-cert`. El agente lo dice al arrancar.
- Los node id deben poder ser nombre de certificado: letras, dígitos, `.`, `-` y `_`, y no `asp-control-plane`. El enroll rechaza los demás con 400.
- Crear sin ningún nodo planificable da 503 en vez de dejar la sandbox en `requested`. Un pin a un nodo desconocido da 409.
- Un agente anterior a este cambio declara siempre 4 cores y 8 GiB, y ahora se aplica: hay que actualizar los agentes antes que el plano de control.

### Límites honestos

- La identidad solo se exige con `ASP_CLIENT_CA`. En el lab abierto cualquiera que alcance el plano de control puede hablar como cualquier nodo, como antes.
- El certificado del plano de control vive en memoria. Varias réplicas del plano de control se emiten cada una el suyo, todos de la misma CA.
- El plano de control ya no corta un `exec` en streaming: limita cada fase de la llamada al agente hasta que este empieza a responder, no el stream ([detalle](../../control-plane/README.md#timeouts-hacia-el-node-agent)). El node-agent sí lo corta a los 60 s: es el timeout total de su cliente hacia el pod-daemon.
- La colocación no conoce `--workspace`: la ruta tiene que existir en el nodo elegido.
- Sin migración: el disco del guest vive en su nodo.
- Con varias réplicas del plano de control, cada una corre su monitor: el CAS lo hace seguro, pero el fencing podría repetirse.
- El agente no adopta VMs tras reiniciar: las sandboxes fallan (sin mentir) y sus VMs se paran, también al actualizar el agente. Hay que drenar el nodo antes.
- Un node-agent por host: al arrancar borra todos los TAPs `asp-*` y túneles `wg-asp-*`, que son del host entero.
