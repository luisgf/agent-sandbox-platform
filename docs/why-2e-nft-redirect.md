# Por qué / Qué ganamos — nftables egress redirect completo (Fase 2e)

Ver también ADR: [`adr/0006-fase-2e-nft-ssh-guest.md`](adr/0006-fase-2e-nft-ssh-guest.md).

## Por qué

En Fase 2d el sketch solo redirigía TCP 80/443 y fallaba en SoftFail sin root.
Un guest comprometido podía:

1. Hablar HTTP(S) por otros puertos (8080, 8443, …) sin pasar por el proxy.
2. Resolver nombres con DNS público (UDP/TCP 53) y dialar IPs directas, bypasseando la allowlist del forward proxy.

Confiar solo en `HTTP_PROXY` del guest sigue siendo voluntario.

## Qué ganamos

- Script completo `scripts/nftables-egress-redirect.sh` con tabla namespaced **`asp_egress`**:
  - Redirect TCP HTTP(S) (`--http-ports`, default `80,443`) → puerto del egress proxy.
  - DNS: `--dns-action redirect` (hacia `--egress-dns-sink`) o `drop`.
  - `dry-run` / `apply` / `flush` idempotentes; dry-run **sin root**.
- Modos claros: **`soft`** (SoftFail, CI/dry-run) vs **`enforce`** (falla si no hay root/`nft`).
- Flags node-agent: `--egress-nft-redirect` / `--nft-egress-redirect`, `--nft-egress-mode=soft|enforce`, `--nft-http-ports`, `--nft-dns-action`.

## Límites honestos

En CI/box sin TAP/KVM real **no** se prueba bypass-proof en hardware. SoftFail cubre lab sin `CAP_NET_ADMIN`. **Enforce** exige: root, binario `nft`, iface TAP con el subnet guest, y proxy/DNS sink escuchando.
