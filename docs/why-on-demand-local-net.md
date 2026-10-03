# Por qué / Qué ganamos — Red local bajo demanda

Ver también ADR: [`adr/0010-on-demand-local-net.md`](adr/0010-on-demand-local-net.md) (**propuesta — no implementada**).  
El egress público no se toca: [`adr/0002-networking.md`](adr/0002-networking.md).  
La sesión que acota la vida del túnel: [`adr/0009-agent-sessions.md`](adr/0009-agent-sessions.md).  
La identidad del flujo sigue siendo `owner_sub`: [`adr/0008-network-flow-attribution.md`](adr/0008-network-flow-attribution.md), [`adr/0007-multi-user-identity.md`](adr/0007-multi-user-identity.md).

## Por qué

El sandbox está en un nodo remoto. Su ruta por defecto sale por el proxy y la allowlist de ese nodo. La LAN del usuario (`192.168.1.0/24` y similares) no es enrutable desde ahí. El agente no puede hablar con el NAS, el dev server o el puerto que solo existe en casa.

Las salidas fáciles abren un agujero **hacia dentro** de esa LAN (port forward, UPnP, un reverse tunnel dejado vivo) o mandan **todo** el tráfico del sandbox por el portátil (`0.0.0.0/0`). Las dos sobreviven mal al sueño del portátil, no saben de la sesión y le dan al guest más red de la que nadie pidió. Dejar que el propio guest elija la ruta tampoco sirve: no tiene `NET_ADMIN` y, si lo tuviera, no sería frontera.

Hace falta un camino que:

- nazca **apagado** (`local_net=false` salvo `asp session start --local-net`);
- lo abra el **agente local**, de dentro hacia fuera, sin publicar un puerto en el router;
- lleve solo prefijos y puertos declarados;
- deje la ruta por defecto en el egress del nodo;
- muera con la sesión, con el idle, o cuando el portátil deje de contestar;
- siga atribuido a `owner_sub`, no a un sujeto nuevo.

## Qué ganamos (diseño objetivo)

- **Alcance explícito.** Con el flag, el sandbox llega a `192.168.1.0/24` tcp/443 (el ejemplo) y no al resto de la casa ni a Internet por el NAT del usuario.
- **Sin hueco de entrada.** El portátil marca el nodo. Si el proceso no está, no hay mapping que heredar.
- **Separado del egress público.** Otro dispositivo o tabla. `curl https://github.com` sigue en el proxy. El NAS no se cuela en la allowlist de hostnames.
- **Fallo cerrado.** Túnel caído o portátil dormido → blackhole de esos CIDR. No caen a la default (donde podrían acertar otra `192.168.1.0/24` o salir al NAT del nodo con IP privada).
- **Misma sesión, mismo dueño.** El túnel no vive más que el sandbox. El grant lo pide el `owner_sub`. El JSON de `asp session` no se convierte en una clave.
- **Auditoría alineada con ADR-0008:** `path=local_net`, `owner_sub`, destino, puerto, allow/deny. El guest no rellena esos campos.

```text
sin --local-net                         con --local-net y attach vivo
────────────────                        ──────────────────────────────
default → proxy del nodo                default → proxy del nodo (igual)
192.168.1.0/24 → no hay ruta especial    192.168.1.0/24 tcp/443 → túnel saliente
                                         resto de ese /24 → drop
                                         portátil dormido → blackhole, no default
```

## Qué no ganamos (límites honestos)

- **Nada de esto está implementado.** No hay flag en `asp`, ni columna, ni peer WireGuard.
- Con el flag puesto, el guest **sí** puede hablar a esos `ip:port` mientras el túnel esté `up`. Ese es el riesgo que se acepta al optar. No es «toda la LAN» si el filtro de puertos se cumple; tampoco es inocuo.
- **No** es una VPN. `0.0.0.0/0` hacia casa no entra en v1.
- **No** entra tráfico de la LAN hacia el guest. No hay servicio publicado.
- **No** hay DNS de casa (`nas.home`), ni mDNS, ni UDP, ni IPv6 de la LAN.
- **No** mantiene la microVM despierta. Los bytes del túnel no refrescan `last_activity_at`.
- El control plane **no** reenvía el payload en v1. Si el portátil no puede marcar el node-agent, el estado se queda en `pending`.
- FakeVMM puede guardar el flag. Eso no demuestra un paquete en la LAN.
- Un admin del tenant no se attach a la casa de otro usuario.

Detalle normativo, API, fallos y la lista de lo que no hay que construir: el ADR-0010.
