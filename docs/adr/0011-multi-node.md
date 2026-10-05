# ADR-0011: Varios nodos — identidad de nodo y canal plano de control ↔ nodo

- **Estado:** Propuesta. Esta revisión fija la identidad y el canal. La colocación por capacidad y la detección de nodos caídos se añadirán a este ADR en los cambios siguientes.
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

Si no coincide: **403**. En el lab abierto o con API key (sin certificado verificado) el comportamiento no cambia. Los certificados existentes ya llevan CN = node id y OU `nodes`: no hace falta re-enrolar para esto.

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

## Alternativas consideradas

- **Comprobar el nombre de host del endpoint en vez del node id.** Ata el certificado a una IP o nombre concretos; cambiar de dirección exigiría re-emitir. Además no prueba *qué* nodo contesta si dos nodos comparten nombre. Rechazada.
- **Pasar `--agent-listen` a HTTPS.** Rompe a los operadores que llaman a `approve` y `egress-check` con `curl http://127.0.0.1:9100` en el propio nodo, y al plano de control en la misma máquina. Un listener aparte, restringido a lo que necesita el plano de control, es más pequeño de auditar.
- **Token compartido en cabecera en vez de mTLS.** Un secreto más que repartir y rotar, y no autentica al servidor. La PKI de enrollment ya existe.

## Consecuencias

### Positivas

- Un nodo comprometido ya no puede actuar en nombre de otro, ni desviar su tráfico de `exec`.
- El plano de control puede vivir en otra máquina sin abrir un `exec` sin autenticar a la red.
- Un nodo revocado se queda revocado.

### Negativas / coste

- **Cambio que rompe (seguridad):** quien hoy alcance un agente por `http://` desde otra máquina debe pasar a `--agent-tls-listen` o activar las salidas inseguras.
- Los nodos enrolados antes de este cambio tienen certificados solo de cliente: para usar `--agent-tls-listen` hay que re-enrolar (`--enroll`) o `rotate-cert`. El agente lo dice al arrancar.
- Los node id deben poder ser nombre de certificado: letras, dígitos, `.`, `-` y `_`, y no `asp-control-plane`. El enroll rechaza los demás con 400.

### Límites honestos

- La identidad solo se exige con `ASP_CLIENT_CA`. En el lab abierto cualquiera que alcance el plano de control puede hablar como cualquier nodo, como antes.
- El certificado del plano de control vive en memoria. Varias réplicas del plano de control se emiten cada una el suyo, todos de la misma CA.
- El timeout de 30 s del cliente hacia el agente sigue cortando `exec` largos en streaming (fallo previo, fuera de este cambio).
