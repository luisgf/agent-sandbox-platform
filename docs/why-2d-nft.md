# Por qué / Qué ganamos — nftables egress redirect (Fase 2d sketch)

> **Actualización Fase 2e:** el sketch se completó (HTTP+DNS, soft|enforce).
> Canon: [`why-2e-nft-redirect.md`](why-2e-nft-redirect.md) y [ADR-0006](adr/0006-fase-2e-nft-ssh-guest.md).
> Este archivo conserva el “por qué” original de 2d para el historial del roadmap.

Ver también ADR: [`adr/0005-fase-2d-hardening.md`](adr/0005-fase-2d-hardening.md).

## Por qué

`HTTP_PROXY` / `HTTPS_PROXY` en el guest son **voluntarios**. Un proceso malicioso (o simplemente una herramienta que ignora el proxy) puede:

1. Abrir TCP directo a `1.2.3.4:443`.
2. Hablar HTTP en puertos no estándar.
3. Resolver nombres por DNS público y dialar la IP — la allowlist del forward proxy nunca ve el hostname.

Confiar solo en la buena fe del guest contradice el threat model (guest = untrusted).

## Qué ganamos en el sketch 2d

- Script `scripts/nftables-egress-redirect.sh` (dry-run / apply / remove).
- Flag node-agent `--egress-nft-redirect` con **SoftFail** sin root/`nft` (CI verde).
- Redirección de TCP **80/443** del subnet guest hacia el puerto del forward proxy.

## Qué quedó incompleto (pagado en 2e)

- Sin DNS redirect/drop.
- Sin puertos HTTP configurables.
- SoftFail implícito vs modo **enforce** explícito.

Usa 2e en despliegues nuevos: `--egress-nft-redirect --nft-egress-mode=enforce` (bare-metal) o `soft` (lab).
