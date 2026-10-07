# ADR-0010: Red local bajo demanda (túnel completo iniciado por el agente local)

- **Estado:** Contrato de esta revisión vigente. Los comandos `ip`/`wg` del dispositivo por sesión están cableados (2026-10-03) — ver [Estado de implementación](#estado-de-implementación). Un lab con paquetes no está demostrado.
- **Fecha:** 2026-10-03
- **Revisión:** 2026-10-03. La primera redacción (commit `1858dd2`) fijaba v1 como allowlist de CIDR **y** puertos, y prohibía instalar `0.0.0.0/0` y `::/0` hacia el portátil. **Esta revisión sustituye ese contrato.** v1 es todo o nada: con el flag, la ruta por defecto del sandbox sale por el agente local; sin el flag, no hay túnel. Pedir prefijos al usuario no es v1.
- **Relacionados:** [0002](0002-networking.md) (TAP + proxy + nft; egress del nodo cuando el flag está apagado), [0008](0008-network-flow-attribution.md) (flujo → `owner_sub`, evaluación), [0009](0009-agent-sessions.md) (la sesión es el objeto; egress de esa microVM), [0007](0007-multi-user-identity.md) (`owner_sub`), [`../why-on-demand-local-net.md`](../why-on-demand-local-net.md), [`../roadmap.md`](../roadmap.md)
- **No es:** una VPN de sistema en el portátil (no se toca la ruta por defecto de la máquina del usuario), un agujero de entrada en el router de casa, ni un split tunnel que el usuario tenga que rellenar con CIDRs. Con `local_net` apagado, el egress público de ADR-0002 no se mueve. Con `local_net` encendido, el default **de esa sesión** sí sale por el agente local, y el proxy del nodo deja de ser el camino.

## Contexto

El sandbox vive en un **nodo remoto** (lab o capacidad compartida). Su única red de producción hoy es la de ADR-0002: TAP `asp-{shortID}`, NAT del nodo, forward proxy `:8888`, DNS sink y nft `asp_egress`. La ruta por defecto sale por el **egress del nodo**. El guest no tiene `NET_ADMIN`. Eso está bien para git, registries y APIs públicas allowlisted **cuando el trabajo no depende de la red del usuario**.

No alcanza cuando el trabajo está en la **LAN del usuario** (un NAS, `192.168.1.50:8080`, un dev server que no está en Internet) o cuando el destino útil es «lo que sea que resuelva el DNS de casa», no un prefijo que el humano sepa escribir. Ese prefijo no es enrutable desde el nodo.

La redacción anterior pedía al caller una allowlist de `cidr + proto + ports` y dejaba la ruta por defecto en el nodo, rechazando `0.0.0.0/0`. Eso se descarta como contrato de v1:

- Casi nadie sabe el CIDR de su casa, ni el conjunto de IPs a las que un `git clone`, un `curl` o un agente van a hablar después de un redirect o de un DNS.
- Una allowlist corta se queda corta (el flujo «de la LAN» abre también Internet: OCSP, registries, APIs). Una allowlist larga acaba siendo `0.0.0.0/0` mal escrito.
- El DNS no cabe en «prefijos declarados». Si la resolución sigue en el sink del nodo y los paquetes de datos salen por casa —o al revés— el diseño miente.
- El coste de equivocarse lo paga el usuario en cada `session start`. v1 no puede exigir ese experto.

Las formas ingenuas de «arreglar la LAN» siguen siendo peores que un túnel de sesión:

1. **DNAT / UPnP / port forward en el router de casa** — un listener alcanzable desde el nodo (o desde Internet) dentro de la LAN. Sobrevive a la sesión, no sabe de `owner_sub`, y un sandbox olvidado sigue teniendo camino de vuelta.
2. **`ssh -R` o reverse tunnel dejado abierto** — el extremo de casa escucha, o el forward queda atado a un proceso que nadie asocia al ciclo `asp session`.
3. **VPN de sistema en el portátil** (`AllowedIPs = 0.0.0.0/0` en la tabla principal) — secuestra también el tráfico del humano, no el de una sesión. No es este diseño.
4. **Que el guest elija rutas** (`ip route`, marks, `HTTP_PROXY` hacia donde quiera) — el guest no es confiable (ADR-0002/0003/0007). Cualquier ruta que el workload pueda instalar no es frontera.
5. **Volver en silencio al proxy del nodo** si el portátil se duerme — el usuario cree que el tráfico sigue saliendo por su casa (o que no sale) y en realidad sale por el NAT y la allowlist del nodo, o al revés. Un fallback no declarado es una política distinta. **Rechazado.**

Restricciones que este ADR fija:

- El túnel es **saliente desde el agente local** (el proceso en la máquina del usuario, que ya está en esa LAN). Nadie abre un hueco de entrada hacia la LAN.
- Transporte candidato: **WireGuard** (dispositivo L3 real, en un netns o TUN de usuario, no en la tabla principal del portátil) **o** un TUN sobre un stream que **el agente local inicia**. El nodo no llama a un puerto del router de casa.
- **v1 no tiene lista de CIDR ni de puertos.** El opt-in mueve la ruta por defecto del sandbox: `0.0.0.0/0` y, si esa sesión tiene default IPv6, `::/0`. Todo el unicast que habría usado esa default —LAN, Internet público y DNS— pasa por el túnel.
- **Excepciones estrechas** (devolver un prefijo al proxy del nodo, o dropear un destino) pueden existir más adelante. No son configurables en v1. v1 es **todo o nada**.
- Camino **separado** de la ruta por defecto **del nodo** (la máquina). Policy routing por sandbox. No se reutiliza el forward proxy HTTP como datapath de esta sesión mientras el flag está encendido.
- Si el agente local no está, o deja de contestar, el egress de **esa** sesión se **hunde** (blackhole). No cae al proxy ni al DNS sink. No hay fallback silencioso. Un fallback al nodo solo podría existir en otro corte, con flag propio y documentación propia; esta revisión no lo define y prohíbe el implícito.
- **Solo bajo demanda.** Default apagado: `local_net=false` hasta que el caller opta (`asp session start --local-net` o `"local_net": true` en `POST /v1/sandboxes`). La identidad del tráfico sigue siendo `owner_sub`. No hay un sujeto nuevo. El guest no puede encender, apagar ni estrechar esto.

Hoy no existe un agente local de datos. `asp` es un cliente HTTPS del control plane. El fichero `~/.cache/asp/sessions/<nombre>.json` guarda id y URL, **sin** token (ADR-0009). Los puertos vsock 26500/26501/26502 son host↔guest **dentro del nodo**; no cruzan la WAN y no deben terminar un túnel hacia la LAN del usuario. `Node.agent_endpoint` es cómo el CP habla con el node-agent, no un derecho del guest a pivotar.

## Decisión

**La red local es una capacidad de la sesión, apagada por defecto, que el plano de control enciende solo si el caller lo pide.** Cuando está encendida, el **agente local** abre un túnel **saliente** hacia el node-agent y el node-agent desvía la **ruta por defecto de ese sandbox** por ese túnel. No hay policy de prefijos que persistir. El guest ve su TAP de siempre y no recibe claves, tablas ni `NET_ADMIN`.

Con el túnel **up**, el portátil **ve y hace NAT** de todo ese tráfico: la LAN de casa, el Internet público y el DNS. La allowlist de hostnames y el DNS sink de ADR-0002 **no aplican** a esa sesión mientras el túnel está up: el choke point pasa a ser la máquina del usuario.

Si el túnel no está (`pending` o `withdrawn`) y el flag sigue true, la default de esa sesión está en **blackhole**. No se reinstala el proxy `:8888` ni el sink. Cuando la sesión termina, el idle reap corre, o el agente local hace detach, el nodo **destruye** el túnel. No queda listener en casa.

### Forma concreta de API y CLI (contrato; sin código en este cambio)

`POST /v1/sandboxes` gana un booleano. Ausente o false = comportamiento de hoy (egress del nodo, ADR-0002).

```json
{
  "tenant_id": "default",
  "image_ref": "debian:bookworm-slim",
  "cpu_millis": 1000,
  "memory_mib": 512,
  "node_id": "dev-node",
  "local_net": false
}
```

Opt-in. No hay objeto policy en v1:

```json
{
  "local_net": true
}
```

Reglas de validación del CP (fail closed):

| Entrada | Resultado |
|---|---|
| Campo omitido o `"local_net": false` sin más campos de red local | Sesión normal. Cero túnel, default del nodo, proxy y DNS sink como hoy |
| `local_net: true` **sin** listas de rutas | Aceptado. Desired state = túnel completo de la default de esa sesión. `local_net_state=pending` hasta el attach |
| Cualquier `local_net_policy`, `prefixes`, `cidrs`, `ports`, `routes`, `exceptions` o equivalente, con el flag true **o** false | **400**. v1 no acepta una allowlist a medias ni una excepción. No se recorta en silencio a «todo» ni se ignora el campo |
| `local_net` con tipo distinto de bool | **400** |
| `owner_sub` dentro del body como autoridad | Igual que ADR-0007: el sujeto sale del JWT. Un campo de cliente no elige el dueño ni enciende el túnel |

CLI (misma semántica; el binario sigue sin ser un plugin):

```text
asp session start --node-id=dev-node
  # no envía local_net (o envía false). Default de hoy: egress del nodo

asp session start --local-net
  # único opt-in de v1. Equivale a {"local_net": true}
  # no pide CIDR, puertos ni DNS

asp session start --local-net --local-net-allow 192.168.1.0/24
  # 400. --local-net-allow no existe en v1. No es azúcar y no es un opt-in parcial
```

No hay `--local-net-allow`, `--local-net-cidr` ni `--local-net-default-route`. El flag `--local-net` **es** la ruta por defecto. No hace falta un segundo flag para «de verdad todo»: todo es lo único que v1 sabe hacer.

El agente local, **después** de que la sesión está `running` y `local_net=true`, se ata con un grant de corta vida (no con el JSON de sesión):

```text
POST /v1/sandboxes/{id}/local-net/grant
  Authorization: Bearer (mismo IdP; el CP exige sub == owner_sub de ese sandbox)
  → { "grant": "<opaco, TTL minutos>", "dial": "<url node-agent o rendezvous>", "expires_at": "..." }

# el proceso local abre el túnel saliente usando el grant
POST /v1/sandboxes/{id}/local-net/heartbeat   # o el propio keepalive del transporte
DELETE /v1/sandboxes/{id}/local-net/attach    # cierre ordenado (sleep, stop del agente)
```

Forma de CLI equivalente, no implementada: `asp local-net attach --name NOMBRE`. Tiene que resolver el Bearer por `asp auth`, no leer un secreto del fichero de sesión. `asp session stop` (y el reaper) piden el detach aunque el attach no haya existido. Attach sobre una sesión con `local_net=false` es **409**: no se enciende el túnel a posteriori sin haberlo pedido en el create. Cambiar el flag a mitad de vida no es v1 (habría que parar y volver a crear).

`GET /v1/sandboxes/{id}` expone estado para que el usuario vea la verdad:

```json
{
  "local_net": true,
  "local_net_state": "pending"
}
```

No hay `local_net_policy` en la respuesta de v1. Inventar una lista vacía de prefijos confundiría con la allowlist descartada.

`local_net_state`: `off` | `pending` | `up` | `withdrawn`.

- `off` — flag apagado. Egress del nodo. No hay túnel.
- `pending` — opt-in aceptado, túnel aún no establecido. La default de esa sesión está en blackhole. El proxy `:8888` y el DNS sink **no** reciben ese tráfico.
- `up` — agente local autenticado y keepalive vivo. `0.0.0.0/0` (y `::/0` si existe) de esa sesión apuntan al túnel. El portátil hace NAT.
- `withdrawn` — estaba pedido pero el túnel cayó (sueño, red, detach) o la sesión está muriendo. Ruta útil fuera. Blackhole de la default mientras la sesión siga viva y el flag siga true. **No** se restaura el proxy del nodo. Al pasar el sandbox a `stopping`/`stopped` se borra también el blackhole, porque ya no hay guest.

El arranque de la sesión **no** espera al túnel. El sandbox puede estar `running` con `local_net_state=pending` y **sin egress** (ni LAN ni Internet ni DNS) hasta el attach. Quien necesite fallo duro puede, en un corte posterior, pasar `--local-net-required`; **no** es v1. v1: la VM arranca igual y los dials fallan cerrados hasta que el attach existe. Eso es preferible a un arranque que ya salga por el nodo «mientras tanto».

### Qué se mueve, y qué no, cuando el flag está on

Se mueve la **ruta por defecto del sandbox**, no la del nodo y no la del portátil como sistema:

- En el nodo, policy routing (tabla aparte, seleccionada por el TAP o la IP de **ese** sandbox): `default` → dispositivo del túnel. Si el guest tiene (o el nodo le instala) una default IPv6, `::/0` va al mismo sitio. Dejar `::/0` en el nodo con `0.0.0.0/0` en el túnel sería un agujero de fuga, no un «IPv6 aún no».
- Si esa sesión **no** tiene default IPv6, v1 no inventa una pila IPv6. El hueco de IPv6 del nodo (ADR-0002) sigue abierto. La obligación es no dejar un default v6 huérfano en el proxy.
- La ruta **más específica** de la subred del TAP (el enlace guest↔host, p.ej. `10.200.0.0/24`) se queda en el nodo. Hace falta para no mandar al portátil el tráfico de control que vive en esa subred. No es una allowlist que el usuario configure.
- **No** se inyecta `HTTP_PROXY` / `HTTPS_PROXY` hacia `10.200.0.1:8888` en un sandbox con `local_net=true`. Un proxy explícito a la IP del host sería un camino más específico que la default y se saltaría el túnel.
- nft `asp_egress` **no** redirige el TAP de esa sesión al proxy ni al DNS sink mientras `local_net=true` (ni en `pending`, ni en `up`, ni en `withdrawn`). Esos redirects son el fallback silencioso que este ADR prohíbe. Si la imagen trae el proxy a fuego, el nodo **rechaza** conexiones de ese TAP al `:8888` y al sink en lugar de servirlas: fallo cerrado, no egress por el nodo.
- DNS (UDP/TCP 53 y el resolutor que el guest use hacia fuera de la subred TAP) viaja por la misma default. Con túnel `up`, resuelve el DNS que vea el portátil tras su NAT, no el sink. Con túnel caído, la query se hunde con el resto. No hay split DNS en v1.
- Multicast, broadcast, mDNS, SSDP y link-local **no** quedan resueltos por mover `0.0.0.0/0`. v1 no añade routing multicast ni un puente L2. Una impresora que solo existe por mDNS sigue sin «aparecer». El unicast sí (incluida la LAN, si el portátil tiene esa ruta).

En el portátil el túnel vive en un **netns o en un TUN que lee el agente**, no como `AllowedIPs = 0.0.0.0/0` de la tabla principal. Si el cryptokey routing de WireGuard se aplicara a la tabla del sistema, el diseño habría instalado una VPN de sistema: **prohibido**. El agente hace NAT (MASQUERADE) del tráfico que sale del túnel hacia el uplink que el portátil ya usa. Por eso el portátil ve cada flujo.

### Modelo de datos (previsto)

Sin migración en este cambio. Cuando se implemente, campos en `sandboxes` (siguiente migración después de `007`; el número exacto lo fija el corte de código):

| Campo | Tipo | Notas |
|---|---|---|
| `local_net` | bool NOT NULL default `false` | El default de SQL y el de la API coinciden. Un store viejo que no mande el campo sigue en false |
| `local_net_state` | text NOT NULL default `off` | `off` / `pending` / `up` / `withdrawn` |
| `local_net_attached_at` | timestamptz NULL | Último paso a `up` |
| `local_net_grant_expires_at` | timestamptz NULL | Tope del grant vigente; no es un secreto |

No hay columna `local_net_policy` en v1. Reservarla «por si acaso» invita a rellenar CIDRs. Las excepciones futuras, si llegan, son otra migración y otro contrato, no un jsonb vacío que el cliente pueda mandar hoy.

El grant en claro **no** se guarda en Postgres ni en `sessions/<nombre>.json`. Solo hash o id si hace falta revocar. El JSON de sesión puede anotar `local_net: true` como recordatorio de UX; no es autoridad y no lleva clave WireGuard.

**Actualizado 2026-10 (asignación en el nodo):** la tabla de policy routing, el puerto UDP del dispositivo y la /30 del túnel salían de un hash FNV del id corto de 8 caracteres, sin detectar colisiones. Dos sesiones con la misma tabla mandaban el tráfico de una al túnel de la otra, es decir, al portátil de otro usuario (50 % de probabilidad con unas 170 sesiones por nodo). Ahora los asigna el node-agent (`localnet.Allocator`): tabla en `10000`–`29999`, puerto en `47000`–`54999`, /30 en `10.188.0.0/16`. Empieza por el valor del hash (así suele coincidir con lo que calculaban las versiones anteriores), salta lo que ya tiene otra sesión y lo que el host usa (`ip rule show`, `ss -lun`), y lo guarda en `<id>.alloc` junto a la clave del nodo, para que un agente reiniciado y el reaper lo encuentren y lo liberen. El nodo publica puerto y direcciones con su clave pública (`POST …/local-net/node-public` con `listen_port`, `node_tunnel_addr`, `client_tunnel_addr`); el control plane los guarda (`local_net_listen_port`, `local_net_node_addr`, `local_net_client_addr`, migración `017`) y el grant devuelve exactamente eso. Antes de que el nodo publique, el grant los trae vacíos y el CLI dice qué falta. Los nombres de dispositivo siguen saliendo del id corto: si dos sandboxes del mismo nodo comparten id corto, los nombres son de la primera y el plan de la segunda se rechaza (su arranque falla) en vez de tomar el túnel de la otra.

Eventos (`sandbox_events`, `actor_sub` = quien llamó):

- `sandbox.local_net_requested` — create con `local_net=true` (sin lista de destinos; no hay policy)
- `sandbox.local_net_up` / `sandbox.local_net_withdrawn` — transiciones que reporta el node-agent
- `sandbox.local_net_denied` — 400 (cliente mandó prefijos, o intentó encenderlo sin ser el caller del create). Opcional; el 400 HTTP ya es la respuesta

La auditoría de **flujos** no es una fila por paquete en `sandbox_events`. Es el mismo sitio que ADR-0008 reserva al proxy: log estructurado en el nodo, con `sandbox_id`, `owner_sub`, `tenant_id`, `dst`, `proto`, `port`, `action=forward|deny`, `path=local_net`. Con el flag apagado el path del egress público no cambia. Con el flag encendido el nodo ve el desvío (y puede loguear el blackhole), pero **el byte de Internet lo ve el portátil**: el log del nodo no es una copia completa de lo que el NAT de casa observó. Puede existir antes de que 0008 esté implementado; la clave de identidad es la misma. El guest no rellena esos campos.

No hay tope `ASP_LOCAL_NET_ALLOW_CIDRS` ni `ASP_LOCAL_NET_MAX_PREFIXES`. No hay nada que topar. Quien puede crear el sandbox puede pedir el flag. Un kill-switch de operador, si algún día hace falta en un nodo corporativo que no quiera ceder el egress, es configuración de plataforma con nombre propio; no se cuela como allowlist de CIDR y no es requisito de esta propuesta.

### Responsabilidades

```text
caller (asp session start --local-net)          agente local (máquina del usuario)
        │ Bearer IdP                                      │ mismo owner_sub, grant de corta vida
        ▼                                                 │ dial SALIENTE
control plane ──desired state (bool, no CIDRs)──────────► node-agent
        │                                                 │ tabla APARTE, solo esa sesión
        │ owner_sub sellado en el create                  │ blackhole de la default si no hay túnel
        ▼                                                 ▼
     sandbox row                                    guest (TAP de siempre)
                                                    sin NET_ADMIN, sin claves, sin ruta elegida
```

**Control plane**

- Única autoridad de `local_net`. Lo sella en el create junto a `owner_sub`.
- Rechaza los casos de la tabla de arriba. No interpreta una lista de CIDR como «quiso decir todo».
- Emite el grant solo si el Bearer pertenece al `owner_sub` de **ese** sandbox (o a un rol admin ya definido en ADR-0007; el admin no hereda la LAN de otro usuario: un admin puede apagar, no attacharse a la casa de otro). v1: **solo el `owner_sub`** puede hacer grant/attach.
- Al `DELETE`, al idle reap (`stop_reason=idle_timeout`) y al fallo del sandbox, el desired state pasa a túnel muerto. No espera a que el portátil coopere para dejar de enrutar.
- No trata bytes del túnel como actividad del reaper. `last_activity_at` sigue siendo create, paso a `running`, exec bien proxyado y stdin bien proxyado (ADR-0009). Un túnel callado no mantiene la VM viva. Tampoco la mantiene el hecho de que el portátil esté haciendo NAT de un `curl` largo: el tráfico de red **no** refresca el idle. (Es deliberado y fácil de discutir; cambiarlo sería otro corte. v1 no lo hace, para no dejar una VM viva para siempre por un scan o un keepalive.)
- No mete el flag por `X-ASP-*` ni por el body de exec. El guest no lo enciende.

**Node-agent**

- Lee el booleano del work item. Si `local_net` es false, no crea dispositivo ni regla nueva: proxy, sink y nft de ADR-0002 como hoy.
- Crea un netdev **por sandbox** (v1): `wg-asp-{short}` / `tun-asp-{short}`. La tabla de rutas de esa sesión no es la default del nodo.
- Con `local_net=true` y estado distinto de `up`, instala **blackhole** de `0.0.0.0/0` (y de `::/0` si esa sesión tiene default v6) en la tabla de esa sesión. No programa `asp_nat` ni el redirect a `:8888` para ese TAP.
- Con estado `up`, sustituye el blackhole por la default vía el túnel. No instala una lista de CIDR de usuario.
- Rechaza en el filtro del host el intento de ese TAP de hablar con el proxy o el sink del nodo, para que un `HTTP_PROXY` hardcoded no sea un segundo egress.
- Acepta el túnel solo con el grant vigente ligado a ese `sandbox_id`. Un grant de otro sandbox no instala la default aquí.
- Keepalive (orden de decenas de segundos, valor concreto en la implementación). N fallos → `withdrawn`, borra la ruta útil, **reinstala blackhole**, no restaura el proxy, evento al CP.
- Al Stop de la microVM, destruye dispositivo, rutas, blackhole y estado de peer. No reutiliza la clave en el sandbox siguiente.
- Audita el desvío con `owner_sub` que le pasó el CP (cache de ADR-0008). No lee identidad del paquete interno. No pretende haber visto el payload que el portátil ya hizo NAT.
- Sigue sin dar `NET_ADMIN` al guest. El TAP del guest no es el dispositivo del túnel.

**Guest**

- Habla IP por su TAP como hoy. La clasificación es del host, por interfaz de entrada o por IP origen de **esa** sesión, no por la tabla que el guest crea tener.
- No ve claves, no pide `local_net` en el exec, no puede apagar el túnel ni «volver al proxy» borrando una ruta. Sin `NET_ADMIN` no instala nada; si pudiera, la policy route del host gana igual.
- Con túnel caído, un dial a Internet **y** un dial a `192.168.1.20` fallan igual (blackhole). No hay camino bueno que sobreviva a medias.
- Con túnel up, ese mismo dial sale por el portátil, con la IP de casa, sin pasar por la allowlist del nodo.

**Agente local**

- Proceso en la máquina que **ya** tiene la ruta hacia la LAN y hacia el uplink de Internet. No es el node-agent ni el pod-daemon.
- Inicia el transporte hacia fuera. No abre un socket de escucha en interfaces de la LAN, no configura UPnP, no pide port forward.
- Hace NAT de **todo** lo que entrega el túnel. No filtra por CIDR en v1 (no hay lista que aplicar). Defensa en profundidad no es «inventar una allowlist que el usuario no pidió».
- No hace bridge de `eth0`/`wlan0` y no instala una VPN de sistema.
- Cierra el túnel al dormir, al apagar, al `session stop` y al perder el grant. No rearranca el túnel para **otras** sesiones ni para otro `owner_sub`.
- No escribe la clave privada en el JSON de sesión ni en el repo.
- Es un observador de facto de ese tráfico (DNS incluido). Quien enciende `--local-net` acepta eso. El proceso no tiene por qué ser un MITM TLS; el NAT ya revela destinos IP, puertos y queries DNS que no vayan cifradas.

### Transporte (decisión de diseño; WireGuard implementado)

**Parámetros por sesión (2026-10):** puerto UDP del dispositivo y direcciones de la /30 los asigna el nodo y los publica con su clave; el grant los entrega al portátil (ver «Modelo de datos»). Ya no hay funciones de hash en el control plane.

**v1 preferido: WireGuard en un dispositivo por sandbox, con el handshake iniciado por el agente local, dentro de un netns o de un TUN de usuario.**

El nodo escucha WireGuard solo en un endpoint que el agente puede marcar (UDP reachable del node-agent, o TCP/TLS de fuera que encapsula el UDP de WG si el camino de red del lab no deja UDP). La primera paquetización sale del portátil, así que el NAT de casa no necesita un mapping permanente ni un puerto publicado.

Cuidado con `AllowedIPs`. En el **nodo**, poner `0.0.0.0/0` en el peer del portátil robaría la default de la máquina entera por cryptokey routing. La default del sandbox se instala con policy routing en la tabla de esa sesión; el `AllowedIPs` del peer en el nodo es la IP de túnel (y lo justo para que el retorno de ese peer sea válido), no la default del host. En el **portátil**, `0.0.0.0/0` solo es legal dentro del netns del agente, donde «todo» significa «todo lo que el nodo metió en este túnel», no «todo lo que hace el usuario en el navegador».

Por qué WG y no el primer día un protocolo propio: hace falta un L3 que policy routing y nft ya saben tratar, con claves de sesión y roaming corto si el portátil cambia de IP. El agente local puede usar wireguard-go; no se exige el módulo del kernel en el guest (el guest no participa).

**Alternativa válida si WG no entra en el nodo:** un stream TCP/TLS que el agente local abre hacia el `agent_endpoint` (mTLS de nodo o grant como bearer de ese dial) y que lleva tramas L3 hacia un TUN. Es el mismo patrón mental que el CONNECT de vsock del nodo (framing sobre un canal ya autenticado), **no** es reutilizar el puerto 26500. 26500 sigue siendo exec host→guest. Meter el túnel dentro de ese vsock pondría al guest a terminarlo: **rechazado**.

Si el portátil no puede marcar el node-agent (nodo solo visible para el CP), el rendezvous es: ambos dial salientes a un sitio que el CP nombra. **El payload no se proxya por el proceso del control plane en v1.** Si no hay camino de datos directo, `local_net_state` se queda en `pending` (blackhole) y se documenta el lab. Un relay de datos en el CP es otro ADR (coste, secreto de tránsito, el CP vería todo el egress del usuario).

### Flujos

#### A) Opt-in feliz

```text
asp session start --local-net
  → POST /v1/sandboxes  local_net=true
  → CP sella owner_sub, state=pending, evento requested
  → reconciler arranca la VM
  → node-agent: tabla de ESA sesión con blackhole de 0.0.0.0/0 (y ::/0 si aplica)
  → sin HTTP_PROXY al :8888, sin redirect nft de ese TAP al proxy ni al sink
  → asp local-net attach
       grant (sub == owner_sub) → dial saliente → túnel up
  → node-agent sustituye el blackhole por default vía wg-asp-{short}
  → state=up
guest: curl https://192.168.1.20/   → TAP → default de la sesión → túnel → NAT del portátil → LAN
guest: curl https://github.com/     → el mismo camino → NAT del portátil → Internet
guest: resolución DNS               → el mismo camino, no el sink del nodo
       (la allowlist de ADR-0002 no interviene)
```

#### B) Sin flag (el caso común)

```text
asp session start
  → local_net false, state off
  → no grant, no dispositivo, no blackhole nuevo
  → default del nodo, proxy :8888, DNS sink, allowlist ADR-0002
  → el guest que marque 192.168.1.20 sale por esa default y muere
     en la allowlist / en un destino no enrutable
  → no hay proceso en el portátil para esta sesión
```

#### C) El portátil duerme, o el agente muere

```text
keepalive pierde N ventanas, o DELETE /local-net/attach
  → node-agent: state=withdrawn, evento local_net_withdrawn
  → borra la default vía túnel, reinstala blackhole de 0.0.0.0/0 (y ::/0 si aplica)
  → NO rearma proxy, NAT del nodo, ni DNS sink para ese TAP
  → el sandbox puede seguir running (el reaper de idle es otro reloj)
guest dial a 192.168.1.20 o a github.com o a 1.1.1.1:53 → unreachable
el portátil despierta y attach de nuevo, mismo sandbox, grant nuevo
  → state=up, la default vuelve al túnel
```

No existe un modo «si se cae, sigue por el nodo» en v1. Documentarlo como comportamiento implícito sería el fallback silencioso rechazado abajo. Un modo explícito de fallback tendría que ser otro flag, otra fila de esta tabla y otra frase en el why; no se añade aquí.

#### D) Fin de sesión

```text
asp session stop | DELETE | idle reap
  → desired state: túnel muerto
  → node-agent destruye dev, peer, rutas, blackhole, filtros de esa sesión
  → un heartbeat tardío del agente local recibe 409/404 y cierra
  → no queda peer, ni NAT de sesión, ni DNAT hacia la LAN
  → la VM ya no está: no hace falta «devolverla» al proxy
```

#### E) Guest hostil con la flag puesta

```text
el workload escanea la LAN y habla con Internet
  → es el riesgo comprado: con el túnel up, el unicast no está filtrado por puerto
  → audit path=local_net owner_sub=... en el nodo (desvío); el portátil ve el resto
el workload intenta `ip route add default via 10.200.0.1` o un mark
  → sin NET_ADMIN no instala nada
  → si pudiera, la policy route del host clasifica por el TAP, no por la tabla del guest
el workload manda X-ASP-Local-Net: false o un CIDR en el exec
  → el CP no lee el flag ni rutas del exec; el nodo ignora el header
el workload apunta HTTP_PROXY al proxy del nodo
  → el nodo no acepta ese flujo desde este TAP; falla, no sale por ADR-0002
```

#### F) Guest hostil con la flag apagada

```text
el workload no puede abrir el túnel: no hay grant que el guest pueda pedir,
el create ya selló local_net=false, y el exec no tiene campo para cambiarlo
```

## Alternativas consideradas

| Alternativa | Pros | Contras | Decisión |
|---|---|---|---|
| **Allowlist CIDR+puertos, default del nodo intacta** (redacción `1858dd2`) | Blast radius pequeño; la allowlist del nodo sigue filtrando Internet; un error de túnel no mata el egress público | El usuario no sabe qué CIDR escribir; DNS, redirects y CDNs no caben; la lista acaba vacía o equivalente a `0.0.0.0/0` sin decirlo | **Rechazada como v1.** Puede volver como excepciones estrechas, no como el contrato que hay que rellenar para encender la opción |
| **Túnel completo de la default, solo con `--local-net`** | Un flag. LAN e Internet «de esa sesión» funcionan. El DNS va con los datos. No hay formulario de prefijos | El portátil ve y hace NAT de todo, incluida la Internet pública; se salta el proxy y la allowlist; si el agente cae, **no hay egress** | **Elegida para v1** |
| **Inbound a la LAN** (DNAT, UPnP, `ssh -R` en el router) | El nodo «llega» sin proceso en el portátil | Hueco de entrada, sobrevive a la sesión | **Rechazada** |
| **VPN de sistema en el portátil** | Cualquier proceso local, no solo el sandbox, usa el túnel | Secuestra el tráfico del humano; no está acotada a la sesión | **Rechazada** |
| **Fallback silencioso al proxy del nodo** cuando el túnel cae | El `curl` a GitHub no se rompe al cerrar el portátil | El usuario no pidió egress por el nodo en esa sesión; mezcla dos políticas; un atacante que tire el agente recupera el camino corporativo o el NAT del lab | **Rechazada.** Ni siquiera como default «temporal» de `pending` |
| **Fallback documentado y opt-in** (`--local-net-fallback=node` o similar) | Alguien que lo quiera lo pide con nombre | Sigue mezclando políticas; hay que definir DNS, allowlist y auditoría del modo mixto | **Fuera de v1.** Solo existiría con flag propio. Esta revisión no lo especifica y prohíbe que una implementación lo añada sin decirlo |
| **Excepciones estrechas** (un prefijo se queda en el nodo, o se dropea) | Permite «todo menos la RFC1918 del laboratorio» o «el DNS corporativo sigue en el sink» | Vuelve el problema de los CIDR, ahora como lista de exclusiones | **Fuera de v1.** v1 no acepta el campo. Un corte posterior puede añadirlas sin convertirlas en el camino feliz |
| **Rutas elegidas por el guest** | Cero trabajo en el nodo | Forgeable; contradice ADR-0002 y el espíritu de ADR-0008 | **Rechazada** |
| **Reutilizar el forward proxy HTTP** para llevar la LAN | Un solo choke point | Los destinos de LAN no son CONNECT/HTTPS corporativo; con túnel completo el proxy estorba | **Rechazada** como datapath cuando el flag está on. Sigue siendo el datapath cuando el flag está off |
| **Split DNS** (datos por el túnel, resolutor en el nodo) | El sink sigue viendo nombres | Resuelve con una vista que no es la de casa; filtra nombres que el usuario no pidió filtrar; es otro fallback a medias | **Rechazada en v1.** El DNS va con la default |
| **Terminar el túnel en el guest** (WG o vsock 26500 dentro de la microVM) | El nodo no enruta | Claves y rutas en el workload; el guest apaga el túnel; 26500 es exec | **Rechazada** |
| **Un túnel de nodo para todas las sesiones del mismo `owner_sub`** | Menos handshakes | Un sandbox olvidado y uno nuevo comparten peer; el teardown de uno rompe al otro | **Rechazada en v1.** Un túnel por sandbox |
| **Relay de payload en el control plane** | Funciona aunque el nodo sea inalcanzable desde casa | El CP ve todo el egress, coste, no es su trabajo | **Fuera de v1** |
| **Solo TCP tunelizado a mano, sin WG** | Menos dependencia | Se reinventa crypto, roaming y un L3 que nft ya entiende | **Reserva** si WG no es operable. Misma política (default entera), otro encapsulado |
| **mDNS / puente L2 / «descubre mi LAN»** | UX de impresoras | Refleja tráfico L2; no sale de mover la default | **Fuera de v1** |
| **Encender `local_net` por defecto si el cliente es un humano** | Menos flags | Contradice el mandato on-demand; un agente autónomo sacaría todo su egress por la casa de alguien | **Rechazada** |

### Qué no funciona (explícito)

1. **Publicar un puerto en casa** y llamar a eso «túnel saliente». Si el mapping existe sin un proceso que lo cierre al `stop`, no cumple este ADR.
2. **Quitar el blackhole cuando el túnel cae** «para que Internet al menos siga por el nodo». Eso es el fallback silencioso. Restaurar `asp_egress` o el sink en `withdrawn` o en `pending` incumple este ADR.
3. **Confiar en el fichero de sesión como capability del túnel.** No lleva token hoy y no debe llevar la clave WG mañana.
4. **Dejar que un admin de tenant se attach a la LAN del `owner_sub`.** Puede destruir la sesión; no hereda el grant.
5. **Contar paquetes del túnel como `last_activity_at`.** Un scan o un keepalive impedirían el idle reap para siempre.
6. **SoftFail del estilo nft:** si el nodo no puede instalar la tabla o el blackhole, la feature no «sigue igual» por el proxy. El sandbox con `local_net=true` que no pueda hundir la default debe fallar el Start (estado `failed`, log claro). Nunca «rutas a medias hacia el proxy».
7. **Aceptar `prefixes: []` como sinónimo de túnel completo.** El cliente que manda una policy, aunque vacía, recibe 400. El único opt-in es el booleano.
8. **Poner `AllowedIPs = 0.0.0.0/0` en la tabla de rutas principal del portátil o del nodo.** El «todo» es la tabla de esa sesión y el netns del agente, no el sistema operativo del humano ni el egress de los demás sandboxes.

## Consecuencias

### Positivas (si se implementa)

- Un flag. El sandbox remoto alcanza la LAN del usuario **y** cualquier destino unicast que el portátil ya sepa enrutar, sin que nadie escriba un CIDR.
- El DNS va con el resto del tráfico. No hay una segunda política escondida en el sink.
- El caso normal (`asp session start` sin flag) no cambia de amenaza: no hay túnel, no hay proceso local, no hay ruta nueva. Sigue ADR-0002.
- La frontera sigue fuera del guest. Encaja con ADR-0007 (`owner_sub`) y ADR-0009 (la sesión es quien vive y quien muere). ADR-0002 sigue describiendo el egress **cuando el flag está apagado**, y el nodo **cuando el flag está encendido** deja de aplicar esa allowlist a esa sesión — a propósito.
- Al dormir el portátil se cae el egress de esa sesión en segundos (blackhole), no al cabo del idle de la VM (lab: 2h), y no se redirige a escondidas al nodo.
- La auditoría puede usar la misma clave humana que ADR-0008, con `path=local_net`.

### Negativas / coste (hay que decirlas así)

- **El portátil ve y hace NAT de todo el tráfico de la VM**, no solo de la LAN. Eso incluye Internet público, SNI si el agente o el sistema lo miran, direcciones IP de destino, puertos y **DNS** (las queries que no vayan cifradas se leen en claro en la máquina del usuario). La IP de origen hacia el mundo es la de casa o la de la VPN que el humano ya tenga, no la del nodo. Quien comparte ese portátil comparte ese hecho.
- **La allowlist de ADR-0002 no protege esa sesión** mientras el túnel está `up`. Un guest comprometido habla con quien el portátil pueda hablar: la LAN entera alcanzable por unicast y todo Internet. No hay filtro de puertos en v1. Ese es el riesgo que se compra al optar. No es un «/24 y el tcp/443».
- **Si el agente se desconecta, el egress para.** Blackhole de la default. No hay navegación residual por el proxy del nodo. Una sesión `running` con `local_net_state=withdrawn` está viva y sorda. Hay que volver a hacer attach o parar la sesión.
- El nodo y el portátil pueden discrepar de lo que «se auditó». El log del nodo atribuye el desvío a `owner_sub`; no es un duplicado del NAT de casa.
- Hay un dataplane nuevo (WG o TUN) en el nodo, con keepalive, blackhole y teardown. Más formas de dejar una ruta colgada si Stop falla a medias: el Stop tiene que ser idempotente y no puede «dejar el proxy puesto por si acaso».
- Mientras `pending`, la VM no tiene red útil. Quien espere clonar un repo en el primer segundo del `start`, antes del attach, se encuentra un fallo de red, no el proxy.
- WireGuard (o el TUN de reserva) es otra dependencia operativa. No se implementan los dos encapsulados en el primer corte.
- El agente local es software nuevo en la máquina del usuario. `asp` hoy no lo es. Tiene que hacer NAT sin convertirse en la VPN del sistema.
- Dos sandboxes del mismo usuario son dos túneles y dos NATs. No se comparten.

### Límites honestos / no-goals

- El **contrato** de arriba es la decisión. El flag, el handshake y los comandos del dispositivo están anotados en «Estado de implementación». Un peer con tráfico real **no** está demostrado.
- **v1 no** pide ni acepta CIDR, puertos ni excepciones. Todo o nada.
- **v1 no** publica servicios del guest hacia la LAN (nada de DNAT inverso, nada de «entra a mi sandbox desde el NAS»).
- **v1 no** hace puente L2, mDNS, SSDP ni LLMNR. Mover la default no descubre vecinos.
- **v1 no** ofrece fallback al proxy del nodo, ni silencioso ni con flag. El silencioso está rechazado; el explícito no está diseñado.
- **v1 no** comparte túnel entre sandboxes ni entre usuarios de un mismo portátil.
- **v1 no** relaya bytes por el control plane.
- **v1 no** mantiene la VM despierta porque el túnel tenga tráfico.
- **v1 no** deja que el guest encienda, apague o estreche `local_net`.
- **v1 no** cambia la ruta por defecto del nodo ni la del sistema operativo del portátil.
- **v1 no** exige ADR-0008 mergeado, pero no inventa otra identidad: el campo es `owner_sub`.
- **v1 no** demuestra el túnel en CI sin red de verdad. FakeVMM puede persistir el flag y el estado `pending`; eso no prueba forwarding ni NAT.
- IPv6 solo se desvía si esa sesión tiene default `::/0`. No se añade pila IPv6 nueva en este corte (gap ya abierto en ADR-0002). Si el default v6 existe, no puede quedarse en el nodo.

## Qué no construir en v1

Lista cerrada para el primer corte, cuando exista. Si una de estas entra en el PR inicial, el corte se ha salido del ADR.

1. Allowlist de CIDR o de puertos, flags `--local-net-allow`, o un body `local_net_policy` aceptado con 200.
2. VPN de sistema en el portátil, o `AllowedIPs = 0.0.0.0/0` en la tabla principal del nodo o del portátil.
3. Listener en la LAN, UPnP, PCP, NAT-PMP, o documentación que recomiende abrir el router.
4. Terminación del túnel dentro del guest o sobre vsock 26500/26501/26502.
5. Encendido implícito (por ser humano, por tener workspace virtiofs, por usar `asp session` en vez de `sandbox run`, por adjuntar después a una sesión creada sin el flag).
6. Excepciones estrechas, split DNS, o «todo menos estos prefijos».
7. Fallback al proxy, al NAT o al DNS sink del nodo en `pending` o `withdrawn`, aunque sea «solo hasta que el attach llegue».
8. Contar el túnel como actividad de idle.
9. Relay de payload en el CP, puente L2, compartir túnel entre sesiones.
10. Implementar el dataplane de ADR-0008 «de paso». Como mucho, el log de `local_net` ya nace con `owner_sub`.
11. Un segundo flag del estilo `--local-net-full` para distinguir de una allowlist que v1 ya no tiene.

## Criterios de aceptación para una futura implementación

1. `POST /v1/sandboxes` sin el campo, y `asp session start` sin `--local-net`, dejan `local_net=false`. En el nodo no aparece dispositivo ni regla nueva. El egress sigue ADR-0002. Un test de API cubre el 400 si el body trae policy, prefijos o puertos.
2. Con `--local-net`, antes del attach, un dial a una IP pública, a una IP de LAN y una query DNS **no** salen por el proxy ni por el sink (blackhole comprobable en un nodo con red; en FakeVMM basta el estado `pending` y la ausencia de redirect de ese sandbox al `:8888`).
3. Tras attach, ese mismo dial público y ese dial de LAN salen por el túnel y el NAT del agente local, no por la allowlist del nodo. El DNS también. No hace falta haber configurado un CIDR.
4. Matar el proceso local o simular sueño pasa a `withdrawn`, retira la default útil e instala blackhole **sin** rearmar el proxy. `session stop` e idle reap destruyen el peer aunque el portátil no conteste.
5. La ruta por defecto de la máquina nodo y la de otro sandbox sin el flag no cambian. La subred del TAP de la sesión tunelizada sigue siendo local al nodo.
6. Un segundo sandbox del mismo usuario no reutiliza el peer del primero. Un Bearer de otro `owner_sub` no obtiene grant. El guest no puede cambiar el flag vía exec ni headers.
7. El JSON de sesión sigue sin secretos de túnel.
8. Un `HTTP_PROXY` hacia el proxy del nodo, con el flag on, no obtiene egress por ADR-0002.

## Estado de implementación

Corte que sí está en el árbol (FakeVMM / tests; no demuestra un paquete):

| Pieza | Qué hace |
|---|---|
| API + store | `local_net` bool default false, `local_net_state` `off\|pending\|up\|withdrawn`. Migraciones `010_sandbox_local_net.sql`, `011_sandbox_local_net_node_public.sql` (clave pública del nodo, no el secreto) y `017_local_net_node_tunnel.sql` (puerto y direcciones que asigna el nodo). 400 si el body trae `local_net_policy`, `prefixes`, `cidrs`, `ports`, `routes` o `exceptions` |
| Quién lo enciende | Solo el create autenticado (lab sin IdP incluido) y `asp session start --local-net`. Header `X-ASP-Caller: guest` (o `X-ASP-Guest: 1`) con el campo, y `local_net` dentro de exec o status, responden **403**. El guest no lo cambia |
| Handshake | `POST .../local-net/grant` (el grant en claro no se guarda; solo sha256 y `expires_at`), `POST .../local-net/heartbeat` pasa a `up` y **no** mueve `last_activity_at`, `DELETE .../local-net/attach` pasa a `withdrawn`. Otro `owner_sub` con JWT no recibe grant |
| CLI | `asp session local-net up\|down`. Clave privada modo 0600 junto al JSON, nunca dentro ni en git. Con `wireguard-tools` y `CAP_NET_ADMIN`, `up` ejecuta `ip`+`wg` sin tocar la default del host; si no, imprime el argv. `down` y `session stop` borran el iface local |
| Nodo | Si `local_net` es false, no hay dispositivo nuevo. Si es true, el applier de host ejecuta `ip`/`wg`: `pending` y `withdrawn` borran `wg-asp-{short}` y blackholean la tabla de esa sesión; `up` crea el dispositivo (peer = agente local) y pone la default solo en esa tabla (`iif` del TAP). Nada apunta a `:8888`. Stop e idle reap borran iface y regla. Hace falta `CAP_NET_ADMIN` |

Qué hace el corte de dispositivo (comandos reales, sin lab de paquetes):

- Con `local_net_state=up` y la clave pública del cliente, el node-agent ejecuta `ip link add type wireguard` y `wg set` sobre `wg-asp-{id8}`. El peer es el agente local. La default de **esa** sesión va en una tabla aparte, elegida con `ip rule … iif` del TAP. No hay `ip route replace default` sin `table`.
- `pending` y `withdrawn` borran ese iface y reinstalan blackhole en la misma tabla. `session stop` y el idle reap (`stopping`) llaman a `Clear`, que borra iface, regla y tabla. Nada de eso apunta a `:8888`.
- `asp session local-net up` aplica el extremo cliente con los mismos binarios cuando hay `wireguard-tools` y `CAP_NET_ADMIN`. Si no, imprime el argv. `down` y `session stop` hacen `ip link delete`. La clave privada sigue en modo 0600, fuera del JSON y de git.
- El nodo publica la clave pública (`local_net_node_public`) junto con el puerto y las direcciones que asignó. La privada no sale del directorio de claves del nodo.
- CI sustituye `wg` e `ip` por scripts en el `PATH`. No hace falta el módulo del kernel.

Qué **sigue** sin estar demostrado (límite honesto):

- *Actualizado 2026-10:* una prueba a mano en un host KVM, con dos sesiones y el cliente en un network namespace, pasó tráfico por cada túnel y comprobó que no se cruzan ([ops-local-net.md](../ops-local-net.md)). Sigue sin haber un lab que haya pasado un paquete a una LAN real, a Internet o al DNS de casa por el túnel, ni un NAT comprobado, ni nada de esto en CI. Si `nft` no está en el portátil, el MASQUERADE no se instala.
- No hay temporizador de keepalive de N ventanas en el nodo. El cliente pone `persistent-keepalive 25` en su `wg set`. La caída de control plane sigue siendo explícita: `down`, stop, idle, o un grant caducado (eso pasa a `withdrawn`, no al proxy). El idle reap no borra el iface del portátil; borra el del nodo. El del portátil cae con `down` o `stop`.
- FakeVMM en los tests del reconciler guarda el plan en memoria y no ejecuta `ip`. El applier de producción sí. No hay forwarding hasta una LAN real ni DNS de casa probados (la prueba a mano de arriba llegó hasta el namespace del cliente).
- No hay relay de bytes en el control plane, ni excepciones por CIDR, ni fallback al proxy.
- `dial` sale del `local_net_dial` del nodo de la sandbox (`--local-net-dial`, [ADR-0011](0011-multi-node.md)) y, si el nodo no lo declara, de `ASP_LOCAL_NET_DIAL` (vacío si no está). Sin endpoint el cliente no tiene a quién marcar y el egress sigue hundido.

## Referencias cruzadas

- Egress del nodo, vigente **solo** con `local_net=false`: [0002](0002-networking.md), [0006](0006-fase-2e-nft-ssh-guest.md)
- Identidad de flujos (mismo `owner_sub`, otro path cuando el flag está on): [0008](0008-network-flow-attribution.md)
- La sesión que acota la vida del túnel: [0009](0009-agent-sessions.md)
- Por qué / qué ganamos: [`../why-on-demand-local-net.md`](../why-on-demand-local-net.md)
- Roadmap (ítem futuro): [`../roadmap.md`](../roadmap.md)
