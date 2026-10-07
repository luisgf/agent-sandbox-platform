# ADR-0002: Red y control de egress

- **Estado:** Aceptada
- **Fecha:** 2026-09
- **Relacionados:** [0005](0005-fase-2d-hardening.md), [0006](0006-fase-2e-nft-ssh-guest.md), [0008](0008-network-flow-attribution.md) (atribución flujos → `owner_sub`, evaluación), [`../why-2e-nft-redirect.md`](../why-2e-nft-redirect.md), [`../bare-metal-ch.md`](../bare-metal-ch.md) §3

## Contexto

Los agentes ejecutan código **no confiable**. Necesitan salida a Internet (git, registries, APIs) pero:

1. Una política aplicada **dentro del guest** puede ser alterada o ignorada por el workload → no es frontera.
2. `HTTP_PROXY` es **voluntario**: malware puede dialar IP:443 directo o resolver con DNS público.
3. Entrada desde Internet a la microVM es inaceptable (superficie de ataque + bypass de tenancy).
4. Corporativo FOSS: preferimos nftables + proxies en userland frente a middleboxes propietarios.

El guest **no recibe `NET_ADMIN`**. Cualquier control real debe vivir en el **host** (node-agent + nft + proxies).

## Decisión

Cada microVM usa **TAP + NAT en el nodo**. Todo egress web y DNS de producción pasa por proxies controlados por el node-agent. Política **deny-by-default**, allowlist por tenant (y, cuando exista, por sandbox).

Capas concretas (de dentro hacia fuera):

1. **TAP por sandbox** — nombre `asp-{shortID}` (8 primeros chars del UUID). Creación automática con `--tap-auto` (SoftFail sin `CAP_NET_ADMIN`).
2. **NAT/MASQUERADE host** — ops manual vía nft (`asp_nat`); da ruta IP mínima hacia el proxy, no “Internet libre”.
3. **Forward proxy HTTP(S)** — `--egress-proxy-listen` (p.ej. `:8888`): CONNECT + HTTP absolutos; deny → **403**; rate-limit token-bucket; audit JSON; MITM **off** por defecto.
4. **DNS sink** — `--egress-dns-sink` (p.ej. `:5353`): NXDOMAIN a nombres no allowlisted.
5. **nft redirect anti-bypass (Fase 2e)** — tabla `asp_egress`: redirige TCP HTTP(S) (puertos configurables) y DNS (redirect|drop) desde el subnet guest hacia el proxy/sink. Modos `soft` | `enforce`.

Allowlist efectiva en el proxy (orden). **Actualizado 2026-10:** la cabecera `X-ASP-Allowlist-JSON` la escribe el guest y permitía `allow-all`; ya no se lee. La cache era una sola por nodo (la del último exec de cualquier tenant); ahora es por sandbox, buscada por IP de origen:

1. Origen en la /30 de un sandbox → la política de su tenant (deny hasta que el nodo la recibe)
2. Origen desconocido → env `ASP_EGRESS_ALLOWLIST_JSON`
3. Default deny del proceso

El control plane guarda reglas por tenant (`PUT /v1/tenants/{id}/egress`).

**Actualizado 2026-10 (entrega de la política):** la política solo llegaba al nodo con cada `POST /v1/sandboxes/{id}/exec`. Hasta el primer exec el guest no salía a ningún sitio aunque su tenant lo permitiera (un agente que arranca solo en la imagen no tenía red), y un cambio de reglas no llegaba a una sandbox en marcha hasta su siguiente exec. Ahora cada sondeo de `GET /v1/nodes/{id}/work` trae `egress`: qué tenant tiene cada sandbox asignada al nodo y la política efectiva de cada tenant (una lectura del store por sondeo) con una `version` (hash del modo y de las reglas activas, sin ids ni orden). El reconciler la aplica al `PolicyCache` antes del primer arranque y otra vez cuando cambia la versión: un `PUT` de reglas llega en el siguiente sondeo (~2 s). El exec sigue adjuntando `egress_allowlist` durante una versión; después se quitará. `POST /v1/internal/egress-check` con `sandbox_id` dice qué decide ahora el proxy para esa sandbox.

## Alternativas consideradas

| Alternativa | Pros | Contras | Decisión |
|---|---|---|---|
| Política solo en guest (iptables guest) | Simple | Guest no confiable | Rechazada |
| eBPF cgroup egress sin proxy | Elegante, L3/L4 | Menos visible para audit HTTP; ops más dura | Futuro posible; MVP = proxy + nft |
| Transparent MITM siempre on | Inspección de contenido TLS | Rompe pinning, riesgo legal/corp, complejidad CA | **Off** por defecto; flag explícito `--egress-mitm` |
| Permitir salida raw TCP allowlisted por IP | Flexible | Bypass de SNI/host policy; dificulta audit | Solo vía excepción documentada / gateway dedicado |
| CNI / Kubernetes NetworkPolicy | Familiar en K8s | Sandboxes no son Pods (ADR-0004) | Fuera de alcance |

## Consecuencias

### Positivas

- La decisión de allow/deny y el audit viven **fuera** del guest.
- SoftFail en TAP/nft permite CI verde sin root; Enforce documenta el camino bare-metal real.
- Separar “check API” (`/v1/internal/egress-check`) del “proxy en wire” permite smokes incrementales.

### Negativas

- Ops debe cablear NAT + (idealmente) nft redirect; sin eso, `HTTP_PROXY` sigue siendo voluntario.
- HTTPS por CONNECT se filtra por host/SNI/destino; inspeccionar bytes TLS exige MITM consciente.
- Protocolos no-HTTP (SSH directo, gRPC raw, UDP arbitrario) necesitan gateway dedicado o excepción explícita — **nunca** “abrir el firewall”.
- El proxy es infraestructura crítica: HA local, límites de body, redacción de secretos en logs.

### Follow-ups

- Completado en 2e: puertos HTTP configurables + DNS + soft|enforce (ADR-0006).
- Pendiente: bypass-proof medido en hardware real (no solo dry-run del script); IPv6; UDP no-DNS.
- Pendiente (evaluación): atribución de flujos a `owner_sub` — [0008](0008-network-flow-attribution.md).

## Detalle de implementación en este repo

| Pieza | Ruta / flag |
|---|---|
| Allowlist | `node-agent/internal/egress/` (`Allowlist`, `Check`, `PolicyCache`, `ForwardProxy`, `DNSSink`) |
| MITM CA | `node-agent/internal/egress/mitm/` — solo con `--egress-mitm` |
| Rate limit | `node-agent/internal/egress/ratelimit.go` |
| TAP manager | `node-agent/internal/tap/` + `--tap-auto` / `ASP_TAP_AUTO=1` |
| nft enforcer | `node-agent/internal/nftredirect/` + `scripts/nftables-egress-redirect.sh` |
| Flags nft | `--nft-egress-redirect`, `--nft-egress-mode=soft\|enforce`, `--nft-http-ports`, `--nft-dns-action`, `--guest-subnet` |
| Proxy listen | `--egress-proxy-listen`, `--egress-dns-sink`, `--egress-enforce` |
| CP egress API | `PUT/GET /v1/tenants/{id}/egress`, `POST …/egress/check` |
| Migración | `control-plane/migrations/003_tenant_egress.sql` |
| Defaults CP | `ASP_EGRESS_DENY_DEFAULT=1` (prod); memory-dev puede auto-set `ASP_EGRESS_DEFAULT_ALLOW=1` |
| Smoke | `scripts/smoke-egress-proxy.sh`, `scripts/smoke-identity-egress.sh` |

Subnet por defecto del sketch: **`10.200.0.0/16`** (con `--tap-auto`, una /30 por sandbox: TAP `.1`, guest `.2`).

## Límites honestos / no-goals

- **SoftFail ≠ Enforce.** Sin root/`nft`, el enforcer avisa y sigue; eso **no** prueba anti-bypass.
- nft `asp_egress` cubre TCP HTTP(S) configurables + DNS UDP/TCP 53. **No** cubre IPv6, ni “todos los puertos del mundo”, ni QUIC/UDP arbitrario.
- MITM desactivado por defecto; activarlo es decisión corporativa explícita.
- NAT (`asp_nat`) sigue siendo **ops manual** en bare-metal; `--tap-auto` no configura MASQUERADE solo.
- No exponer microVMs directamente a Internet (sin DNAT de entrada).

## Enmiendas

- **2026-10 (#98):** el proxy comprobaba solo el nombre contra la allowlist y marcaba lo que resolviera. Ahora comprueba la dirección a la que va a conectar, tras resolver, y nunca marca loopback, link-local, multicast, reservadas, las direcciones del nodo ni la red de los guests; las redes privadas solo las abre el operador (`--egress-allow-cidr`). Una regla sin puerto vale para 80 y 443, no para todos.

## Referencias cruzadas

- Hardening nft 2d/2e: ADR-0005, ADR-0006
- Why: [`../why-2e-nft-redirect.md`](../why-2e-nft-redirect.md)
- Arquitectura § Red: [`../architecture.md`](../architecture.md)
- Ops: [`../bare-metal-ch.md`](../bare-metal-ch.md) §3, §8e
