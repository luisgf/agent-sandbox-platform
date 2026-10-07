# ADR-0006: Fase 2e — nft redirect completo + SSH agent auto en guest

- **Estado:** Aceptada
- **Fecha:** 2026-09
- **Extiende / supersede operativo:** el sketch nft del punto 4 de [0005](0005-fase-2d-hardening.md); alinea identidad guest con [0003](0003-identity.md)

## Contexto

Tras Fase 2d:

1. El redirect nft solo cubría **TCP 80/443** y el modo SoftFail era implícito. Un guest podía:
   - Hablar HTTP(S) por 8080/8443/… sin proxy.
   - Resolver con DNS público (UDP/TCP 53) y dialar IPs crudas → bypass de allowlist por nombre.
2. El mount del SSH agent en el guest seguía siendo **manual** (socat ad-hoc o virtiofs). Sin `SSH_AUTH_SOCK` estable, las herramientas del agente no firman con las claves del host aunque `--host-vsock` esté up.

Objetivo de 2e: cerrar esos dos gaps sin romper CI (sin root/KVM).

## Decisiones

### 1) nftables anti-bypass completo

**Por qué:** ADR-0002 — la frontera es host-side; HTTP_PROXY es voluntario; DNS directo es el bypass clásico.

**Qué decidimos:**

- Tabla namespaced **`asp_egress`** generada por `scripts/nftables-egress-redirect.sh`:
  - Redirect TCP de `--http-ports` (default `80,443`) desde `--guest-subnet` hacia el puerto del egress proxy.
  - DNS: `--dns-action redirect` (hacia DNS sink) **o** `drop`.
  - Acciones CLI: `dry-run` / `apply` / `flush` idempotentes; dry-run **sin root**.
- Modos explícitos: **`soft`** (SoftFail: warn + exit 0) vs **`enforce`** (falla sin root/`nft`).
- Integración node-agent: `--egress-nft-redirect` (el alias `--nft-egress-redirect` se retiró), `--nft-egress-mode`, `--nft-http-ports`, `--nft-dns-action`, `--guest-subnet`.
- Paquete Go: `node-agent/internal/nftredirect` (`Apply`/`Flush`/`DryRun`, `ModeSoft`/`ModeEnforce`).

### 2) SSH agent auto vía vsock proxy en guest

**Por qué:** el host ya expone el agent en CID 2:26501; falta el último metro en el guest.

**Qué decidimos:**

- Binario **`vsock-ssh-agent-proxy`**: escucha unix `/run/agent-sandbox/ssh-agent.sock` y diala vsock `2:26501`.
- Unidad **`ssh-agent-vsock.service`** (+ ejemplo OpenRC) en `images/guest/`, habilitada al build del rootfs.
- Preferencia **vsock** sobre virtiofs (menos superficie CH, mismo mapa de puertos).
- Lab sin KVM: `ASP_SSH_AGENT_UPSTREAM=unix:/path/to/host-vsock-26501.sock`.
- Flag node-agent **`--guest-ssh-agent-auto`** (retirado, #126): solo escribía una línea en el log; que la imagen monte el agente lo decide la imagen.
- Con Cloud Hypervisor, el path productivo guest→host es **`AttachSandbox`** → `{vsock}_{26501}` (no basta AF_VSOCK Listen); ver why hybrid.
- El confirm gate de 2d (`--ssh-agent-confirm`) **sigue aplicando** a firmas que llegan por host-vsock/bridge.

## Alternativas consideradas

| Tema | Alternativa | Pros | Contras | Decisión |
|---|---|---|---|---|
| Anti-bypass | Solo eBPF cgroup | Potente | Ops/debug más duro en MVP | Futuro; nft primero |
| DNS | Forzar DoH solo vía proxy | Moderno | Apps que hablan UDP/53 directo siguen existiendo | Redirect/drop UDP/TCP 53 |
| SSH mount | Virtiofs del sock host | Menos proceso guest | Config CH + mount por sandbox; frágil | Manual OK; auto = vsock |
| SSH mount | socat one-shot en cloud-init | Rápido de documentar | No reproducible en imagen | Rechazado como path primary |
| Enforce default | `enforce` en todos lados | Más seguro | Rompe CI/box | Default **`soft`**; enforce en bare-metal docs |

## Consecuencias

### Positivas

- Camino documentado a “el guest no puede saltarse el proxy” en hosts con privilegios reales.
- `SSH_AUTH_SOCK` estable out-of-the-box en la imagen guest.
- Dry-run del script nft + tests Go mantienen el contrato en CI.
- Virtiofs queda como escape hatch ops, no como dependencia del happy path.

### Negativas

- Enforce nft exige root, binario `nft`, TAP/subnet correctos y proxy/sink escuchando — fallar en silencio con SoftFail es un riesgo ops si nadie mira logs.
- Rebuild de rootfs necesario para pillar el helper en bare-metal ya desplegado.
- Redirect no cubre IPv6 / QUIC / puertos no listados.

### Follow-ups

- Medición bypass-proof en lab KVM real (checklist bare-metal §8e).
- Posible ampliar `--nft-http-ports` por tenant (hoy: flag de nodo).
- CLI `asp` ya existe (2f) para ejercitar lifecycle sin curl.

## Detalle de implementación en este repo

| Pieza | Ruta / flag |
|---|---|
| Script nft | `scripts/nftables-egress-redirect.sh` |
| Enforcer | `node-agent/internal/nftredirect/enforcer.go` |
| Flags | `--egress-nft-redirect`, `--nft-egress-mode=soft\|enforce`, `--nft-http-ports=80,443`, `--nft-dns-action=redirect\|drop`, `--guest-subnet=10.200.0.0/16` |
| Guest proxy | `images/guest/cmd/vsock-ssh-agent-proxy/` |
| Systemd unit | `images/guest/systemd/ssh-agent-vsock.service` (y OpenRC bajo `images/guest/openrc/`) |
| Rootfs build | `scripts/build-guest-rootfs.sh` |
| Guest README | `images/guest/README.md` |
| Flag auto | retirado (#126): `--guest-ssh-agent-auto` / `ASP_GUEST_SSH_AGENT_AUTO` se aceptan, avisan y no hacen nada |
| Why | [`../why-2e-nft-redirect.md`](../why-2e-nft-redirect.md), [`../why-2e-ssh-guest-mount.md`](../why-2e-ssh-guest-mount.md), [`../why-ch-hybrid-guest-host.md`](../why-ch-hybrid-guest-host.md) |
| Tests | `nftredirect/enforcer_test.go`, `vsock-ssh-agent-proxy/proxy_test.go`, `sshagent/guest_mount_test.go` |

Ejemplo bare-metal (enforce):

```bash
node-agent ... \
  --egress-enforce --egress-proxy-listen=0.0.0.0:8888 --egress-dns-sink=0.0.0.0:5353 \
  --egress-nft-redirect --nft-egress-mode=enforce \
  --nft-http-ports=80,443,8080 --nft-dns-action=redirect \
  --host-vsock
```

**Actualizado 2026-10 (#123):** el redirect pasa a estar **activado por defecto** cuando el nodo tiene `--egress-proxy-listen` y no es `--dry-run`, y su modo por defecto es `enforce`: «deny by default» ya no depende de que el operador ponga dos flags más. `soft` queda para `--dry-run` y para quien lo pida (con aviso: el nodo arranca sin forzar el egress). El script ya no se instala en el host: va embebido en el binario (`ASP_NFT_SCRIPT` nombra uno propio). El nodo informa `egress_enforced` al registrarse (migración 021) y `asp node list` lo muestra. Esto cambia el comportamiento de un nodo con proxy y sin los flags nft: antes arrancaba sin reglas, ahora las aplica o no arranca; quien no quiera el redirect pone `--egress-nft-redirect=false`.

Ejemplo CI/lab (soft):

```bash
node-agent ... --dry-run --egress-proxy-listen=:8888 \
  --egress-nft-redirect --nft-egress-mode=soft
# Apply SoftFail sin root; DryRun del script sigue siendo testeable
```

## Límites honestos / no-goals

- SSH auto diala CID 2:26501; en CH el host debe tener **`AttachSandbox`** (`{muxer}_26501`) o el guest ve RST.
- **Sin TAP/KVM en CI no se prueba bypass-proof en hardware.** SoftFail mantiene verde el pipeline; no sustituye el checklist bare-metal.
- SoftFail mal interpretado como “ya estamos seguros” es un anti-patrón — los docs deben decirlo en voz alta.
- Virtiofs SSH **no** se automatiza; solo se documenta.
- Este ADR no añade TPM/SEV ni Windows guests.

## Referencias cruzadas

- Sketch previo: [0005](0005-fase-2d-hardening.md)
- Identidad: [0003](0003-identity.md) · Red: [0002](0002-networking.md)
- Roadmap §2e: [`../roadmap.md`](../roadmap.md)
- Bare-metal §8e: [`../bare-metal-ch.md`](../bare-metal-ch.md)
- Diagrama (nft + ssh-agent-vsock): [`../diagram.svg`](../diagram.svg)
