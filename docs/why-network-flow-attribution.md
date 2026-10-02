# Por qué / Qué ganamos — Atribución de flujos de red a `owner_sub`

Ver también ADR: [`adr/0008-network-flow-attribution.md`](adr/0008-network-flow-attribution.md) (**propuesta / aceptada para evaluación — no implementada**).  
Complementa la identidad humana de [`adr/0007-multi-user-identity.md`](adr/0007-multi-user-identity.md) y la frontera de egress de [`adr/0002-networking.md`](adr/0002-networking.md).

## Por qué

Con ADR-0007 el auditor ya puede preguntar *quién creó este sandbox* y *quién lanzó este exec*. Eso vive en el **plano de control** (`owner_sub`, `actor_sub`, `user_sub` en OIDC).

En cuanto el workload **abre una conexión** (git, npm, API corporativa, C2…), la pregunta cambia:

- ¿Este HTTPS hacia `api.empresa.com` es del sandbox de **Alice** o del de **Bob** en el mismo nodo?
- ¿El SOC puede ligar el alert del proxy/firewall al **empleado**, no solo al IP del hypervisor tras NAT?
- ¿Podemos **forzar** egress por el proxy corporativo con identidad inyectada por ASP, sin confiar en el guest?

Hoy la respuesta honesta es débil:

1. El forward proxy puede loguear `sandbox_id` si alguien manda `X-ASP-Sandbox-ID` — **forgeable** desde el guest.
2. Todos los TAP usan por defecto el mismo CIDR host (`10.200.0.1/24`) → no hay IP global única por sandbox.
3. nft `asp_egress` fuerza (en enforce) el camino al proxy, pero **no etiqueta** el flujo con `owner_sub`.
4. `sandbox_events` no ve dials espontáneos del malware ni del agente autónomo.

Sin atribución en el wire, multi-user + egress deny-by-default sigue siendo “seguro en allowlist” pero **ciego en responsabilidad humana**.

## Qué ganamos (diseño objetivo)

- **Mapa host-side** `flujo → sandbox_id → owner_sub` con señales que el guest **no** elige (IP/TAP, iif, `ct mark`).
- **Camino natural:** enriquecer el egress proxy (y DNS sink) que ya es choke point HTTP(S) — audit JSON con `owner_sub` / `tenant_id`.
- **Forced egress corporativo:** el hop ASP → proxy empresa lleva identidad **inyectada en el host** (header limpio o mTLS de nodo + claims), no cabeceras del workload.
- **Misma historia que ADR-0007:** el humano dueño del sandbox es quien aparece en red; coherente con `user_sub` del JWT de workload.
- **Base para raw TCP:** marks nft / eBPF como follow-up cuando no hay HTTP.

## Qué no ganamos (límites honestos)

- **Nada de esto está implementado** aún — solo ADR + esta narrativa para evaluar.
- SoftFail nft / dry-run **no** demuestran atribución real.
- UDP/QUIC/IPv6 siguen en los gaps de ADR-0002 hasta que el dataplane los cubra.
- `actor_sub` de un exec **≠** dueño de cada socket posterior; la atribución estable de egress es `owner_sub`.
- No sustituye allowlist, MITM consciente, ni NDR comercial.
- Guest-set marks / headers / UIDs **siguen rechazados** como autoridad.

## Cómo encaja con lo que ya hay

| Pieza actual | Sigue igual | Cambia (si se implementa) |
|---|---|---|
| Deny-by-default + proxy + nft | sí | proxy enriquece audit con `owner_sub` |
| TAP `asp-{shortID}` | sí | IP o mark único por sandbox |
| `owner_sub` en store (0007) | sí | se propaga al node-agent / proxy cache |
| Header `X-ASP-Sandbox-ID` | existe | deja de ser fuente de verdad (strip + re-inject) |
| OIDC `user_sub` | sí | red cuenta la misma historia en logs |
| Guest sin NET_ADMIN / sin claims | sí | tampoco elige identidad de red |

## Lectura rápida del modelo

```text
Humano (owner_sub) ──create──► sandbox
                                 │
                    Start/TAP ───┼──► srcKey (IP o mark) en node-agent
                                 │
guest dial ──► nft enforce ──► proxy lookup(srcKey) ──► audit{owner_sub}
                                 │
                                 └──► (opcional) corporate proxy + identidad host-injected
```

## Orden de evaluación sugerido

1. **Proxy attribution + IP/TAP (o mark) por sandbox** — MVP creíble.
2. **nft `ct mark` por iifname** — raw TCP / post-NAT.
3. **VLAN/VRF o eBPF** — solo si corporate/ops lo exige.

Detalle normativo, alternativas y criterios de aceptación: el ADR-0008.
