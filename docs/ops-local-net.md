# Ops — Red local bajo demanda (`--local-net`)

Contrato: [ADR-0010](adr/0010-on-demand-local-net.md). Por qué: [`why-on-demand-local-net.md`](why-on-demand-local-net.md).

El nodo y el CLI **sí lanzan** `ip` y `wg` para crear `wg-asp-{id8}`. No es un esqueleto que se queda en disco. Probado a mano en un host KVM (Ubuntu 26.04, octubre de 2026) con dos sesiones a la vez y el cliente Linux en un network namespace: cada sandbox llegaba a la red de su propio cliente por su túnel, la del otro le daba `Network is unreachable` y el otro túnel no contaba ni un byte. **Lo que no se ha comprobado**: el reenvío y el NAT hasta una LAN real, el DNS de casa, el cliente macOS de punta a punta, y nada de esto corre en CI.

La default que se mueve es la **de esa sesión** (tabla de policy routing, `iif` del TAP `asp-{id8}`). No se instala `ip route replace default` en la tabla principal del nodo ni del portátil. `AllowedIPs = 0.0.0.0/0` en `wg set` elige el peer; no añade esa ruta al sistema. Sin el flag, el egress sigue siendo el proxy del nodo (ADR-0002).

## Encender

Default **apagado**. Un `asp session start` normal no manda `local_net`.

```bash
asp session start --name agente --local-net
```

Equivale a `POST /v1/sandboxes` con `"local_net": true` y nada más. No hay CIDR ni puertos. Un body con `local_net_policy`, `prefixes`, `cidrs`, `ports`, `routes` o `exceptions` responde **400**. `--local-net-allow` en la CLI sale 2: no existe en v1.

El sandbox puede llegar a `running` con `local_net_state=pending`. En ese estado el node-agent borra `wg-asp-…` si existía e instala blackhole de `0.0.0.0/0` y `::/0` en la tabla de esa sesión (`10000`–`29999`, nunca `main`). No hay redirect de ese sandbox a `:8888`. Si `ip` no puede hacerlo, el Start falla: no se deja el proxy «por si acaso».

El node-agent genera una clave X25519, la guarda en `ASP_LOCAL_NET_KEY_DIR` (por defecto `/var/lib/asp/local-net/<id>.key`, modo `0600`; en dry-run, un directorio bajo el temp) y publica solo la pública en `POST /v1/sandboxes/{id}/local-net/node-public`. El guest no puede llamar a eso.

## Túnel

Cuando la sesión está viva y el nodo ya publicó su clave:

```bash
asp session local-net up --name agente
asp session local-net down --name agente
```

`up` pide `POST /v1/sandboxes/{id}/local-net/grant` y luego `POST .../local-net/heartbeat` con la clave pública del portátil. El grant trae `node_public_key`, `listen_port`, `node_tunnel_addr`, `client_tunnel_addr` y `tunnel_iface`. Puerto, direcciones y clave los asigna y publica el node-agent al arrancar la sesión. Si aún no lo ha hecho, el handshake igual pasa a `up` y el CLI **no** crea el dispositivo: dice qué falta (clave, puerto o dirección) y hay que repetir `up`.

Si `wg` e `ip` están en el `PATH` y el proceso tiene `CAP_NET_ADMIN`, `up` ejecuta, en este orden aproximado:

```text
ip link add dev wg-asp-… type wireguard
ip address add <client /30> dev wg-asp-…
cat <fichero 0600> | wg set wg-asp-… private-key /dev/stdin peer <clave nodo> allowed-ips 0.0.0.0/0,::/0 endpoint <dial del grant> persistent-keepalive 25
ip link set wg-asp-… up
ip route add 10.200.0.0/16 dev wg-asp-…
```

La clave llega a `wg` por una tubería, no como ruta. El perfil AppArmor de `wg` en Ubuntu 26.04 solo le deja abrir ficheros bajo `/etc/wireguard`, y con una ruta `wg set` falla con `fopen: Permission denied`. El nodo hace lo mismo con `/var/lib/asp/local-net/<id>.key`, así que no hace falta ningún override en `/etc/apparmor.d/local/wg`.

El guest conserva su dirección dentro del túnel. Sin la ruta a `10.200.0.0/16` (el pool por defecto de `--guest-subnet`, el mismo que enruta el cliente de macOS), las respuestas del portátil saldrían por su ruta por defecto. No es una ruta por defecto. Solo una sesión por portátil la tiene: el `add` de una segunda sesión la encuentra puesta y la deja.

No hay `ip route … default` sin `table`. Si falta la capability o falta `wg`, el CLI imprime esos argv y no los ejecuta. `ASP_LOCAL_NET_APPLY=0` tampoco los ejecuta. `=1` solo fuerza el intento en un lab que ya tiene las herramientas (los tests lo usan con un `wg` falso en el `PATH`).

En el nodo, con `local_net_state=up` y la clave pública del cliente, el reconciler hace lo mismo en `wg-asp-…` (peer = agente local, `listen-port` estable por sandbox) y luego:

```text
ip route replace default dev wg-asp-… table <id>
ip route replace ::/0 dev wg-asp-… table <id>
ip rule add iif asp-… lookup <id> priority <id>
```

`down` borra el iface del portátil (`ip link delete`) y hace `DELETE .../local-net/attach`. El estado queda `withdrawn`. El nodo, en el siguiente reconcile, borra el iface e instala otra vez el blackhole. No rearma `:8888`.

Ficheros, todos modo `0600`, al lado del JSON de sesión, **fuera de git**:

| Fichero | Contenido |
|---|---|
| `~/.cache/asp/sessions/<nombre>.json` | `local_net: true`. Sin grant y sin clave privada |
| `<nombre>.json.local-net.key` | Clave privada WireGuard (X25519, base64 estándar). Solo en `up` |
| `<nombre>.json.local-net.conf` | `[Interface]` + `[Peer]`. `Table = off` recuerda que **no** hay que usar `wg-quick` (instalaría `0.0.0.0/0` en la tabla principal). La aplica el CLI con `ip`+`wg`, no `wg-quick` |
| `<nombre>.json.local-net.plan` | Clave pública, iface, si el dispositivo se aplicó. Sin secreto |

El grant en claro no se escribe. El control plane guarda el sha256 y `local_net_grant_expires_at` (10 minutos). Un heartbeat con el grant caducado pasa a `withdrawn`.

`asp session stop` y `asp session rm` hacen detach, borran el iface local si pueden, y borran la clave (el `rm`, antes del `DELETE` del sandbox). El idle reap (`stop_reason=idle_timeout`) pone la sesión en `stopping` y `withdrawn`; el node-agent **borra** su iface al parar la VM. Reanudar una sesión parada no restablece el túnel: `asp session local-net up` otra vez. El reap no corre en el portátil: el iface de allí se quita con `down`, `stop` o `rm`. Ni el heartbeat ni los bytes del túnel mueven `last_activity_at`.

Identidad: `owner_sub` del create. Otro sujeto con JWT no obtiene grant. El guest (`X-ASP-Caller: guest`, o un `local_net` en el exec) recibe 403.

## Qué tiene que haber en el host

- `wireguard-tools` (`wg`) y `iproute2` en el portátil y en el nodo.
- `CAP_NET_ADMIN` (en la práctica, root o una capability acotada) para crear el dispositivo. Sin eso el CLI solo imprime los comandos y el nodo falla el Start de un sandbox con `local_net=true` en vez de caer al proxy.
- `nft` en el portátil si se quiere el MASQUERADE (`iifname` del `wg-asp-…`). Si no está, el dispositivo puede crearse y el NAT no se instala. Este repo no ha comprobado el reenvío hasta una LAN real (solo hasta el namespace del cliente).
- Una dirección del nodo alcanzable desde el portátil: `--local-net-dial` en cada node-agent (`host` o `host:puerto`). Si el nodo no la declara, el grant usa `ASP_LOCAL_NET_DIAL` del plano de control, que solo vale con un único nodo. Vacío = el `wg set` del cliente no lleva `endpoint` y no hay paquetes. El control plane no reenvía el payload. Con varios nodos, abre UDP `47000–54999` hacia cada uno ([`ops-multi-node.md`](ops-multi-node.md)). Cada sesión tiene su propio puerto de ese rango, asignado por el nodo sin colisiones; el reparto se guarda en `<id>.alloc` junto a la clave en `ASP_LOCAL_NET_KEY_DIR`.
- No poner `AllowedIPs = 0.0.0.0/0` con `wg-quick` en la tabla principal. Está prohibido.

Los tests de unidad meten un script `wg` e `ip` en el `PATH` y comprueban el argv, incluido que la desconexión no menciona `:8888`. No cargan el módulo WireGuard del kernel.
