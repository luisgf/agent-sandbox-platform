# Estudio: un navegador y un escritorio en la sandbox (#231)

Los scripts que produjeron las cifras de [ADR-0018](../../docs/adr/0018-browser-desktop.md), para repetirlas. Son de laboratorio: rutas fijas del host de pruebas (`/sandbox/spike-browser`, que el script llama `$W`) y de la VM desechable (`/root`), y nada de esto lo usa ASP.

| Fichero | Dónde corre | Qué hace |
|---|---|---|
| [`rig.sh`](rig.sh) | el host, como root | una VM Ubuntu 24.04 desechable con KVM anidado en un network namespace propio, un proxy con lista blanca y un servidor de ficheros para la release (`up`, `ssh asp …`, `tproxy`, `down [purge]`); el del estudio de Kubernetes recortado a una máquina |
| [`tproxy-host.py`](tproxy-host.py) | el host (lo arranca `rig.sh tproxy`) | la salida del proxy de ASP: el namespace redirige `93.184.216.34:80/443` aquí, y esto lo pasa al proxy del laboratorio por la cabecera `Host` o el SNI |
| [`rig-setup.sh`](rig-setup.sh) | la VM, como root, tras el instalador | los sitios de las pruebas resuelven a esa dirección; la lista blanca del tenant `default` |
| [`tests.sh`](tests.sh) | la VM, como root | las medidas: `plain`, `switch`, `desktop`, `background`, `background-flags`, `headless`, `density N MiB` |
| [`cdp.py`](cdp.py) | dentro del guest o en el nodo | un cliente mínimo de DevTools (solo la biblioteca estándar): `wait`, `version`, `goto URL [--screenshot]` |
| [`vsock_bridge.py`](vsock_bridge.py) | el nodo | un puerto TCP delante de un puerto vsock del guest, por el socket de Cloud Hypervisor (`CONNECT <puerto>`): lo que haría el nodo en un reenvío de puertos |
| [`rm-all-sandboxes.sh`](rm-all-sandboxes.sh) | la VM, como root | borra por id las sandboxes que una medida interrumpida dejó sin fichero de sesión, y espera a que no quede ninguna VM |

## Cómo se repite

En el host de pruebas, con este árbol en `$W/src` (un worktree) y los scripts copiados en `$W`:

```bash
W=/sandbox/spike-browser
curl -fsSL -o $W/img/noble-server-cloudimg-amd64.img https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img

# Las dos imágenes desde esta rama, con el kernel que ya tenga el nodo (Docker sin buildx: unset DOCKER_BUILDKIT)
cd $W/src
ASP_GUEST_VERSION=0.0.0-SNAPSHOT-x ASP_GUEST_KERNEL_FILE=/opt/sandbox/vmlinux scripts/build-guest-image.sh $W/guest
ASP_GUEST_VARIANT=desktop ASP_GUEST_VERSION=0.0.0-SNAPSHOT-x-desktop ASP_GUEST_KERNEL_FILE=/opt/sandbox/vmlinux \
  scripts/build-guest-image.sh $W/guest-desktop

# La release en $W/release: los paquetes de `make snapshot` (hechos en otra máquina), install.sh, y las dos
# imágenes con el prefijo que espera `asp image pull` (asp-guest_<versión>_rootfs.img.gz, _vmlinux, _image.json, _SHA256SUMS)
sudo $W/rig.sh up && sudo $W/rig.sh tproxy
```

En la VM (`sudo $W/rig.sh ssh asp`), con `tests.sh`, `rig-setup.sh`, `cdp.py` y `vsock_bridge.py` copiados en `/root`:

```bash
curl -fsSL http://10.82.0.1:8001/install.sh | sudo env https_proxy=http://10.82.0.1:3129 http_proxy=http://10.82.0.1:3129 \
  no_proxy=10.82.0.1 INSTALL_ASP_URL=http://10.82.0.1:8001 INSTALL_ASP_VERSION=0.0.0-SNAPSHOT-x INSTALL_ASP_ROLE=standalone sh
sudo bash /root/rig-setup.sh
sudo bash /root/tests.sh plain        # con la imagen normal, la que instala el instalador
sudo bash /root/tests.sh switch       # instala la variante y la hace la actual
sudo bash /root/tests.sh desktop      # y background, background-flags, headless, "density 8 1536", "density 8 1024"
```

`tests.sh` necesita `HOME` (la CLI guarda ahí las sesiones): bajo `systemd-run`, `--setenv=HOME=/root`. No hay que pararlo a medias: las sandboxes que estaban arrancando se quedan sin fichero de sesión (`rm-all-sandboxes.sh` las borra).

El banco de arranque de [ADR-0017](../../docs/adr/0017-fast-start.md) con cada imagen, en la misma VM (necesita `qemu-img`: `sudo apt-get install qemu-utils`):

```bash
I=/var/lib/asp/images/0.0.0-SNAPSHOT-x-desktop
sudo python3 bench-vm-start.py cold --image $I/rootfs.img --kernel $I/vmlinux --virtiofsd /usr/libexec/virtiofsd \
  --helper mount-virtiofs-workspace.sh --workdir /var/tmp/bench-desktop --runs 3
```

Al terminar: `sudo $W/rig.sh down purge`, quitar el worktree (`git worktree remove --force $W/src`), las imágenes de Docker del build (`asp-guest-build:<epoch>-*`, `asp-guest-packer:<epoch>`) y `$W`.
