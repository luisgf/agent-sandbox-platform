# Por qué / Qué ganamos — SSH agent mount automatizado en guest (Fase 2e)

Ver también ADR: [`adr/0006-fase-2e-nft-ssh-guest.md`](adr/0006-fase-2e-nft-ssh-guest.md). Identidad: [ADR-0003](adr/0003-identity.md).

## Por qué

El host ya expone el SSH agent en AF_VSOCK CID **2** puerto **26501** (`--host-vsock`) y/o un bridge unix (`--ssh-agent-bridge`). El guest, en cambio, necesitaba un paso **manual** (socat o virtiofs) para tener `SSH_AUTH_SOCK`. Sin eso:

1. `git` / `ssh` / herramientas del agente fallan con “Could not open a connection to your authentication agent”.
2. Cada imagen o cloud-init reinventaba el mismo one-liner frágil.
3. Virtiofs del sock host implica configurar el filesystem en Cloud Hypervisor **por sandbox**.

## Qué ganamos

- Helper **`vsock-ssh-agent-proxy`** (`images/guest/cmd/vsock-ssh-agent-proxy/`): escucha unix en `/run/agent-sandbox/ssh-agent.sock` y diala vsock `2:26501`.
- Unidad systemd **`ssh-agent-vsock.service`** (+ ejemplo OpenRC) habilitada al construir el rootfs (`scripts/build-guest-rootfs.sh`).
- Fallback lab: `ASP_SSH_AGENT_UPSTREAM=unix:/path` (p. ej. `host-vsock-26501.sock`) sin KVM.
- Alternativa in-process: `pod-daemon --ssh-auth-bridge` (mismo contrato de path).
- (La bandera `--guest-ssh-agent-auto` solo escribía una línea en el log: ya no hace nada y avisa. Que `ssh-agent-vsock.service` corra o no lo decide la imagen del guest.)
- El **confirm gate** (`--ssh-agent-confirm`, Fase 2d) sigue aplicando a firmas vía este path.

## Elección: vsock proxy en guest (no virtiofs)

| Criterio | Vsock proxy guest | Virtiofs sock host |
|---|---|---|
| Config CH extra | No | Sí (fs mount por VM) |
| Alineado a 26501 | Sí (mismo puerto) | N/A |
| Dry-run sin KVM | `ASP_SSH_AGENT_UPSTREAM=unix:…` | Montaje artificial |
| Superficie | Un binario pequeño en imagen | Dependencia del VMM fs |

**Decisión:** auto = vsock. Virtiofs queda documentado como alternativa ops manual (`ssh-agent-{id}.sock` symlink en el host), no como happy path.

## Límites honestos

- Hace falta **rebuild del rootfs** para hosts que aún no tienen el helper en la imagen.
- Sin `--host-vsock` ni bridge, el flag auto solo advierte: el guest no tiene a quién dialar.
- FakeAgent (0 keys) en lab sin `SSH_AUTH_SOCK` host valida protocolo, no git real.
- Con Cloud Hypervisor, `--host-vsock` debe adjuntar listeners hybrid `{vsock}_26501`
  (no basta AF_VSOCK Listen). Ver [`why-ch-hybrid-guest-host.md`](why-ch-hybrid-guest-host.md).
