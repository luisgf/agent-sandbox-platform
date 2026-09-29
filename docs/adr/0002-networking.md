# ADR-0002: Red y control de egress

- **Estado:** Accepted
- **Fecha:** 2026-09-29

## Contexto

Los agentes ejecutan código no confiable y necesitan acceso saliente limitado. Una política configurada dentro del guest puede ser alterada por el workload y no es una frontera válida.

## Decisión

Cada microVM usa **TAP + NAT en el nodo**. Todo egress web y DNS pasa obligatoriamente por un **proxy HTTP(S) y un proxy/resolver DNS** controlados por el nodo. La política es **deny-by-default**, con allowlist por tenant y, cuando proceda, por sandbox. El guest **no recibe `NET_ADMIN`**.

El nodo impide rutas alternativas al proxy mediante reglas host-side; no se habilita entrada directa desde Internet.

## Consecuencias

- Las políticas se aplican y auditan fuera del guest.
- HTTPS por CONNECT puede filtrarse por host/SNI y destino; inspeccionar contenido TLS exige una decisión posterior explícita.
- Protocolos no HTTP necesitan gateways dedicados o una excepción de política explícita, nunca salida arbitraria.
- DNS debe bloquear resolvers externos, rebinding y respuestas hacia rangos prohibidos.
- Node-agent necesita reconciliación idempotente de TAP, NAT, nftables/eBPF y cleanup tras fallos.
- El proxy es infraestructura crítica: requiere HA local, límites, métricas y protección contra secretos en logs.
