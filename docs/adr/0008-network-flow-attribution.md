# ADR-0008: Atribución de flujos de red (TCP/egress) a `owner_sub`

- **Estado:** Propuesta / **Aceptada para evaluación** — **no implementada**
- **Fecha:** 2026-10
- **Relacionados:** [0002](0002-networking.md) (TAP + proxy + nft), [0006](0006-fase-2e-nft-ssh-guest.md) (nft redirect), [0007](0007-multi-user-identity.md) (`owner_sub` / `actor_sub`), [0010](0010-on-demand-local-net.md) (otro path de red, misma identidad), [`../why-network-flow-attribution.md`](../why-network-flow-attribution.md), [`../architecture.md`](../architecture.md) § Red, [`../roadmap.md`](../roadmap.md)
- **Extiende:** la frontera de egress de ADR-0002 con **sujeto humano** en el plano de red (no solo en create/exec/OIDC)

## Contexto

ADR-0007 ya atribuye **acciones de control plane** a un humano (`owner_sub` en el sandbox, `actor_sub` en eventos) y cadena humana en tokens de workload (`user_sub` / `act`). Eso responde a: *quién creó / ejecutó / destruyó / firmó*.

La pregunta del auditor corporativo que **sigue abierta** es distinta:

> Esta conexión TCP/HTTPS que salió del nodo hacia `api.empresa.com` — **¿de qué sandbox y de qué empleado es?**

Hoy el dataplane de red sabe (de forma fiable o semi-fiable):

| Señal | Dónde | Atribuye a humano | Honesto |
|---|---|---|---|
| `sandbox_id` opcional en header `X-ASP-Sandbox-ID` | Forward proxy audit JSON | Solo si el cliente lo manda y se confía | **No** — el guest puede forjar/omitir el header |
| Allowlist / deny / CONNECT host:port | Proxy + DNS sink | Tenant vía política adjunta a exec | Parcial: tenant/política, no `owner_sub` |
| Nombre TAP `asp-{shortID}` | Host (`--tap-auto`) | Sandbox (8 chars del UUID) | Sí a sandbox **si** se correlaciona; hoy no se propaga a logs de egress |
| IP host TAP | una `/30` propia por sandbox dentro de `--guest-subnet` (`10.200.0.0/16`): TAP `.1`, guest `.2` (`--tap-auto`) | Sandbox, dentro del nodo | **Sí, dentro del nodo** — la IP origen identifica la sandbox; el proxy ya la usa para elegir la política del tenant. *Actualizado 2026-10: antes era `10.200.0.1/24` en cada TAP* |
| nft `asp_egress` redirect | iif del subnet guest → proxy | Flujo forzado al proxy | Atribución aún no etiquetada |
| `sandbox_events` / OIDC `user_sub` | CP / JWT | Humano en plano de control | No aparece en conntrack ni en SIEM de red |

Restricciones reales del código actual (no wishful):

- `node-agent/internal/egress.ForwardProxy` audita `sandbox_id` leído del header; **no** consulta store/`owner_sub`.
- *Actualizado 2026-10:* el reconciler da a cada TAP su propia `/30` (`tap.GuestNet`), así que dentro de un nodo la IP origen sí es única por sandbox (`tap.Manager.DefaultHostCIDR` solo queda para quien crea un TAP sin pasar por el reconciler). **No** hay IP única global: dos nodos reparten el mismo `--guest-subnet`.
- nft redirect opera por **subnet** (`10.200.0.0/16`), no por mark por sandbox.
- El guest **no es confiable** (ADR-0002/0003): no `NET_ADMIN` de producción; cualquier mark/header que el guest elija es forgeable.
- ADR-0007 rechazó UID Linux guest ↔ humano; lo mismo aplica a “el proceso dentro del guest pone un mark”.

Amenazas / requisitos que el gap deja abiertos:

1. **SIEM / SOC:** correlacionar alertas de firewall corporativo o del proxy ASP con `owner_sub` / email.
2. **Forced egress corporativo:** inyectar identidad (header, mTLS client cert, SNI tag, log field) **en el hop host**, no en el guest.
3. **Multi-user en el mismo nodo:** dos sandboxes del mismo tenant hacia el mismo destino deben distinguirse *después* del NAT.
4. **No-HTTP / raw TCP:** CONNECT/HTTP proxy no ve todo; hace falta señal L3/L4 o mark que sobreviva a MASQUERADE.

## Decisión (para evaluación — no código aún)

**Tratar la atribución de flujos como un mapa host-side `flujo → sandbox_id → owner_sub`**, anclado en señales que el **guest no puede forjar**, y usar el **egress proxy (+ nft enforce)** como camino natural de primera entrega.

Orden de preferencia para el diseño (de más pragmático a más pesado):

| # | Mecanismo | Idea | Cuándo evaluarlo |
|---|---|---|---|
| **1** | **Egress proxy attribution** (camino natural) | El proxy ya es el choke point HTTP(S)/DNS. Resolver `sandbox_id`/`owner_sub` por **origen host-confiable** (IP guest única, mark, o iif), **ignorar** headers del guest; enriquecer audit JSON + opcionalmente headers hacia el proxy corporativo upstream | **Primera opción** — reusa ADR-0002/0006 |
| **2** | **IP / TAP por sandbox** | Asignar IP (o /30) única por sandbox; tabla en node-agent `guest_ip → {sandbox_id, owner_sub, tenant_id}` actualizada en Start/Stop | Base casi necesaria para (1) y para raw TCP |
| **3** | **nft `ct mark` / `meta mark`** | En prerouting/forward, `meta iifname "asp-*" mark set …` o map iif→mark; conntrack conserva mark tras NAT; proxy/eBPF/ulog lee mark | Cuando RemoteAddr post-NAT se pierde o hay raw TCP |
| **4** | **VLAN / VRF por sandbox o por owner** | Aislamiento L3 más fuerte; ruteo/policy routing hacia proxies distintos | Multi-tenant host denso / requisitos de red corporativa duros |
| **5** | **eBPF** (tc/cgroup/sockops) | Adjuntar cookie/metadata al socket; export a maps para audit | Si marks nft no bastan o se quiere atribución sin proxy |

**Decisión de diseño propuesta para la evaluación:** adoptar **(1)+(2) como MVP de atribución**, con **(3) como follow-up** para raw TCP y post-NAT; **(4)/(5) como opciones** si ops/corporate lo exigen. Explicitar: *sin IP (o mark) host-side por sandbox, el proxy no puede atribuir de forma no forgeable*.

### Relación con ADR-0007

```text
ADR-0007                          ADR-0008 (este)
────────                          ───────────────
owner_sub en sandboxes     ──►    misma clave en el plano de red
actor_sub en sandbox_events       (flujo ≠ acción CP; owner del sandbox
user_sub / act en OIDC             es la atribución estable del egress)
guest no elige claims      ──►    guest no elige marks / sandbox_id / owner
```

- **`owner_sub`** es la atribución **estable** del tráfico del sandbox (quién “posee” esa microVM).
- **`actor_sub`** de un `exec` concreto **no** viaja en cada SYN (el guest no es autoridad). Opcional futuro: anotar “última acción humana” en metadatos host, sin pretender que sea el dueño del socket.
- El mint OIDC ya lleva `user_sub=owner_sub`; la red debe poder contar la **misma historia** en logs de egress sin depender del JWT.

### Flujos (diseño objetivo)

#### A) Camino natural — HTTP(S) vía forward proxy (MVP evaluable)

```text
guest (HTTP_PROXY=http://<host-tap-ip>:8888)
  → TCP a IP del TAP host (única por sandbox si se adopta §2)
  → [nft asp_egress enforce: redirect si intentó bypass]
  → ForwardProxy
       1. src = RemoteAddr (o ct mark / iif)  — NO header X-ASP-Sandbox-ID
       2. lookup node-local: src → sandbox_id → owner_sub (cache desde reconciler/CP)
       3. audit JSON: {sandbox_id, owner_sub, tenant_id, host, port, action}
       4. (opcional) hacia proxy corporativo: Header o mTLS con identidad ASP
          inyectada por el *host* (p.ej. X-ASP-Owner-Sub), nunca reenviada desde guest
  → Internet / corporate proxy
```

#### B) Raw TCP / no-HTTP (follow-up)

```text
guest SYN → TAP asp-{short}
  → nft: meta iifname → ct mark = sandbox_mark
  → MASQUERADE (mark sobrevive en conntrack)
  → export: ulog / nflog / eBPF / proxy-gateway dedicado lee mark → owner_sub
```

Sin gateway dedicado, el audit L7 no existe; solo L3/L4 + mark.

#### C) Visibilidad corporativa (forced egress)

```text
ASP node (atribución ya resuelta)
  → HTTP CONNECT / TLS hacia proxy corporativo OBLIGATORIO (allowlist solo ese hop)
  → identidad: mTLS client cert del *nodo ASP* + cabeceras/claims añadidos en host
     { sandbox_id, owner_sub, tenant_id }
  → SOC ve al empleado sin confiar en el workload
```

Requisito: nft enforce + deny default (ADR-0002/0006). Sin enforce, el guest diala directo y la atribución corporativa se pierde.

## Alternativas consideradas

| Alternativa | Pros | Contras | Decisión |
|---|---|---|---|
| **Confiar en `X-ASP-Sandbox-ID` / marks del guest** | Cero cambios de IP/nft | Guest forja identidad; malware se hace pasar por otro sandbox | **Rechazada** como fuente de verdad |
| **Solo logs de `sandbox_events` (exec)** | Ya existe ADR-0007 | No cubre dials espontáneos del workload ni malware | Insuficiente sola; complementaria |
| **UID / netns humanos dentro del guest** | Familiar en multi-user UNIX | Guest comprometido; sin NET_ADMIN; contradice ADR-0007 | **Rechazada** |
| **Un proxy TCP transparente por sandbox (namespace)** | Atribución trivial por listen | Densidad, fds, ops; duplica forward proxy | Posible en VRF/VLAN; no MVP |
| **SPAN/mirror + DPI offline** | No toca dataplane hot | Caro; atribución post-facto frágil tras NAT compartido | Complemento SOC, no diseño primario |
| **eBPF-only sin proxy** | Elegante L3/L4 | Pierde audit HTTP/SNI fácil; ops más dura (como en ADR-0002) | Futuro; MVP = proxy + IP/mark |
| **IP única por sandbox sin proxy enrich** | Simple en tabla routes | Tras MASQUERADE el destino corporativo no ve la IP guest | Necesita mark, proxy hop, o 1:1 NAT visible |

### Qué no funciona (explícito)

1. **Marks / iptables / nft rules *dentro* del guest** — el workload no confiable las altera o ignora; sin `NET_ADMIN` ni existen.
2. **Headers HTTP puestos por el agente/guest** (`X-ASP-Sandbox-ID`, `X-Forwarded-User`, etc.) como autoridad — forgeables; el proxy debe **borrar** y **re-inyectar** desde lookup host.
3. **Misma IP host `10.200.0.1` en todos los TAP + RemoteAddr guest `10.200.0.2`** sin clave (iif/mark) — ambigüedad entre sandboxes en el proceso proxy si el socket no expone iif.
4. **Atribuir al `actor_sub` del último exec** como dueño del flujo — incorrecto: el sandbox sigue siendo del `owner_sub`; un admin que hace exec no “posee” el malware dial posterior.
5. **SoftFail nft** — sin redirect enforce, bypass = sin atribución en el proxy.

## Consecuencias

### Positivas (si se implementa tras evaluación)

- Misma narrativa humana que ADR-0007 en el **wire**: SIEM puede join `owner_sub` + destino + tiempo.
- Forced egress corporativo con identidad **host-injected** (compliance).
- Encaje natural con proxy + nft ya existentes (ADR-0002/0006).
- Preserva “secretos y claims fuera del guest”.

### Negativas / coste

- *Superado en parte (2026-10): ya hay una `/30` por sandbox.* Quedaba por **romper o evolucionar** el esquema “todos los TAP con `10.200.0.1/24`” → plan de direccionamiento (p.ej. `10.200.0.0/16` con host `.1` común por bridge, o /30 por sandbox, o mark-only sin IP única).
- Node-agent necesita cache `sandbox_id → owner_sub` (reconciler ya ve el sandbox; hoy no lo usa el proxy).
- Raw TCP y UDP/QUIC siguen siendo ciudadanos de segunda: o gateway o solo mark L4.
- Más superficie ops: mapas nft, agotar espacio de marks (32-bit), documentar colisiones.

### Límites honestos (no-goals de este ADR)

- **No implementado** en este cambio de docs; sin flags nuevos ni migraciones.
- **No** promete atribución de cada datagrama UDP arbitrario ni IPv6 hasta que nft/proxy los cubran (ADR-0002 gaps).
- **No** convierte ASP en NDR/XDR; solo etiquetado + audit en el choke point.
- **No** usa el guest como autoridad de identidad de red.
- **No** sustituye allowlist ni deny-by-default.
- Evaluación puede concluir “solo (1)+(2)” y deferir eBPF/VRF sin invalidar el ADR.

## Esquema de implementación previsto (solo mapa — fuera de alcance actual)

| Pieza | Ruta prevista | Cambio futuro |
|---|---|---|
| Direccionamiento TAP | `node-agent/internal/tap/` | IP o mark id por sandbox; API Create con meta |
| Tabla atribución | nuevo p.ej. `node-agent/internal/egress/attrib.go` | `Register(sandboxID, ownerSub, srcKey)` en Start; Unregister en Stop |
| Proxy audit | `egress/forward_proxy.go` | lookup por src/mark; campos `owner_sub`; strip headers guest |
| nft | `nftredirect/` + script | opcional `ct mark` por iifname |
| CP | store ya tiene `owner_sub` | opcional endpoint interno o claim en work item hacia node-agent |
| Docs/smokes | `scripts/smoke-egress-*.sh` | caso “dos sandboxes, mismo destino, logs distintos owner” |

## Criterios de aceptación para una futura implementación

1. Dos sandboxes en el mismo nodo, distinto `owner_sub`, mismo `HTTPS` destino → dos líneas de audit con `owner_sub` distintos **sin** que el guest envíe headers de identidad.
2. Guest que miente `X-ASP-Sandbox-ID` **no** cambia la atribución.
3. Con `--nft-egress-mode=enforce`, intento de bypass al proxy sigue atribuyéndose (redirect) o se deniega — no hay “fuga sin etiqueta”.
4. Documentado: SoftFail / dry-run **no** demuestran atribución en hardware.

## Relación con ADR-0010 (red local)

ADR-0010 no implementa este ADR ni al revés. Con `local_net` apagado el egress sigue el proxy de este diseño. Con `local_net` encendido (propuesta, sin código: [0010](0010-on-demand-local-net.md)) la ruta por defecto de **esa** microVM sale por el túnel del agente local —Internet público y DNS incluidos— y el proxy `:8888` no ve esos flujos. Siguen siendo otro `path` (`local_net`) y la identidad estable sigue siendo `owner_sub`: lookup host-side, headers del guest ignorados. La ruta por defecto **del nodo** (la máquina, y los sandboxes sin el flag) no se sustituye.

## Referencias cruzadas

- Red / proxy / nft: [0002](0002-networking.md), [0006](0006-fase-2e-nft-ssh-guest.md)
- Identidad humana: [0007](0007-multi-user-identity.md)
- Por qué / qué ganamos: [`../why-network-flow-attribution.md`](../why-network-flow-attribution.md)
- Arquitectura § Red: [`../architecture.md`](../architecture.md)
- Roadmap (ítem futuro): [`../roadmap.md`](../roadmap.md)
- Red local (propuesta, path distinto): [0010](0010-on-demand-local-net.md)
