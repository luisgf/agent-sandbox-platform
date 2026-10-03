# Ops — Red local bajo demanda (`--local-net`)

Contrato: [ADR-0010](adr/0010-on-demand-local-net.md). Por qué: [`why-on-demand-local-net.md`](why-on-demand-local-net.md).

Esto es el **corte mínimo**. Enciende el flag, el handshake y el plan de blackhole. **No** levanta un interfaz WireGuard en el kernel ni hace NAT. Sin `wireguard-tools` y sin privilegios de red en el nodo y en el portátil no hay paquetes.

## Encender

Default **apagado**. Un `asp session start` normal no manda `local_net`. El egress sigue el proxy del nodo (ADR-0002).

```bash
asp session start --name agente --node-id=dev-node --local-net
```

Equivale a `POST /v1/sandboxes` con `"local_net": true` y nada más. No hay CIDR ni puertos. Un body con `local_net_policy`, `prefixes`, `cidrs`, `ports`, `routes` o `exceptions` responde **400**. `--local-net-allow` en la CLI sale 2: no existe en v1.

El sandbox puede llegar a `running` con `local_net_state=pending`. En ese estado el nodo **no** instala la default pública de esa sesión: el plan es blackhole (`0.0.0.0/0` y `::/0` en la tabla de esa sesión). No hay redirect de ese sandbox a `:8888`.

## Túnel (handshake)

Cuando la sesión está viva:

```bash
asp session local-net up --name agente
asp session local-net down --name agente
```

`up` pide `POST /v1/sandboxes/{id}/local-net/grant` y luego `POST .../local-net/heartbeat` con la clave pública. `down` es `DELETE .../local-net/attach` y deja `local_net_state=withdrawn` **sin** volver al proxy.

Ficheros, todos modo `0600`, al lado del JSON de sesión, **fuera de git**:

| Fichero | Contenido |
|---|---|
| `~/.cache/asp/sessions/<nombre>.json` | `local_net: true`. Sin grant y sin clave privada |
| `<nombre>.json.local-net.key` | Clave privada WireGuard (X25519, base64 estándar). Solo en `up` |
| `<nombre>.json.local-net.conf` | Esqueleto `[Interface]` para `wg`. **No se aplica solo** |
| `<nombre>.json.local-net.plan` | Clave pública, iface `wg-asp-…`, si `wg` está en el PATH. Sin secreto |

El grant en claro no se escribe. El control plane guarda el sha256 y `local_net_grant_expires_at` (10 minutos). Un heartbeat con el grant caducado pasa a `withdrawn`.

`asp session stop` hace detach y borra la clave antes del `DELETE` del sandbox. El idle reap (`stop_reason=idle_timeout`) también retira el túnel. Ni el heartbeat ni los bytes del túnel mueven `last_activity_at`.

Identidad: `owner_sub` del create. Otro sujeto con JWT no obtiene grant. El guest (`X-ASP-Caller: guest`, o un `local_net` en el exec) recibe 403.

## Qué tiene que haber en el host (y no hay aquí)

- `wireguard-tools` (`wg`) en el portátil y en el nodo.
- Capacidad de crear `wg-asp-{id8}` y una tabla de rutas **distinta** de la default del nodo (`aspln-{id8}`), más NAT en un netns del agente. `AllowedIPs = 0.0.0.0/0` en la tabla principal del portátil o del nodo está prohibido.
- `ASP_LOCAL_NET_DIAL` si el cliente debe saber a dónde marcar. Vacío = el JSON de grant trae `dial` vacío y el dataplane no tiene a quién hablar. El estado puede quedarse en `pending` con el egress hundido. El control plane no reenvía el payload.

FakeVMM en CI solo comprueba el flag, el 403 del guest, el 400 de una policy, y que al desconectar el plan sigue siendo blackhole y no el proxy.
