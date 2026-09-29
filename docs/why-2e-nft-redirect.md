# Por qué / Qué ganamos — nftables egress redirect completo (Fase 2e)

Ver también ADR: [`adr/0006-fase-2e-nft-ssh-guest.md`](adr/0006-fase-2e-nft-ssh-guest.md). Sketch previo: [`why-2d-nft.md`](why-2d-nft.md).

## Por qué

En Fase 2d el sketch solo redirigía TCP 80/443 y fallaba en SoftFail sin root.
Un guest comprometido podía:

1. Hablar HTTP(S) por otros puertos (8080, 8443, …) sin pasar por el proxy.
2. Resolver nombres con DNS público (UDP/TCP 53) y dialar IPs directas, bypasseando la allowlist del forward proxy.

Confiar solo en `HTTP_PROXY` del guest sigue siendo voluntario. La frontera real tiene que vivir en el **host** (ADR-0002).

## Qué ganamos

- Script completo `scripts/nftables-egress-redirect.sh` con tabla namespaced **`asp_egress`**:
  - Redirect TCP HTTP(S) (`--http-ports`, default `80,443`) → puerto del egress proxy.
  - DNS: `--dns-action redirect` (hacia `--egress-dns-sink`) o `drop`.
  - `dry-run` / `apply` / `flush` idempotentes; dry-run **sin root**.
- Modos claros: **`soft`** (SoftFail, CI/dry-run) vs **`enforce`** (falla si no hay root/`nft`).
- Integración node-agent: `--egress-nft-redirect` / `--nft-egress-redirect`, `--nft-egress-mode=soft|enforce`, `--nft-http-ports`, `--nft-dns-action`, `--guest-subnet`.
- Paquete Go testeable: `node-agent/internal/nftredirect` (`DryRun` renderiza reglas; `Apply` SoftFail vs Enforce).

### Ejemplo lab (soft)

```bash
node-agent ... --egress-proxy-listen=:8888 --egress-dns-sink=:5353 \
  --nft-egress-redirect --nft-egress-mode=soft --guest-subnet=10.200.0.0/16
```

### Ejemplo bare-metal (enforce)

```bash
sudo -E node-agent ... --egress-enforce \
  --egress-proxy-listen=0.0.0.0:8888 --egress-dns-sink=0.0.0.0:5353 \
  --nft-egress-redirect --nft-egress-mode=enforce \
  --nft-http-ports=80,443,8080 --nft-dns-action=redirect
```

Inspección sin aplicar:

```bash
./scripts/nftables-egress-redirect.sh dry-run \
  --guest-subnet 10.200.0.0/16 --proxy-port 8888 --dns-sink-port 5353
```

## Límites honestos

- En CI/box sin TAP/KVM real **no** se prueba bypass-proof en hardware. SoftFail cubre lab sin `CAP_NET_ADMIN`.
- **Enforce** exige: root, binario `nft`, iface/subnet guest coherente, y proxy/DNS sink escuchando.
- No cubre IPv6, QUIC/UDP arbitrario, ni “todos los puertos del mundo” — solo la lista `--nft-http-ports` + DNS 53.
- SoftFail mal leído como “ya estamos seguros” es un anti-patrón: mira logs y usa enforce en prod.
