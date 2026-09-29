# ADR-0006: Fase 2e — nft redirect completo + SSH agent auto en guest

- **Estado:** Accepted
- **Fecha:** 2026-09-29
- **Supersede/extends:** addendum operativo a [`0005-fase-2d-hardening.md`](0005-fase-2d-hardening.md) (el sketch nft 2d queda completado aquí)

## Contexto

Tras Fase 2d:

1. El redirect nft solo cubría TCP 80/443 y no DNS; el modo SoftFail era implícito.
2. El mount del SSH agent en el guest seguía siendo manual (socat/virtiofs).

## Decisiones (Por qué / Qué ganamos)

### 1) nftables anti-bypass completo

**Por qué:** HTTP_PROXY es voluntario; DNS directo a resolvers públicos bypasea allowlist.

**Qué ganamos:**

- Tabla `asp_egress` con redirect HTTP(S) (puertos configurables) + DNS redirect/drop.
- Modos `soft` | `enforce`; dry-run sin root; apply/flush idempotentes.
- Integración node-agent: `--nft-egress-redirect`, `--nft-egress-mode`, `--nft-http-ports`, `--nft-dns-action`.

### 2) SSH agent auto via vsock proxy en guest

**Por qué:** sin socket unix estable en el guest, `SSH_AUTH_SOCK` no existe para herramientas.

**Qué ganamos:**

- Binario `vsock-ssh-agent-proxy` + `ssh-agent-vsock.service`.
- Preferencia **vsock** sobre virtiofs (alineado con host-vsock CID 2:26501).
- Dry-run: upstream unix; tests de contrato de protocolo.
- `--guest-ssh-agent-auto` en node-agent.

## Consecuencias

- Enforce nft requiere privilegios de host reales (documentado; CI sigue en soft/dry-run).
- La imagen guest debe incluir el helper; rootfs rebuild necesario para bare-metal.
- Confirm gate (`--ssh-agent-confirm`) sigue aplicando a firmas vía host-vsock/bridge.

## Referencias

- Script: `scripts/nftables-egress-redirect.sh`
- Why: [`../why-2e-nft-redirect.md`](../why-2e-nft-redirect.md), [`../why-2e-ssh-guest-mount.md`](../why-2e-ssh-guest-mount.md)
- Roadmap: [`../roadmap.md`](../roadmap.md) § Fase 2e
