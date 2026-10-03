# Por qué / Qué ganamos — Red local bajo demanda

Ver también ADR: [`adr/0010-on-demand-local-net.md`](adr/0010-on-demand-local-net.md) (contrato vigente; **corte mínimo** en código, sin dataplane WireGuard real). Cómo encenderlo: [`ops-local-net.md`](ops-local-net.md).  
El egress del nodo, cuando el flag está apagado: [`adr/0002-networking.md`](adr/0002-networking.md).  
La sesión que acota la vida del túnel: [`adr/0009-agent-sessions.md`](adr/0009-agent-sessions.md).  
La identidad del flujo sigue siendo `owner_sub`: [`adr/0008-network-flow-attribution.md`](adr/0008-network-flow-attribution.md), [`adr/0007-multi-user-identity.md`](adr/0007-multi-user-identity.md).

La primera redacción de esta propuesta (commit `1858dd2`) decía que v1 era una allowlist de CIDR y puertos, y que `0.0.0.0/0` quedaba fuera. **Eso ya no es el diseño.** Pedir prefijos es demasiado difícil para quien solo quiere que el sandbox use su red. v1 es un flag, todo o nada.

## Por qué

El sandbox está en un nodo remoto. Su ruta por defecto sale por el proxy y la allowlist de ese nodo. La LAN del usuario (`192.168.1.0/24` y similares) no es enrutable desde ahí. El agente no puede hablar con el NAS, el dev server o el puerto que solo existe en casa.

Tampoco basta con «dejar salir solo esos prefijos». Quien arranca la sesión no suele conocer el CIDR, ni la lista de IPs que un comando va a tocar después de un DNS o de un redirect. Si el DNS se queda en el sink del nodo y los datos salen por casa, el camino está partido y miente. Si la lista de puertos es corta, el flujo real se cae. Si es larga, es un túnel completo escrito a mano.

Las salidas fáciles siguen siendo malas por otras razones. Abrir un agujero **hacia dentro** de la LAN (port forward, UPnP, un reverse tunnel dejado vivo) sobrevive a la sesión y no sabe de `owner_sub`. Instalar una VPN de sistema en el portátil secuestra también el tráfico del humano, no el de una microVM. Dejar que el propio guest elija la ruta no sirve: no tiene `NET_ADMIN` y, si lo tuviera, no sería frontera. Y si el túnel se cae y el nodo, sin decirlo, vuelve a sacar los paquetes por su proxy, el usuario tiene dos políticas y no ha elegido ninguna.

Hace falta un camino que:

- nazca **apagado** (`local_net=false` salvo `asp session start --local-net`, o el mismo booleano en `POST /v1/sandboxes`);
- lo abra el **agente local**, de dentro hacia fuera, sin publicar un puerto en el router;
- cuando esté on, lleve la **ruta por defecto** de esa sesión (`0.0.0.0/0` y `::/0` si esa default existe) por el túnel, **sin** formulario de CIDR ni de puertos;
- incluya el **DNS** en ese mismo camino, no en el sink del nodo;
- deje la ruta por defecto **del nodo** (la máquina, y los demás sandboxes) donde está;
- si el agente se desconecta, **pare el egress** (blackhole) en vez de caer en silencio al proxy del nodo;
- muera con la sesión, con el idle, o cuando el portátil deje de contestar;
- siga atribuido a `owner_sub`, no a un sujeto nuevo;
- no deje que el guest encienda, apague o estreche el flag.

Las excepciones estrechas («todo menos este prefijo») pueden estudiarse después. No son v1. v1 es todo o nada.

## Qué ganamos (diseño objetivo)

- **Un solo opt-in.** `asp session start --local-net` no pide prefijos. El sandbox usa la red que el portátil ya sabe enrutar: la LAN y el Internet público. Quien no pasa el flag no cambia de egress.
- **Sin hueco de entrada.** El portátil marca el nodo. Si el proceso no está, no hay mapping que heredar.
- **El DNS va con los datos.** No hay split DNS que resuelva con la vista del nodo y conecte con la vista de casa.
- **Fallo cerrado y visible.** Túnel aún no atado, o portátil dormido, o agente muerto → blackhole de la default de **esa** sesión. No se rearma el proxy `:8888` ni el sink «para que al menos GitHub responda». Un fallback al nodo no existe en v1; si algún día existe, tendrá flag propio y no será el comportamiento mudo de la desconexión.
- **La frontera no es el guest.** La policy route la instala el nodo por el TAP de esa sesión. El guest no tiene claves ni `NET_ADMIN`, y un `HTTP_PROXY` apuntando al proxy del nodo no recupera el egress de ADR-0002.
- **Misma sesión, mismo dueño.** El túnel no vive más que el sandbox. El grant lo pide el `owner_sub`. El JSON de `asp session` no se convierte en una clave. El tráfico del túnel no refresca `last_activity_at`.
- **Auditoría alineada con ADR-0008** en la identidad: `path=local_net`, `owner_sub`. El guest no rellena esos campos. El nodo registra el desvío; no es un espejo de todo lo que el NAT de casa ha visto.

```text
sin --local-net                         con --local-net y attach vivo
────────────────                        ──────────────────────────────
default → proxy y DNS sink del nodo     default (0.0.0.0/0, ::/0 si existe)
192.168.1.0/24 → no hay ruta especial      → túnel saliente → NAT del portátil
allowlist de ADR-0002 aplica            LAN, Internet público y DNS van juntos
                                        la allowlist del nodo no aplica
                                        no hay CIDR que configurar

                                        attach aún no, o portátil dormido
                                        → blackhole de esa default
                                        → no hay vuelta silenciosa al proxy
```

## Qué no ganamos (límites honestos)

- **El corte mínimo sí está:** flag, columna, handshake y plan de blackhole en el nodo. **No** hay peer WireGuard de kernel, ni NAT real en el agente local, ni un paquete de prueba. Ver el ADR, sección «Estado de implementación», y [`ops-local-net.md`](ops-local-net.md).
- **El portátil ve y hace NAT de todo el tráfico de la VM** mientras el túnel está `up`, no solo de un `/24`. Internet público incluido. Las queries DNS que no vayan cifradas se ven en esa máquina. La IP de origen hacia fuera es la del usuario (o la de la VPN que el usuario ya tenga), no la del nodo. Ese es el coste de no pedir CIDRs.
- Con el flag puesto, un guest comprometido **puede** hablar con cualquier destino unicast que el portátil alcance, mientras el túnel esté `up`. No hay filtro de puertos en v1. No es inocuo. Es exactamente lo que el opt-in compra.
- **Si el agente se desconecta, el egress para.** La sesión puede seguir `running` y no tener red. No sale a escondidas por el proxy del nodo. Esta propuesta **no** documenta un modo fallback: lo rechaza en silencio y tampoco lo ofrece con otro nombre. Añadir `--local-net-fallback=node` sería otro diseño, no una lectura amable de este.
- **No** es una VPN de sistema. La tabla de rutas del portátil no gana un `0.0.0.0/0`. El «todo» vive en el túnel de esa sesión (netns o TUN del agente). Los demás sandboxes del nodo siguen por ADR-0002.
- **No** entra tráfico de la LAN hacia el guest. No hay servicio publicado. No hay port forward.
- **No** hay descubrimiento L2. mDNS, SSDP y broadcast no aparecen solo porque la default haya cambiado de sitio.
- **No** mantiene la microVM despierta. Los bytes del túnel no refrescan `last_activity_at`.
- El control plane **no** reenvía el payload en v1. Si el portátil no puede marcar el node-agent, el estado se queda en `pending` y el egress sigue hundido.
- El guest **no** puede activar esto, ni desactivarlo, ni pasar una lista de excepciones en el exec.
- FakeVMM puede guardar el flag. Eso no demuestra un paquete en la LAN ni un NAT.
- Un admin del tenant no se attach a la casa de otro usuario.
- IPv6 solo acompaña a la default si esa default existe. No se promete pila IPv6 nueva.

Detalle normativo, API, fallos y la lista de lo que no hay que construir: el ADR-0010.
