# Imagen guest Debian

Objetivo: Debian stable mínimo, kernel compatible con virtio/vsock y `pod-daemon` como servicio. La imagen debe ser reproducible, inmutable y no contener claves, tokens, compiladores ni credenciales de registro.

## Build OCI

Desde la raíz del repo:

```bash
docker build -t agent-sandbox-guest -f images/guest/Dockerfile .
```

El `Dockerfile`:

- Compila `pod-daemon` (Rust release, `--locked`) y el proxy del agente SSH (Go, `-mod=readonly` con su `go.sum`).
- Instala el binario en `/usr/local/bin/pod-daemon`.
- Instala `systemd` y `systemd-sysv` (`/sbin/init`). Cloud Hypervisor no usa el ENTRYPOINT de Docker; sin init el kernel arranca y `pod-daemon` no llega a ejecutarse.
- Incluye unidades systemd `pod-daemon.service`, **`ssh-agent-vsock.service`** y **`workspace-virtiofs.service`**, más ejemplos OpenRC.
- Binario **`vsock-ssh-agent-proxy`**: unix `/run/agent-sandbox/ssh-agent.sock` ← vsock CID 2:26501.
- **CMD por defecto:** `--listen vsock --vsock-port 26500` (path productivo CH).
- Usuario `sandboxd` (uid 10001); `ASP_HOST_CID=2`; `SSH_AUTH_SOCK=/run/agent-sandbox/ssh-agent.sock`.
- **Quién ejecuta los comandos:** con la unit de systemd, `pod-daemon` corre como root, pero cada comando corre como el dueño de `/workspace` (o como `sandboxd`) salvo que la petición pida `as_root`, con límites y en el cgroup `/sys/fs/cgroup/asp-exec`, y el daemon solo contesta al host. Detalle en [`pod-daemon/README.md`](../../pod-daemon/README.md#quién-ejecuta-un-comando-y-con-qué-límites). El socket del agente SSH es `0666` en `/run/agent-sandbox` (`0755`): los comandos ya no corren como `sandboxd` y lo necesitan; quién puede firmar con qué clave lo decide el node-agent.

## Reproducible: el mismo `rootfs.img` en cada build

Lo que arranca un nodo es `rootfs.img`, y se construye para que dos builds del mismo commit den **los mismos bytes** (y por tanto el mismo hash, que es lo que se publica, se verifica y se atesta):

| Qué podía cambiar | Cómo está fijado |
|---|---|
| Las imágenes base | por digest: `debian:bookworm-slim@sha256:…`, `rust:1-bookworm@sha256:…`, `golang:1.27-bookworm@sha256:…` |
| Los paquetes de Debian | de una **foto del archivo** (`snapshot.debian.org/archive/debian/<SNAPSHOT>/`), no del mirror de hoy. La imagen oficial de Debian dice de qué foto se hizo en su `debian.sources`; el `ARG SNAPSHOT` es esa |
| Los crates y los módulos de Go | `Cargo.lock` con `--locked`, `go.sum` con `-mod=readonly` (ya no hay `go mod tidy` en el build) |
| Rutas y fechas dentro de los binarios | `--remap-path-prefix`, `-trimpath -buildvcs=false -buildid=` |
| Lo que el build deja por el camino | logs, cachés de `ldconfig`/`apt`, copias `/etc/*-` y el «último cambio de contraseña» (`/etc/shadow`) se fijan o se quitan; `/etc/machine-id` es un valor fijo (antes lo sorteaba el postinst de systemd: todas las sandboxes compartían el del build, ahora el mismo en todos los builds) |
| La fecha | `SOURCE_DATE_EPOCH` (por defecto, la hora del commit): es el `mtime` de todos los ficheros y la fecha de creación del ext4 (`E2FSPROGS_FAKE_TIME`) |
| El ext4 | `mke2fs -d` desde un directorio en un **tmpfs** (el orden en que se reparten los ficheros es el del tar), con UUID y `hash_seed` fijos, `lazy_itable_init=0`, sin `orphan_file` (un kernel anterior a 6.5 lo monta también), con el `e2fsprogs` 1.47.2 de Debian trixie (de la misma foto): desde la 1.47.1 `SOURCE_DATE_EPOCH` fija también el `ctime` de cada fichero, que la 1.47.0 de bookworm copiaba del momento de extraerlo. No necesita root ni montar nada |

```bash
scripts/build-guest-image.sh                # build/guest/{rootfs.img,rootfs.img.gz,image.json,SHA256SUMS}
scripts/build-guest-image.sh --verify       # lo construye dos veces desde cero y falla si los hashes difieren
```

`image.json` lleva la versión, el commit, la fecha, la imagen base y la foto, los hashes y tamaños, y **la lista de paquetes con su versión**: para cambiar de paquetes se cambian `SNAPSHOT` y los digests, se construye y se mira el diff de esa lista. Variables: `SOURCE_DATE_EPOCH`, `ASP_ROOTFS_SIZE_MB` (512), `ASP_GUEST_VERSION`, `ASP_GUEST_KERNEL_FILE` (un kernel que publicar con la imagen, como `vmlinux`) y `ASP_GUEST_MODULES` (un `lib/modules/<release>` para meter en la imagen: el kernel de Ubuntu del lab trae vsock como módulo, ver más abajo).

## Instalar una versión en un nodo

Cada versión publica, con el prefijo `asp-guest_<versión>_`, `rootfs.img.gz`, `vmlinux`, `image.json` y `SHA256SUMS`. En el nodo:

```bash
sudo asp image pull --version 0.1.0        # → /var/lib/asp/images/0.1.0, current -> 0.1.0, enlaces en /opt/sandbox
asp image verify /opt/sandbox              # vuelve a comprobar lo instalado contra su SHA256SUMS
```

`asp image pull` baja `SHA256SUMS` y `image.json`, comprueba que se corresponden, descomprime el rootfs **al vuelo** (escribiéndolo disperso, con un tope en el tamaño que dice `image.json`) y lo compara con la suma; **si algo no coincide no instala nada** (ni un fichero, ni el enlace `current`). Lo ya instalado y entero no se baja otra vez. `--base-url` apunta a un espejo, `--no-link` no toca `/opt/sandbox` (un fichero que no sea un enlace nunca se pisa). Las sandboxes en marcha siguen con la imagen con la que arrancaron.

El node-agent comprueba el kernel y la imagen base contra el `SHA256SUMS` que tengan al lado (`--guest-verify`, por defecto `auto`) cada vez que va a arrancar una VM o a clonar un disco: si no coinciden, la sandbox falla con el motivo y no arranca nada de ahí. `sudo asp doctor` y `asp node doctor` lo dicen antes, y `asp node list` muestra el digest de la imagen de cada nodo (`GUEST IMAGE`).

## Identidad de red y hora

El nodo escribe en la línea de comandos del kernel lo que el guest necesita saber de sí mismo y la imagen lo aplica al arrancar:

- **`cmdline-ip.service`** (antes de `network.target` y de pod-daemon) lee `ip=<guest>::<gw>:<mask>:<hostname>:eth0:off:<dns>`: pone la dirección y la ruta por defecto (el kernel del lab no tiene `IP_PNP`), el **hostname** (`asp-<shortid>`; también `/etc/hostname` y `/etc/hosts`) y el **resolver** en `/etc/resolv.conf` (`options timeout:2 attempts:2`). Un campo vacío no se toca. Pruebas: `sh images/guest/helpers/cmdline-ip_test.sh` (`make test-guest-helper`, y en CI).
- **`systemd.setenv=HTTP_PROXY=…`** (y `HTTPS_PROXY`, `NO_PROXY`, en mayúsculas y minúsculas) lo entiende systemd como entorno por defecto de todos los servicios: pod-daemon y, por tanto, los comandos que ejecuta lo heredan.
- **`chrony`** sigue el reloj del host con `refclock PHC /dev/ptp0` (el módulo `ptp_kvm` se carga por `modules-load.d/ptp_kvm.conf`; el árbol `lib/modules` de `ASP_GUEST_MODULES` debe traerlo). No hay servidores de red: el guest no tiene ruta a ninguno. `chronyc tracking` muestra la fuente `PHC0`. Sin esto la hora de un guest que dura días se aleja de la del host y los `exp` de los tokens dejan de ser fiables.
- El usuario **`sandbox`** (uid 1000, con `/home/sandbox`) da nombre al dueño habitual de un workspace; pod-daemon lo usa para `HOME`, `USER` y `LOGNAME` de los comandos.

## Rootfs.img

[`scripts/build-guest-image.sh`](../../scripts/build-guest-image.sh) construye el `rootfs.img` ext4 que usa Cloud Hypervisor (`disks[].path`) a partir de esta imagen. [`scripts/build-guest-rootfs.sh`](../../scripts/build-guest-rootfs.sh) sigue existiendo con su nombre de antes y lo llama.

Con un kernel que trae vsock y virtio como **módulos** (el de Ubuntu del lab), la imagen necesita su `/lib/modules/<release>` (`vsock.ko`, `vmw_vsock_virtio_transport.ko`, `ptp_kvm.ko`; los cargan `modules-load.d/`): `ASP_GUEST_MODULES=/lib/modules/$(uname -r) ASP_GUEST_KERNEL_FILE=… scripts/build-guest-image.sh`. El directorio de módulos debe llamarse como el `uname -r` del kernel del guest. Con un kernel que los trae dentro (compilado para microVMs) no hace falta.

En producción: pin de versión CH/kernel y sin herramientas de build en el guest (no las hay: el compilador está en etapas del build que no pasan a la imagen).

## Guest → host (identidad / SSH)

| Servicio | Guest dial | Host |
|---|---|---|
| pod-daemon HTTP | (host→guest) CONNECT 26500 | CH hybrid UDS |
| SSH agent | AF_VSOCK CID **2** port **26501** → unix `/run/agent-sandbox/ssh-agent.sock` | node-agent `--host-vsock` + guest `ssh-agent-vsock.service` |
| OIDC identity | AF_VSOCK CID **2** port **26502** | node-agent `--host-vsock` |

**Elección (SSH):** proxy vsock en guest (no virtiofs del socket). Ver [ADR-0006](../../docs/adr/0006-fase-2e-nft-ssh-guest.md).

## Workspace del host

`workspace-virtiofs.service` monta el tag virtiofs `workspace` en `/workspace` al boot. El helper sale 0 si el tag no está, así un sandbox sin `workspace_host_path` arranca igual. No bloquea el boot.

Una imagen construida antes de esa unidad no monta sola. Hasta reconstruir el rootfs:

```sh
mkdir -p /workspace && mount -t virtiofs workspace /workspace
```

Detalle: [sesiones](../../docs/ops-asp-session.md#virtiofs-y-pty--qué-aterrizó), [`docs/ops-asp-session.md`](../../docs/ops-asp-session.md).

Lab sin KVM: `ASP_SSH_AGENT_UPSTREAM=unix:/path/to/host-vsock-26501.sock`.

Detalle: [`scripts/guest-vsock-notes.md`](../../scripts/guest-vsock-notes.md).
