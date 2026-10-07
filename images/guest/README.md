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
- Instala `systemd` y `systemd-sysv` (`/sbin/init`). Cloud Hypervisor no usa el ENTRYPOINT de Docker; sin init el kernel arranca y `pod-daemon` no llega a ejecutarse.
- Incluye unidades systemd `pod-daemon.service`, **`ssh-agent-vsock.service`** y **`workspace-virtiofs.service`**, más ejemplos OpenRC.
- Binario **`vsock-ssh-agent-proxy`**: unix `/run/agent-sandbox/ssh-agent.sock` ← vsock CID 2:26501.
- **CMD por defecto:** `--listen vsock --vsock-port 26500` (path productivo CH).
- Usuario `sandboxd` (uid 10001); `ASP_HOST_CID=2`; `SSH_AUTH_SOCK=/run/agent-sandbox/ssh-agent.sock`.
- **Quién ejecuta los comandos:** con la unit de systemd, `pod-daemon` corre como root, pero cada comando corre como el dueño de `/workspace` (o como `sandboxd`) salvo que la petición pida `as_root`, con límites y en el cgroup `/sys/fs/cgroup/asp-exec`, y el daemon solo contesta al host. Detalle en [`pod-daemon/README.md`](../../pod-daemon/README.md#quién-ejecuta-un-comando-y-con-qué-límites). El socket del agente SSH es `0666` en `/run/agent-sandbox` (`0755`): los comandos ya no corren como `sandboxd` y lo necesitan; quién puede firmar con qué clave lo decide el node-agent.

## Identidad de red y hora

El nodo escribe en la línea de comandos del kernel lo que el guest necesita saber de sí mismo y la imagen lo aplica al arrancar:

- **`cmdline-ip.service`** (antes de `network.target` y de pod-daemon) lee `ip=<guest>::<gw>:<mask>:<hostname>:eth0:off:<dns>`: pone la dirección y la ruta por defecto (el kernel del lab no tiene `IP_PNP`), el **hostname** (`asp-<shortid>`; también `/etc/hostname` y `/etc/hosts`) y el **resolver** en `/etc/resolv.conf` (`options timeout:2 attempts:2`). Un campo vacío no se toca. Pruebas: `sh images/guest/helpers/cmdline-ip_test.sh` (`make test-guest-helper`, y en CI).
- **`systemd.setenv=HTTP_PROXY=…`** (y `HTTPS_PROXY`, `NO_PROXY`, en mayúsculas y minúsculas) lo entiende systemd como entorno por defecto de todos los servicios: pod-daemon y, por tanto, los comandos que ejecuta lo heredan.
- **`chrony`** sigue el reloj del host con `refclock PHC /dev/ptp0` (el módulo `ptp_kvm` se carga por `modules-load.d/ptp_kvm.conf`; el árbol `lib/modules` de `ASP_GUEST_MODULES` debe traerlo). No hay servidores de red: el guest no tiene ruta a ninguno. `chronyc tracking` muestra la fuente `PHC0`. Sin esto la hora de un guest que dura días se aleja de la del host y los `exp` de los tokens dejan de ser fiables.
- El usuario **`sandbox`** (uid 1000, con `/home/sandbox`) da nombre al dueño habitual de un workspace; pod-daemon lo usa para `HOME`, `USER` y `LOGNAME` de los comandos.

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
