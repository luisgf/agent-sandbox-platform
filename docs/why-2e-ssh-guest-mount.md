# Por qué / Qué ganamos — SSH agent mount automatizado en guest (Fase 2e)

Ver también ADR: [`adr/0006-fase-2e-nft-ssh-guest.md`](adr/0006-fase-2e-nft-ssh-guest.md).

## Por qué

El host ya expone el SSH agent en AF_VSOCK CID **2** puerto **26501** (`--host-vsock`) y/o un bridge unix (`--ssh-agent-bridge`). El guest, en cambio, necesitaba un paso **manual** (socat o virtiofs) para tener `SSH_AUTH_SOCK`. Sin eso, las herramientas del agente no pueden firmar con las claves del host.

## Qué ganamos

- Helper **`vsock-ssh-agent-proxy`** en la imagen guest: escucha unix en `/run/agent-sandbox/ssh-agent.sock` y diala vsock `2:26501`.
- Unidad systemd **`ssh-agent-vsock.service`** (+ ejemplo OpenRC) habilitada al construir el rootfs.
- Fallback lab: `ASP_SSH_AGENT_UPSTREAM=unix:/path` (p. ej. `host-vsock-26501.sock`) sin KVM.
- Alternativa in-process: `pod-daemon --ssh-auth-bridge` (mismo contrato).
- Flag node-agent **`--guest-ssh-agent-auto`** (default on cuando hay `--host-vsock` o `--ssh-agent-bridge`).

## Elección: vsock proxy en guest (no virtiofs)

**Por qué vsock:** el plano host→guest ya usa hybrid vsock; guest→host ya usa CID 2. Un proxy unix←vsock en el guest no requiere configurar virtiofs/fs en Cloud Hypervisor ni montajes por sandbox. Menos superficie CH, mismo modelo de puertos documentado.

Virtiofs sigue documentado como opción ops manual (`ssh-agent-{id}.sock` symlink) pero **no** es el path automatizado.
