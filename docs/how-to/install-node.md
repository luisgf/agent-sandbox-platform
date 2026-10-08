# Instalar un nodo a mano

Un **nodo** es un servidor con KVM que ejecuta las sandboxes como microVMs de Cloud Hypervisor. [El instalador](install.md) lo deja listo con un comando (`INSTALL_ASP_ROLE=agent`: los paquetes, Cloud Hypervisor, la imagen del guest y el servicio); esta guía son los mismos pasos a mano, para quien no lo usa o quiere saber qué toca. El plano de control se instala aparte ([instalar el plano de control](install-control-plane.md)), o todo en un solo host con [`asp-server`](single-host.md).

> **Sin `/dev/kvm`** (CI, un contenedor, un portátil): no hace falta arrancar Cloud Hypervisor para probar ASP. Un nodo con `--dry-run` usa un VMM de mentira y ejercita todo menos el aislamiento ([`mvp-smoke.md`](../mvp-smoke.md)). Esta guía es para el host con KVM.

## 1. El host

### Hardware y virtualización

| Requisito | Comprobación |
|---|---|
| CPU VT-x (Intel) o AMD-V | `grep -E 'vmx\|svm' /proc/cpuinfo` |
| Módulo KVM cargado | `lsmod \| grep kvm` |
| `/dev/kvm` usable | `ls -l /dev/kvm` (grupo `kvm`, modo `rw` para el usuario del node-agent) |
| `kvm-ok` (Ubuntu) | `sudo apt install cpu-checker && kvm-ok` |

**Nested virtualización:** si el host ASP es a su vez una VM (p. ej. lab en cloud), habilita nested en el hipervisor padre (`kvm-intel.nested=1` / `kvm-amd.nested=1`). Nested funciona para desarrollo; densidad y latencia son peores que bare metal. En producción corporativa preferir hosts físicos o VMs con passthrough de KVM ya validado.

### Paquetes (Ubuntu o Debian)

```bash
sudo apt update
sudo apt install -y \
  cpu-checker \
  qemu-utils \
  iproute2 \
  iptables \
  nftables \
  curl \
  ca-certificates \
  jq
# Opcional: build de binarios ASP
# golang-go rustc cargo
```

Dos programas que normalmente no están en los repositorios: `cloud-hypervisor` (abajo) y el `virtiofsd` en Rust (no el de QEMU; solo hace falta para las sandboxes con workspace). `virtiofsd` sí está en Ubuntu 24.04 y Debian 13 (`sudo apt install virtiofsd`, que lo deja en `/usr/libexec/virtiofsd`: el nodo lo busca ahí además de en el `PATH`); en otras distribuciones, de [su repositorio](https://gitlab.com/virtio-fs/virtiofsd), y `--virtiofsd-bin` lo apunta. `setpriv` (util-linux 2.31 o posterior, ya instalado en Ubuntu y Debian) lo usa el VMM sin privilegios, y `wireguard-tools` solo la red local-net. `sudo asp doctor` dice qué falta.

El usuario del servicio node-agent debe pertenecer al grupo `kvm` (y normalmente `netdev` si crea TAPs):

```bash
sudo usermod -aG kvm,netdev "$USER"
# re-login o newgrp kvm
```

## 2. Cloud Hypervisor

Patrón de releases oficiales:

```text
https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/vX.Y.Z/cloud-hypervisor-static
https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/vX.Y.Z/cloud-hypervisor-static-aarch64
# latest:
https://github.com/cloud-hypervisor/cloud-hypervisor/releases/latest/download/cloud-hypervisor-static
```

Ejemplo x86-64 (fija una versión en prod; no uses `latest` ciegamente):

```bash
CH_VER=v53.0   # ajustar a la release validada
sudo install -d -m 0755 /usr/local/bin
curl -fsSL -o /tmp/cloud-hypervisor-static \
  "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VER}/cloud-hypervisor-static"
chmod +x /tmp/cloud-hypervisor-static
sudo mv /tmp/cloud-hypervisor-static /usr/local/bin/cloud-hypervisor
cloud-hypervisor --version
```

`--ch-binary` (`ASP_CH_BINARY`) apunta al binario que el node-agent **lanza por sandbox** ([cómo ejecuta un nodo las sandboxes](../concepts/node-runtime.md)). La v53 es la probada: `sudo asp doctor` avisa si la mayor es otra.

## 3. El kernel y la imagen del guest

Cada VM arranca el mismo kernel (un `vmlinux` sin comprimir, con virtio-blk, virtio-net y vsock: `CONFIG_VIRTIO_*`, `CONFIG_VSOCKETS`) y su disco es una **copia privada** de una imagen base `rootfs.img` (raw ext4, con el `pod-daemon` y la red mínima; la imagen base no se arranca nunca). El nodo los busca por defecto en `/opt/sandbox/vmlinux` y `/opt/sandbox/rootfs.img`; `--guest-kernel` y `--guest-rootfs` cambian las rutas, y una prueba con otro guest no necesita tocar `/opt/sandbox`.

```bash
sudo install -d -m 0755 /opt/sandbox /var/lib/asp/{images,kernels}

# Una versión publicada: la descarga, la comprueba contra su SHA256SUMS y la enlaza en /opt/sandbox
sudo asp image pull --version 0.1.0
asp image verify /opt/sandbox

# O los tuyos: copiar un kernel y una imagen validados
sudo cp /path/to/vmlinux /var/lib/asp/kernels/vmlinux
sudo cp /path/to/rootfs.img /var/lib/asp/images/rootfs.img
sudo ln -sfn /var/lib/asp/kernels/vmlinux /opt/sandbox/vmlinux
sudo ln -sfn /var/lib/asp/images/rootfs.img /opt/sandbox/rootfs.img
```

Fuentes típicas de kernel y rootfs:

- **Una versión publicada** (`asp image pull`): el `rootfs.img` y el `vmlinux` de la release, con su `SHA256SUMS` y su `image.json` (la lista de paquetes con su versión).
- Construirlos tú: `scripts/build-guest-image.sh --kernel` hace de [`images/guest`](../../images/guest/) un `rootfs.img` ext4 idéntico en cada build, sin root ni montar nada, y compila el kernel del proyecto (6.18, la configuración de Cloud Hypervisor con todo dentro, también reproducible: [el kernel](../../images/guest/README.md#el-kernel)). Con otro kernel (el de tu distribución, con vsock como módulo: `ASP_GUEST_MODULES`) se pasa con `ASP_GUEST_KERNEL_FILE`.
- Una cloud image mínima convertida a raw (`qemu-img convert`) con `pod-daemon` inyectado.

**El nodo comprueba lo que arranca.** Si junto al kernel o a la imagen hay un `SHA256SUMS` que los lista (`asp image pull` lo deja), el node-agent compara su digest con el de la lista antes de arrancar una VM o clonar un disco (`--guest-verify`, por defecto `auto`) y, si no coincide, la sandbox falla con el motivo. `on` exige además que estén listados; `off` no comprueba. `sudo asp doctor` lo dice antes de arrancar.

La línea de comandos del kernel que envía el nodo es `console=ttyS0 root=/dev/vda reboot=k panic=1` (el guest tiene que ver el disco virtio-blk como `/dev/vda`), y con `--tap-auto` se le añaden `ip=…` y, si hay proxy, `systemd.setenv=HTTP_PROXY=…` ([la red de una sandbox](../concepts/networking-and-egress.md)). Una imagen anterior a esos campos arranca igual, pero sin nombre, sin DNS y sin variables: hay que reconstruirla (`scripts/build-guest-rootfs.sh`).

## 4. Dónde guarda cosas el nodo

```text
/opt/sandbox/{vmlinux,rootfs.img}   # lo que arranca (--guest-kernel, --guest-rootfs); suelen ser enlaces
/var/lib/asp/
  images/, kernels/    # la imagen y el kernel, si no usas `asp image pull` (este instala en images/<versión>/, con `current`)
  node-certs/          # --cert-dir: el certificado del nodo (y la CA con que se verifica el plano de control)
  disks/               # --disk-dir: rootfs-{sandboxID}.img, la copia privada de cada sandbox; una parada la conserva
  local-net/           # --local-net-key-dir: la clave WireGuard de cada sesión local-net
  agent.token          # --agent-token-file: el secreto de la API local del agente (0600)
/run/asp/              # --ch-socket-dir: sockets de las VMs (ch-{id}.sock, vsock-{id}.sock…) y node-agent.lock; 0700, de root
/run/asp-vm/           # --vm-run-dir: un directorio por VM, de su usuario sin privilegios (0711)
```

Los discos de las sandboxes pueden ocupar mucho: ponlos en una partición con espacio (`--disk-dir`), no en una `/var` pequeña. Cada uno es una copia de la imagen base hecha con `cp --reflink=auto --sparse=always`: un clon donde el sistema de ficheros lo permite (btrfs, XFS) y una copia dispersa donde no (ext4), que solo ocupa los bloques usados y crece a medida que el guest escribe. Una sandbox parada conserva el suyo hasta que se borra o caduca.

## 5. El servicio

```bash
sudo install -m 0755 build/node-agent /usr/local/bin/node-agent                # de `make build`, o del tarball de la release
sudo install -d -m 0700 /etc/asp/agent.yaml.d
sudo install -m 0600 packaging/etc/agent.yaml /etc/asp/agent.yaml        # lo que necesita todo nodo
sudoedit /etc/asp/agent.yaml.d/10-site.yaml   # control_plane_url, control_plane_ca, endpoint… (ejemplos en agent.yaml)
sudo cp scripts/systemd/asp-node-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now asp-node-agent.service
journalctl -u asp-node-agent -f
```

Primer arranque: añade `enroll: true` y `enroll_token: …` en un fichero propio (`/etc/asp/agent.yaml.d/20-enroll.yaml`) y bórralo cuando el journal diga `enrolled`; el token no debe quedarse en el nodo. Los ajustes viven en el YAML (la unit solo lleva `ExecStart=… --config /etc/asp/agent.yaml`) y `node-agent --print-config` dice qué vale cada uno y de dónde viene; [el fichero de configuración](config-file.md) lo explica.

Las dos unidades reintentan el arranque **sin tope** (`StartLimitIntervalSec=0`, cada 5 s): el agente sale si no puede registrarse, y con el límite por defecto de systemd (5 arranques en 10 s) un plano de control que tarde en responder al arrancar el servidor dejaría el nodo caído hasta arrancarlo a mano. Un nodo de laboratorio con el plano de control en el mismo host (HTTP por loopback, sin mTLS ni enroll) usa [`scripts/systemd/asp-node-agent-lab.service`](../../scripts/systemd/asp-node-agent-lab.service), descrita en [el laboratorio](../lab/README.md).

Con el plano de control en **otro host**, el `exec` llega al nodo por mTLS (`--agent-tls-listen`), con un certificado de enroll de servidor, y hay reglas de firewall: todo está en [varios servidores](../ops-multi-node.md#añadir-un-servidor). Cómo se comporta el agente al reiniciarse y qué limpia al arrancar: [cómo ejecuta un nodo las sandboxes](../concepts/node-runtime.md).

## 6. Comprobar

**La mayor parte de esta lista la hace sola `sudo asp doctor`** (en el nodo, sin arrancar el agente) o `asp node doctor <id>` (al agente en marcha): KVM, hipervisor, `virtiofsd`, kernel e imagen, disco, TAP, nft, reloj y plano de control, con qué arreglar cada fallo ([diagnosticar un nodo](troubleshooting.md)).

1. **El nodo se ve:** `asp node list` lo muestra como `SCHEDULABLE yes`, con la capacidad que debe tener.
2. **Una sandbox de punta a punta:**

   ```bash
   asp session start --name probe                      # el plano de control la coloca en un nodo con hueco (503 si ninguno cabe)
   asp session exec --name probe --cmd 'id; uname -a'
   asp session stop --name probe                       # el guest se apaga solo; el disco queda en --disk-dir
   asp session resume --name probe                     # arranca por segunda vez, con lo escrito antes
   asp session rm --name probe                         # borra la sandbox y su disco
   ```

   Mientras corre, `GET /v1/sandboxes/{id}/events` muestra las transiciones (`requested` → `starting` → `running`).
3. **El egress, con la política de un tenant:** `PUT /v1/tenants/{id}/egress` y un `exec` que alcance un host permitido y otro que no (403). Cómo y qué obliga: [red y egress](../concepts/networking-and-egress.md).
4. **Opcional, el guest hacia el host:** desde dentro, `vsock` al CID 2 puertos 26501 (agente SSH) y 26502 (identidad).

## Siguientes pasos

- Varios nodos, capacidad, `cordon`: [varios servidores](../ops-multi-node.md).
- Endurecer el despliegue (claves, mTLS, atestación, fencing): [operaciones de seguridad](security-operations.md).
- Qué hace el agente cuando algo falla: [diagnosticar un nodo](troubleshooting.md).
