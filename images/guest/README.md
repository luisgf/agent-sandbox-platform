# Imagen guest Debian

Objetivo: Debian stable mínimo, kernel compatible con virtio/vsock y `pod-daemon` como servicio. La imagen debe ser reproducible, inmutable y no contener claves, tokens, compiladores ni credenciales de registro.

## Build OCI

Desde la raíz del repo:

```bash
docker build -t agent-sandbox-guest -f images/guest/Dockerfile .
```

El `Dockerfile`:

- Compila `pod-daemon` (Rust release).
- Instala el binario en `/usr/local/bin/pod-daemon`.
- Incluye unidades systemd `pod-daemon.service`, **`ssh-agent-vsock.service`** y **`workspace-virtiofs.service`**, más ejemplos OpenRC.
- Binario **`vsock-ssh-agent-proxy`**: unix `/run/agent-sandbox/ssh-agent.sock` ← vsock CID 2:26501.
- **CMD por defecto:** `--listen vsock --vsock-port 26500` (path productivo CH).
- Usuario `sandboxd` (uid 10001); `ASP_HOST_CID=2`; `SSH_AUTH_SOCK=/run/agent-sandbox/ssh-agent.sock`.

## Rootfs.img

Ver [`scripts/build-guest-rootfs.sh`](../../scripts/build-guest-rootfs.sh) para exportar la imagen OCI a un `rootfs.img` ext4 usable por Cloud Hypervisor (`disks[].path`).

En producción: firmar el rootfs, pin de versión CH/kernel, y sin herramientas de build en el guest.

## Guest → host (identidad / SSH)

| Servicio | Guest dial | Host |
|---|---|---|
| pod-daemon HTTP | (host→guest) CONNECT 26500 | CH hybrid UDS |
| SSH agent | AF_VSOCK CID **2** port **26501** → unix `/run/agent-sandbox/ssh-agent.sock` | node-agent `--host-vsock` + guest `ssh-agent-vsock.service` |
| OIDC identity | AF_VSOCK CID **2** port **26502** | node-agent `--host-vsock` |

**Elección (SSH):** proxy vsock en guest (no virtiofs del socket). Ver [`docs/why-2e-ssh-guest-mount.md`](../../docs/why-2e-ssh-guest-mount.md).

## Workspace del host

`workspace-virtiofs.service` monta el tag virtiofs `workspace` en `/workspace` al boot. El helper sale 0 si el tag no está, así un sandbox sin `workspace_host_path` arranca igual. No bloquea el boot.

Una imagen construida antes de esa unidad no monta sola. Hasta reconstruir el rootfs:

```sh
mkdir -p /workspace && mount -t virtiofs workspace /workspace
```

Detalle: [`docs/why-virtiofs-pty.md`](../../docs/why-virtiofs-pty.md), [`docs/ops-asp-session.md`](../../docs/ops-asp-session.md).

Lab sin KVM: `ASP_SSH_AGENT_UPSTREAM=unix:/path/to/host-vsock-26501.sock`.

Detalle: [`scripts/guest-vsock-notes.md`](../../scripts/guest-vsock-notes.md).
