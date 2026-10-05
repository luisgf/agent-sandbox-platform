# Varios servidores de microVMs

Cómo añadir capacidad con más nodos. Las decisiones están en [ADR-0011](adr/0011-multi-node.md).

## Qué hace el plano de control

- **Coloca cada sandbox al crearla.** Mira los nodos planificables y lo que ya tienen asignado, y elige uno. Si ninguno cabe, la creación falla al momento con **503** y el motivo. No hay cola.
- **Un nodo es planificable si:** tiene `agent_endpoint`, no está revocado ni `offline`, ha dado señales en los últimos `ASP_NODE_STALE_AFTER` (90 s por defecto: heartbeat o sondeo de trabajo), corre con `--reconcile`, no está en `cordon`, admite el `vmm_profile` y le queda CPU, memoria y hueco (`--max-sandboxes`).
- **Uso** = suma de las sandboxes del nodo en `requested`, `starting`, `running`, `paused` o `stopping`. `stopping` sigue contando: la VM puede no haberse apagado.
- **Solo el nodo elegido** recibe la sandbox como trabajo y puede reclamarla.

## Capacidad

Cada node-agent declara lo que ofrece al registrarse:

| Flag | Por defecto | Significado |
|---|---|---|
| `--capacity-cpu` | `-1` → núcleos del host | Cores. `0` = no se limita. |
| `--capacity-mem-mib` | `-1` → `MemTotal − max(1 GiB, 10 %)` | MiB. `0` = no se limita. Sin `/proc/meminfo` (macOS, dry-run) no se limita. |
| `--max-sandboxes` | `0` | Tope de sandboxes; `0` = sin tope. |
| `--local-net-dial` | vacío | `host[:puerto]` que marca el portátil para local-net en este nodo. Si falta, el plano de control usa `ASP_LOCAL_NET_DIAL`. |

En el plano de control:

| Variable | Por defecto | Significado |
|---|---|---|
| `ASP_SCHED_POLICY` | `spread` | `spread`: el nodo menos cargado tras colocar. `binpack`: el más cargado que aún cabe (deja nodos libres). |
| `ASP_SCHED_CPU_OVERCOMMIT` | `4` | vCPU por core físico. Las sesiones de agentes esperan al modelo casi siempre. La memoria nunca se sobresuscribe. |
| `ASP_NODE_STALE_AFTER` | `90s` | Sin señales durante más tiempo, el nodo deja de recibir sandboxes. |

Ejemplo: un servidor de 16 cores y 64 GiB, con los valores por defecto, ofrece 64 vCPU y 58 GiB. Caben 64 sandboxes de 1 vCPU y 512 MiB, porque la CPU se acaba antes que la memoria.

## Añadir un servidor

Requisitos:

- **Postgres** en el plano de control (`DATABASE_URL`). Con el store en memoria todo se pierde al reiniciar; los agentes se vuelven a registrar solos, pero las sandboxes no.
- **TLS** en el plano de control (`ASP_TLS_CERT`/`ASP_TLS_KEY`) y **`ASP_CLIENT_CA`** apuntando a la CA de enrollment: así cada ruta de nodo exige el certificado de ese nodo.
- La misma CA de enrollment para todos los nodos (`ASP_CA_CERT`/`ASP_CA_KEY`, fuera de `/tmp`).

Pasos en el servidor nuevo (`node2`):

```bash
node-agent \
  --control-plane-url=https://cp.ejemplo.corp:8443 \
  --control-plane-ca=/etc/asp/cp-ca.pem \
  --node-id=node2 \
  --enroll --bootstrap-token="$ASP_NODE_BOOTSTRAP_TOKEN" \
  --cert-dir=/var/lib/asp/node-certs --mtls \
  --agent-listen=127.0.0.1:9100 \
  --agent-tls-listen=0.0.0.0:9443 \
  --endpoint=https://node2.ejemplo.corp:9443 \
  --reconcile \
  --guest-subnet=10.200.16.0/20 \
  --local-net-dial=node2.ejemplo.corp
```

Después, desde un puesto con rol admin u operador:

```bash
asp node list
```

El nodo debe salir como `SCHEDULABLE yes`.

**Firewall:**

| Origen → destino | Puerto | Para qué |
|---|---|---|
| nodo → plano de control | puerto TLS del API | register, heartbeat, trabajo, claim, estado |
| plano de control → nodo | `9443/tcp` (`--agent-tls-listen`) | `exec` con mTLS |
| portátil → nodo | `47000–54999/udp` | solo local-net (WireGuard) |

`9100` no debe abrirse: es HTTP sin autenticar y solo escucha en loopback.

**Subredes del guest:** dale a cada nodo un rango distinto dentro de `10.200.0.0/16` (`--guest-subnet`). Solo importa para local-net: un portátil con sesiones en dos nodos con el mismo rango vería las mismas IPs de guest.

## Mantenimiento

- **Sacar un nodo del reparto:** `asp node cordon node2`. Las sandboxes que ya corren siguen ahí; no se colocan nuevas.
- **Drenar** (para apagar o actualizar): `cordon`, y esperar a que `asp node list` muestre `0/…` sandboxes, o pedir a los usuarios que paren sus sesiones. Las sesiones no se migran: el disco del guest vive en ese servidor.
- **Volver al reparto:** `asp node uncordon node2`.
- **Retirar un nodo para siempre:** `POST /v1/nodes/{id}/revoke`. Un nodo revocado no vuelve con un heartbeat; necesita re-enrolar.

**Orden de actualización:** primero los node-agents y después el plano de control. Un agente antiguo declara siempre 4 cores y 8 GiB, y el planificador nuevo aplica esos valores.

## Diagnóstico

| Síntoma | Causa probable |
|---|---|
| `503 no schedulable nodes registered` | Ningún nodo registrado con `--reconcile`, o el agente aún no se ha registrado. |
| `503 no node can fit … (2 nodes: 1 cordoned, 1 max_sandboxes)` | Los motivos cuentan por qué se descartó cada nodo. Añade nodos, haz `uncordon`, o espera a que terminen sesiones. |
| `409 node X is not registered` / `cannot take sandboxes: stale` | `--node-id` fijado a un nodo desconocido o caído. Quita el pin o revisa ese nodo. |
| `502 … x509: certificate is valid for nodeA, not nodeB` | El `agent_endpoint` de un nodo apunta al agente de otro. Revisa `--endpoint`. |
| `502 … is plain HTTP on a non-loopback host` | Endpoint `http://` hacia otra máquina. Usa `--agent-tls-listen`. |
| `403 client certificate is for node …` | El agente usa un `--node-id` distinto del CN de su certificado. Quita `--node-id` o re-enrola. |
| El agente no arranca: `node certificate cannot serve TLS` | Certificado anterior a ADR-0011. Re-enrola con `--enroll` o `rotate-cert`. |

## Límites

- No hay migración: si un servidor se pierde, sus sesiones se pierden con él.
- La colocación no mira `--workspace`: la ruta debe existir en el nodo elegido. Con varios nodos, fija el nodo (`--node-id`) o comparte la ruta en todos.
- El kernel y el rootfs son locales a cada nodo (`/opt/sandbox`). Mantenlos iguales.
