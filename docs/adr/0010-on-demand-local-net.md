# ADR-0010: Red local bajo demanda (túnel iniciado por el agente local)

- **Estado:** Propuesta — **no implementada**
- **Fecha:** 2026-10-03
- **Relacionados:** [0002](0002-networking.md) (TAP + proxy + nft; default route del nodo), [0008](0008-network-flow-attribution.md) (flujo → `owner_sub`, evaluación), [0009](0009-agent-sessions.md) (la sesión es el objeto; egress de esa microVM), [0007](0007-multi-user-identity.md) (`owner_sub`), [`../why-on-demand-local-net.md`](../why-on-demand-local-net.md), [`../roadmap.md`](../roadmap.md)
- **No es:** una VPN de sitio, un agujero de entrada en el router de casa, ni un segundo default route. El egress público (ADR-0002) no se mueve.

## Contexto

El sandbox vive en un **nodo remoto** (lab o capacidad compartida). Su única red de producción hoy es la de ADR-0002: TAP `asp-{shortID}`, NAT del nodo, forward proxy `:8888`, DNS sink y nft `asp_egress`. La ruta por defecto sale por el **egress del nodo**. El guest no tiene `NET_ADMIN`. Eso está bien para git, registries y APIs públicas allowlisted.

No alcanza cuando el trabajo del agente está en la **LAN del usuario**: un NAS, un servicio en `192.168.1.50:8080`, una impresora, un dev server que no está en Internet. Ese prefijo no es enrutable desde el nodo. Las formas ingenuas de «arreglarlo» son peores que el gap:

1. **DNAT / UPnP / port forward en el router de casa** — un listener alcanzable desde el nodo (o desde Internet) dentro de la LAN. Sobrevive a la sesión, no sabe de `owner_sub`, y un sandbox olvidado sigue teniendo camino de vuelta.
2. **`ssh -R` o reverse tunnel dejado abierto** — mismo problema: el extremo de casa escucha, o el forward queda atado a un proceso que nadie asocia al ciclo `asp session`.
3. **Meter `0.0.0.0/0` por el portátil** — convierte el portátil en egress de Internet del sandbox. Mezcla la política deny-by-default del nodo con la red de casa y con el NAT del ISP. Fuera de este diseño salvo un opt-in futuro explícito que **no** es v1.
4. **Que el guest elija rutas** (`ip route`, marks, `HTTP_PROXY` hacia casa) — el guest no es confiable (ADR-0002/0003/0007). Cualquier ruta que el workload pueda instalar no es frontera.

Restricciones ya acordadas, y que este ADR fija:

- El túnel es **saliente desde el agente local** (el proceso en la máquina del usuario, que ya está en esa LAN). Nadie abre un hueco de entrada hacia la LAN.
- Transporte candidato: **WireGuard** (dispositivo L3 real) **o** reutilizar el patrón vsock/TCP que ya existe en el nodo, pero **el agente local inicia** el canal. El nodo no llama a un puerto del router de casa.
- Solo **prefijos declarados** (ejemplo `192.168.1.0/24`) van por el túnel. La ruta por defecto del guest sigue en el egress del nodo. Prohibido instalar `0.0.0.0/0` (ni `::/0`) hacia casa salvo petición explícita futura, no v1.
- Política **allowlist por sesión**: CIDR **y** puertos. Auditoría de flujos. El túnel muere con la sesión o por timeout / caída (portátil dormido). El guest no elige rutas; las instala el plano de control.
- Camino **separado** del egress público: otro netdev o tabla de rutas. No reutilizar el forward proxy HTTP como si un NAS hablara CONNECT.
- Si el portátil duerme, las rutas se **retiran**. Los paquetes a esos prefijos no caen a la ruta por defecto.
- **Solo bajo demanda.** Default apagado: `local_net=false` hasta que el caller opta (`asp session start --local-net` o el campo equivalente en `POST /v1/sandboxes`). La identidad del tráfico sigue siendo `owner_sub`. No hay un sujeto nuevo.

Hoy no existe un agente local de datos. `asp` es un cliente HTTPS del control plane. El fichero `~/.cache/asp/sessions/<nombre>.json` guarda id y URL, **sin** token (ADR-0009). Los puertos vsock 26500/26501/26502 son host↔guest **dentro del nodo**; no cruzan la WAN y no deben terminar un túnel hacia la LAN del usuario. `Node.agent_endpoint` es cómo el CP habla con el node-agent, no un derecho del guest a pivotar.

## Decisión

**La red local es una capacidad de la sesión, apagada por defecto, que el plano de control enciende solo si el caller lo pide.** Cuando está encendida, el **agente local** (en la máquina del usuario) abre un túnel **saliente** hacia el node-agent. El node-agent instala, en un dispositivo o tabla **distintos** del egress público, rutas únicamente a los prefijos y puertos que el control plane persistió. El guest ve su TAP de siempre y no recibe claves, tablas ni `NET_ADMIN`.

Si el túnel no está, esos prefijos se **hunden** (blackhole / unreachable en el nodo). No hay fallback a la ruta por defecto. Cuando la sesión termina, el idle reap corre, o el agente local deja de contestar, el nodo **retira** rutas y túnel. No queda listener en casa.

### Forma concreta de API y CLI (contrato; sin código en este cambio)

`POST /v1/sandboxes` gana dos campos. Ausentes o false = comportamiento de hoy.

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

Opt-in (el CP rechaza combinaciones a medias con 400):

```json
{
  "local_net": true,
  "local_net_policy": {
    "prefixes": [
      {"cidr": "192.168.1.0/24", "proto": "tcp", "ports": [443, 8080]},
      {"cidr": "192.168.1.50/32", "proto": "tcp", "ports": [22]}
    ]
  }
}
```

Reglas de validación del CP (fail closed):

| Entrada | Resultado |
|---|---|
| Campo omitido o `"local_net": false` sin policy | Sesión normal. Cero rutas, cero túnel |
| `local_net: false` **con** policy | **400**. No se ignora en silencio: o está apagado o está declarado |
| `local_net: true` sin `prefixes` o con lista vacía | **400** |
| CIDR no parseable, prefijo más amplio que el tope del tenant, `0.0.0.0/0`, `::/0` | **400** |
| Solape con la subnet guest (`10.200.0.0/16` por defecto), con rangos link-local (`169.254.0.0/16`, `fe80::/10`) o con las redes que el nodo declara como propias | **400** |
| `proto` distinto de `tcp` en v1 | **400** (UDP/multicast no entran) |
| `ports` vacío, puerto 0, o más de 16 puertos por prefijo | **400** |
| Más de 8 prefijos por sesión (tope de v1) | **400** |
| `owner_sub` / rutas dentro del body como autoridad | Igual que ADR-0007: el sujeto sale del JWT. Un `owner_sub` de cliente no elige el dueño. La policy de prefijos sí la propone el caller, pero solo dentro del **tope** que el tenant tiene configurado |

CLI (misma semántica; el binario sigue sin ser un plugin):

```text
asp session start --node-id=dev-node
  # no envía local_net (o envía false). Default de hoy

asp session start --local-net \
  --local-net-allow 192.168.1.0/24:tcp:443,8080 \
  --local-net-allow 192.168.1.50/32:tcp:22

asp session start --local-net-allow 192.168.1.0/24:tcp:443
  # 2: --local-net-allow sin --local-net es error, no un opt-in implícito
```

El flag repetible `--local-net-allow` es azúcar de `prefixes[]`. No existe `--local-net-default-route` en v1.

El agente local, **después** de que la sesión está `running` y `local_net=true`, se ata con un grant de corta vida (no con el JSON de sesión):

```text
POST /v1/sandboxes/{id}/local-net/grant
  Authorization: Bearer (mismo IdP; el CP exige sub == owner_sub de ese sandbox)
  → { "grant": "<opaco, TTL minutos>", "dial": "<url node-agent o rendezvous>", "expires_at": "..." }

# el proceso local abre el túnel saliente usando el grant
POST /v1/sandboxes/{id}/local-net/heartbeat   # o el propio keepalive del transporte
DELETE /v1/sandboxes/{id}/local-net/attach    # cierre ordenado (sleep, stop del agente)
```

Forma de CLI equivalente, no implementada: `asp local-net attach --name NOMBRE`. Tiene que resolver el Bearer por `asp auth`, no leer un secreto del fichero de sesión. `asp session stop` (y el reaper) piden el detach aunque el attach no haya existido.

`GET /v1/sandboxes/{id}` expone estado para que el usuario vea la verdad:

```json
{
  "local_net": true,
  "local_net_state": "pending",
  "local_net_policy": { "prefixes": [ /* la persistida, no la que el guest querría */ ] }
}
```

`local_net_state`: `off` | `pending` | `up` | `withdrawn`.

- `off` — flag apagado. No hay política efectiva.
- `pending` — opt-in aceptado, túnel aún no establecido. Prefijos en blackhole.
- `up` — agente local autenticado y keepalive vivo. Rutas instaladas.
- `withdrawn` — estaba pedido pero el túnel cayó (sueño, red, detach) o la sesión está muriendo. Rutas fuera. Blackhole si la sesión sigue viva y el flag sigue true, para que un reattach no deje una ventana de fuga por la default. Al pasar el sandbox a `stopping`/`stopped` se borra también el blackhole.

El arranque de la sesión **no** espera al túnel. El sandbox puede estar `running` con `local_net_state=pending`. Quien necesite fallo duro puede, en un corte posterior, pasar `--local-net-required`; **no** es v1. v1: la VM arranca igual y los dials a la LAN fallan cerrados hasta que el attach existe.

### Modelo de datos (previsto)

Sin migración en este cambio. Cuando se implemente, campos en `sandboxes` (siguiente migración después de `007`; el número exacto lo fija el corte de código):

| Campo | Tipo | Notas |
|---|---|---|
| `local_net` | bool NOT NULL default `false` | El default de SQL y el de la API coinciden. Un store viejo que no mande el campo sigue en false |
| `local_net_policy` | jsonb NULL | La lista validada. NULL si `local_net` es false |
| `local_net_state` | text NOT NULL default `off` | `off` / `pending` / `up` / `withdrawn` |
| `local_net_attached_at` | timestamptz NULL | Último paso a `up` |
| `local_net_grant_expires_at` | timestamptz NULL | Tope del grant vigente; no es un secreto |

El grant en claro **no** se guarda en Postgres ni en `sessions/<nombre>.json`. Solo hash o id si hace falta revocar. El JSON de sesión puede anotar `local_net: true` como recordatorio de UX; no es autoridad y no lleva clave WireGuard.

Eventos (`sandbox_events`, `actor_sub` = quien llamó):

- `sandbox.local_net_requested` — create con flag (payload = policy **sin** secretos)
- `sandbox.local_net_up` / `sandbox.local_net_withdrawn` — transiciones que reporta el node-agent
- `sandbox.local_net_denied` — 400 de política (opcional; el 400 HTTP ya es la respuesta al caller)

La auditoría de **flujos** no es una fila por paquete en `sandbox_events`. Es el mismo sitio que ADR-0008 reserva al proxy: log estructurado en el nodo, con `sandbox_id`, `owner_sub`, `tenant_id`, `dst`, `proto`, `port`, `action=forward|deny`, `path=local_net`. Puede existir antes de que 0008 esté implementado; la clave de identidad es la misma. El guest no rellena esos campos.

Tope de tenant (configuración de operador, no del guest), ejemplo de forma:

```text
ASP_LOCAL_NET_MAX_PREFIXES=8
ASP_LOCAL_NET_ALLOW_CIDRS=192.168.0.0/16,10.0.0.0/8
```

Una policy de sesión más amplia que ese tope se rechaza. Sin tope configurado, el CP **rechaza** `local_net: true` (fail closed). Lab que quiera probarlo tiene que declarar el tope; no hay default «cualquier RFC1918».

### Responsabilidades

```text
caller (asp session start --local-net)          agente local (máquina del usuario)
        │ Bearer IdP                                      │ mismo owner_sub, grant de corta vida
        ▼                                                 │ dial SALIENTE
control plane ──desired state (policy, no rutas guest)──► node-agent
        │                                                 │ netdev o tabla APARTE
        │ owner_sub sellado en el create                  │ blackhole si no hay túnel
        ▼                                                 ▼
     sandbox row                                    guest (TAP de siempre)
                                                    sin NET_ADMIN, sin claves, sin ruta elegida
```

**Control plane**

- Única autoridad de `local_net` y de la policy. La sella en el create junto a `owner_sub`.
- Rechaza los casos de la tabla de arriba. No «arregla» un CIDR demasiado amplio recortándolo en silencio.
- Emite el grant solo si el Bearer pertenece al `owner_sub` de **ese** sandbox (o a un rol admin ya definido en ADR-0007; el admin no hereda la LAN de otro usuario: un admin puede apagar, no attacharse a la casa de otro). v1: **solo el `owner_sub`** puede hacer grant/attach.
- Al `DELETE`, al idle reap (`stop_reason=idle_timeout`) y al fallo del sandbox, el desired state pasa a túnel muerto. No espera a que el portátil coopere.
- No trata bytes del túnel como actividad del reaper. `last_activity_at` sigue siendo create, paso a `running`, exec bien proxyado y stdin bien proxyado (ADR-0009). Un túnel callado no mantiene la VM viva.
- No mete la policy por `X-ASP-*` ni por el body de exec.

**Node-agent**

- Lee la policy del work item. Si `local_net` es false, no crea dispositivo ni regla.
- Crea un netdev **por sandbox** (v1) o una tabla de policy routing distinta de la default: `wg-asp-{short}` / `tun-asp-{short}`, o `ip rule` + tabla que no contiene `default`.
- Instala solo los CIDR de la policy. nft (o filtro equivalente en el forward del túnel) limita proto/puertos. Lo que no coincide se droppea en el nodo, no se manda al agente local «a ver».
- Mientras `local_net_state != up`, esos CIDR están en **blackhole**. No se enruta por `asp_nat` ni por el proxy `:8888`.
- Acepta el túnel solo con el grant vigente ligado a ese `sandbox_id`. Un grant de otro sandbox no instala rutas aquí.
- Keepalive (orden de decenas de segundos, valor concreto en la implementación). N fallos → `withdrawn`, borra rutas útiles, deja blackhole, evento al CP.
- Al Stop de la microVM, destruye dispositivo, rutas, blackhole y estado de peer. No reutiliza la clave en el sandbox siguiente.
- Audita flujos con `owner_sub` que le pasó el CP (cache de ADR-0008). No lee identidad del paquete interno.
- Sigue sin dar `NET_ADMIN` al guest. El TAP del guest no es el dispositivo del túnel.

**Guest**

- Habla IP por su TAP como hoy. Si el destino cae en un prefijo tunelizado, el **nodo** lo desvía. El guest no tiene una ruta que pueda borrar para «salir por otro sitio»: la desviación es policy routing del host por marca o por destino, fuera del guest.
- No ve claves, no pide `local_net` en el exec, no puede ampliar puertos.
- Un dial a un prefijo en policy con túnel caído falla (blackhole). Un dial a Internet sigue el egress público y la allowlist de ADR-0002, aunque `local_net` esté true.

**Agente local**

- Proceso en la máquina que **ya** tiene la ruta hacia esa LAN. No es el node-agent ni el pod-daemon.
- Inicia el transporte hacia fuera. No abre un socket de escucha en interfaces de la LAN, no configura UPnP, no pide port forward.
- Filtra otra vez: solo reenvía a los destinos de la policy. Defensa en profundidad si el nodo estuviera mal. No hace bridge de `eth0`/`wlan0`.
- Cierra el túnel al dormir, al apagar, al `session stop` y al perder el grant. No es un servicio que rearranque el túnel para **otras** sesiones ni para otro `owner_sub`.
- No escribe la clave privada en el JSON de sesión ni en el repo.

### Transporte (decisión de diseño, aún sin código)

**v1 preferido: WireGuard en un dispositivo por sandbox, con el handshake iniciado por el agente local.**

El nodo escucha WireGuard solo en un endpoint que el agente puede marcar (UDP reachable del node-agent, o TCP/TLS de fuera que encapsula el UDP de WG si el camino de red del lab no deja UDP). La primera paquetización sale del portátil, así que el NAT de casa no necesita un mapping permanente ni un puerto publicado. El nodo instala el `wg` dev y mete ahí **solo** los `AllowedIPs` iguales a la policy (no `0.0.0.0/0`).

Por qué WG y no el primer día un protocolo propio: hace falta un L3 que policy routing y nft ya saben tratar, con claves de sesión y roaming corto si el portátil cambia de IP. El agente local puede usar wireguard-go; no se exige el módulo del kernel en el guest (el guest no participa).

**Alternativa válida si WG no entra en el nodo:** un stream TCP/TLS que el agente local abre hacia el `agent_endpoint` (mTLS de nodo o grant como bearer de ese dial) y que lleva tramas L3 hacia un TUN. Es el mismo patrón mental que el CONNECT de vsock del nodo (framing sobre un canal ya autenticado), **no** es reutilizar el puerto 26500. 26500 sigue siendo exec host→guest. Meter la LAN del usuario dentro de ese vsock pondría al guest a terminar el túnel: **rechazado**.

Si el portátil no puede marcar el node-agent (nodo solo visible para el CP), el rendezvous es: ambos dial salientes a un sitio que el CP nombra. **El payload de la LAN no se proxya por el proceso del control plane en v1.** Si no hay camino de datos directo, `local_net_state` se queda en `pending` y se documenta el lab. Un relay de datos en el CP es otro ADR (coste, secreto de tránsito, HA).

### Flujos

#### A) Opt-in feliz

```text
asp session start --local-net --local-net-allow 192.168.1.0/24:tcp:443
  → POST /v1/sandboxes  local_net=true + policy
  → CP valida tope del tenant, sella owner_sub, state=pending, evento requested
  → reconciler arranca la VM (TAP + egress público como siempre)
  → node-agent instala blackhole 192.168.1.0/24 en la tabla local_net de ESE sandbox
  → asp local-net attach
       grant (sub == owner_sub) → dial saliente → WG up
  → node-agent sustituye blackhole por ruta dev wg-asp-{short} + filtro tcp/443
  → state=up
guest: curl https://192.168.1.20/  → TAP → policy route del nodo → túnel → agente local
       el agente local solo forward si dst:proto:port ∈ policy → LAN del usuario
guest: curl https://github.com/    → ruta default → proxy :8888 → allowlist ADR-0002
       (no entra al túnel)
```

#### B) Sin flag (el caso común)

```text
asp session start
  → local_net false, policy NULL, state off
  → no grant, no dispositivo, no blackhole
  → el guest que marque 192.168.1.20 sale por la default del nodo y muere
     en la allowlist / en un destino no enrutable
  → no hay proceso en el portátil escuchando por esta sesión
```

#### C) El portátil duerme

```text
keepalive pierde N ventanas
  → node-agent: state=withdrawn, evento local_net_withdrawn
  → borra la ruta vía wg, reinstala blackhole de los mismos CIDR
  → no toca la default ni asp_egress
  → el sandbox puede seguir running (el reaper de idle es otro reloj)
guest dial a 192.168.1.20 → cuelga / unreachable. No sale a Internet con IP privada.
el portátil despierta y attach de nuevo, mismo sandbox, grant nuevo
  → state=up, rutas otra vez
```

#### D) Fin de sesión

```text
asp session stop | DELETE | idle reap
  → desired state: túnel muerto
  → node-agent destruye dev, peer, rutas, blackhole, filtros
  → un heartbeat tardío del agente local recibe 409/404 y cierra
  → no queda AllowedIPs ni DNAT hacia la LAN
```

#### E) Guest hostil con la flag puesta

```text
el workload escanea 192.168.1.0/24 puertos 1-65535
  → nft del nodo (y el filtro del agente local) dejan pasar solo tcp/443 y tcp/8080
  → el resto drop + audit path=local_net action=deny owner_sub=...
el workload intenta `ip route add default via …` o un mark
  → sin NET_ADMIN no instala nada; si pudiera, la policy route del host gana igual
    porque la clasificación es por destino en el nodo, no por la tabla del guest
el workload manda X-ASP-Local-CIDR o un prefijo en el exec
  → el CP no lee rutas del exec; el nodo ignora el header
```

## Alternativas consideradas

| Alternativa | Pros | Contras | Decisión |
|---|---|---|---|
| **Inbound a la LAN** (DNAT, UPnP, `ssh -R` en el router) | El nodo «llega» sin proceso en el portátil | Hueco de entrada, sobrevive a la sesión, no hay allowlist por puerto fácil de retirar al dormir | **Rechazada** |
| **Default route `0.0.0.0/0` por el portátil** | Cualquier destino «de casa» funciona, incluido split tunnel vago | El sandbox usa la IP del usuario para todo Internet; se salta el proxy y la allowlist; duerme = se cae **todo** el egress | **Rechazada en v1.** Solo un flag futuro con nombre propio, nunca el default de `--local-net` |
| **Rutas elegidas por el guest** | Cero trabajo en el nodo | Forgeable; contradice ADR-0002 y el espíritu de ADR-0008 | **Rechazada** |
| **Reutilizar el forward proxy HTTP** para la LAN | Un solo choke point | Los destinos de LAN no son CONNECT/HTTPS corporativo; mezclarlos ensucia la allowlist de hostnames | **Rechazada** como datapath. La auditoría sí puede parecerse a la de ADR-0008 |
| **Terminar el túnel en el guest** (WG o vsock 26500 dentro de la microVM) | El nodo no enruta | Claves y rutas en el workload; el guest amplía destinos; 26500 es exec | **Rechazada** |
| **Un túnel de nodo para todas las sesiones del mismo `owner_sub`** | Menos handshakes | Un sandbox olvidado y uno nuevo comparten peer; el teardown de uno rompe al otro; mezclar políticas es fácil | **Rechazada en v1.** Un túnel por sandbox |
| **Relay de payload en el control plane** | Funciona aunque el nodo sea inalcanzable desde casa | El CP ve tráfico de la LAN, coste, no es su trabajo | **Fuera de v1** |
| **Solo TCP tunelizado a mano, sin WG** | Menos dependencia | Se reinventa crypto, roaming y un L3 que nft ya entiende | **Reserva** si WG no es operable. Misma política, otro encapsulado |
| **mDNS / broadcast / «descubre mi LAN»** | UX de impresoras | Refleja tráfico L2, amplía la superficie más allá de la allowlist | **Fuera de v1** |
| **Encender local_net por defecto si el cliente es un humano** | Menos flags | Contradice el mandato on-demand; un agente autónomo tocaría la LAN sin que nadie lo pidiera | **Rechazada** |

### Qué no funciona (explícito)

1. **Publicar un puerto en casa** y llamar a eso «túnel saliente». Si el mapping existe sin un proceso que lo cierre al `stop`, no cumple este ADR.
2. **Quitar el blackhole cuando el túnel cae** «para que al menos falle rápido por la default». Eso mete `192.168.1.20` en el NAT del nodo y, peor, puede acertar **otra** red `192.168.1.0/24` distinta de la del usuario.
3. **Confiar en el fichero de sesión como capability del túnel.** No lleva token hoy y no debe llevar la clave WG mañana.
4. **Dejar que un admin de tenant se attach a la LAN del `owner_sub`.** Puede destruir la sesión; no hereda el grant.
5. **Contar paquetes del túnel como `last_activity_at`.** Un scan lento o un keepalive impedirían el idle reap para siempre.
6. **SoftFail del estilo nft:** si el nodo no puede instalar la tabla, la feature no «sigue igual». El sandbox con `local_net=true` que no pueda blackholear debe fallar el Start de esa política (estado `failed` o `local_net_state` que no pasa a `up` y log claro). Nunca «rutas a medias hacia la default».

## Consecuencias

### Positivas (si se implementa)

- El sandbox remoto alcanza destinos **nombrados** de la LAN del usuario sin abrir el router.
- El caso normal (`asp session start` sin flag) no cambia de amenaza: no hay túnel, no hay proceso local, no hay ruta.
- La frontera sigue fuera del guest. Encaja con ADR-0002 (default en el nodo), ADR-0007 (`owner_sub`) y ADR-0009 (la sesión es quien vive y quien muere).
- Al dormir el portátil la LAN deja de ser alcanzable en segundos, no al cabo del idle de la VM (lab: 2h).
- La auditoría puede usar la misma clave humana que ADR-0008, en un path distinto (`local_net` vs egress público).

### Negativas / coste

- Con `--local-net`, un guest comprometido **puede** hablar con esos `ip:port` mientras el túnel esté `up`. Eso es el riesgo que se compra. No es «la LAN entera» si los filtros se cumplen; tampoco es cero.
- Hay un dataplane nuevo (WG o TUN) en el nodo, con keepalive, blackhole y teardown. Más formas de dejar una ruta colgada si Stop falla a medias: el Stop tiene que ser idempotente.
- El operador debe publicar un tope de CIDR. Sin eso la feature está apagada incluso con el flag (fail closed).
- Dos redes `192.168.1.0/24` (casa del usuario y, por desgracia, algo en el camino del nodo) se confunden si el filtro de solape no está. Por eso el solape con las redes del nodo es 400, no un warning.
- WireGuard en el nodo es otra dependencia operativa (binario o módulo, UDP). La reserva TCP existe para no bloquear el diseño en eso, pero no se implementan las dos en el primer corte.
- El agente local es software nuevo en la máquina del usuario. `asp` hoy no lo es.

### Límites honestos / no-goals

- **No implementado** con este ADR. Sin flags en el binario, sin migración, sin peer WG.
- **v1 no** lleva `0.0.0.0/0` ni `::/0` hacia casa, ni un flag escondido que lo haga.
- **v1 no** publica servicios del guest hacia la LAN (nada de DNAT inverso, nada de «entra a mi sandbox desde el NAS»).
- **v1 no** hace UDP, QUIC a la LAN, ICMP más allá de lo que el blackhole necesite para fallar, mDNS, SSDP, LLMNR ni escaneo de descubrimiento.
- **v1 no** resuelve nombres (`nas.home`). Quien no tenga la IP no «aparece» por arte de DNS. Hijack de DNS es otro diseño.
- **v1 no** comparte túnel entre sandboxes ni entre usuarios de un mismo portátil.
- **v1 no** relaya bytes por el control plane.
- **v1 no** mantiene la VM despierta porque el túnel tenga tráfico.
- **v1 no** exige ADR-0008 mergeado, pero no inventa otra identidad: el campo es `owner_sub`.
- **v1 no** demuestra el túnel en CI sin red de verdad. FakeVMM puede persistir el flag y el estado `pending`; eso no prueba forwarding. Igual que nft soft no prueba anti-bypass.
- IPv6 de la LAN (ULA) queda fuera hasta que el egress del nodo tenga una historia IPv6 (gap ya abierto en ADR-0002).

## Qué no construir en v1

Lista cerrada para el primer corte, cuando exista. Si una de estas entra en el PR inicial, el corte se ha salido del ADR.

1. VPN de sistema en el portátil, cliente WG global, o cualquier `AllowedIPs = 0.0.0.0/0`.
2. Listener en la LAN, UPnP, PCP, NAT-PMP, o documentación que recomiende abrir el router.
3. Terminación del túnel dentro del guest o sobre vsock 26500/26501/26502.
4. Encendido implícito (por ser humano, por tener workspace virtiofs, por usar `asp session` en vez de `sandbox run`).
5. Policy sin puertos («todo el TCP del /24»).
6. Ampliar prefijos a mitad de sesión desde el guest o desde un exec. Un cambio de policy, si algún día existe, es otra llamada autenticada al CP que reinstala rutas; no es v1.
7. Fallback a la ruta por defecto cuando el túnel cae.
8. Contar el túnel como actividad de idle.
9. Relay de payload en el CP, mDNS, IPv6, UDP, compartir túnel entre sesiones.
10. Implementar el dataplane de ADR-0008 «de paso». Como mucho, el log de `local_net` ya nace con `owner_sub`.

## Criterios de aceptación para una futura implementación

1. `POST /v1/sandboxes` sin el campo, y `asp session start` sin `--local-net`, dejan `local_net=false`. En el nodo no aparece dispositivo ni regla nueva. Un test de API cubre el 400 de las combinaciones de la tabla.
2. Con `--local-net` y policy legal, antes del attach el destino de la policy **no** sale por el proxy ni por la default (blackhole comprobable en un nodo con red; en FakeVMM basta el estado `pending` y la ausencia de ruta default asociada).
3. Tras attach, solo los puertos listados conectan; un puerto no listado del mismo CIDR falla y deja audit con `owner_sub`.
4. Matar el proceso local o simular sueño pasa a `withdrawn` y retira la ruta útil sin tocar la default. `session stop` e idle reap destruyen el peer aunque el portátil no conteste.
5. Un segundo sandbox del mismo usuario no reutiliza el peer del primero. Un Bearer de otro `owner_sub` no obtiene grant.
6. El JSON de sesión sigue sin secretos de túnel.

## Referencias cruzadas

- Egress público que **no** se sustituye: [0002](0002-networking.md), [0006](0006-fase-2e-nft-ssh-guest.md)
- Identidad de flujos (mismo `owner_sub`, otro path): [0008](0008-network-flow-attribution.md)
- La sesión que acota la vida del túnel: [0009](0009-agent-sessions.md)
- Por qué / qué ganamos: [`../why-on-demand-local-net.md`](../why-on-demand-local-net.md)
- Roadmap (ítem futuro): [`../roadmap.md`](../roadmap.md)
