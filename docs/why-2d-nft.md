# Por qué / Qué ganamos — nftables egress redirect (Fase 2d sketch)

> **Actualización Fase 2e:** el sketch se completó (HTTP+DNS, soft|enforce).
> Ver [`why-2e-nft-redirect.md`](why-2e-nft-redirect.md) y ADR-0006.

Ver también ADR: [`adr/0005-fase-2d-hardening.md`](adr/0005-fase-2d-hardening.md).

## Por qué

HTTP_PROXY is voluntary; guest can bypass allowlist.

## Qué ganamos

nftables sketch + --egress-nft-redirect SoftFail forces guest TCP 80/443 through proxy.
