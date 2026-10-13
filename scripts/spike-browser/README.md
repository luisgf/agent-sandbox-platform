# Estudio: un navegador y un escritorio en la sandbox (#231)

Los scripts que produjeron las cifras de [ADR-0018](../../docs/adr/0018-browser-desktop.md), para repetirlas. Son de laboratorio: rutas fijas del host de pruebas (`/sandbox/spike-browser`) y de la VM desechable (`/root`), y nada de esto lo usa ASP.

| Fichero | Dónde corre | Qué hace |
|---|---|---|
| [`rig.sh`](rig.sh) | el host, como root | una VM Ubuntu 24.04 desechable con KVM anidado en un network namespace propio, un proxy con lista blanca y un servidor de ficheros para la release (`up`, `ssh asp …`, `tproxy`, `down [purge]`); el del estudio de Kubernetes recortado a una máquina |
| [`tproxy-host.py`](tproxy-host.py) | el host (lo arranca `rig.sh tproxy`) | la salida del proxy de ASP: el namespace redirige `93.184.216.34:80/443` aquí, y esto lo pasa al proxy del laboratorio por la cabecera `Host` o el SNI |
| [`rig-setup.sh`](rig-setup.sh) | la VM, como root, tras el instalador | los sitios de las pruebas resuelven a esa dirección; la lista blanca del tenant `default` |
| [`tests.sh`](tests.sh) | la VM, como root | las medidas: `plain`, `switch`, `desktop`, `background`, `background-flags`, `headless`, `density N MiB` |
| [`cdp.py`](cdp.py) | dentro del guest o en el nodo | un cliente mínimo de DevTools (solo la biblioteca estándar): `wait`, `version`, `goto URL [--screenshot]` |
| [`vsock_bridge.py`](vsock_bridge.py) | el nodo | un puerto TCP delante de un puerto vsock del guest, por el socket de Cloud Hypervisor (`CONNECT <puerto>`): lo que haría el nodo en un reenvío de puertos |

## Cómo se repite

```bash
# En el host: las dos imágenes desde esta rama, con el kernel que ya tenga el nodo
ASP_GUEST_VERSION=0.0.0-SNAPSHOT-x ASP_GUEST_KERNEL_FILE=/opt/sandbox/vmlinux scripts/build-guest-image.sh /sandbox/spike-browser/guest
ASP_GUEST_VARIANT=desktop ASP_GUEST_VERSION=0.0.0-SNAPSHOT-x-desktop ASP_GUEST_KERNEL_FILE=/opt/sandbox/vmlinux \
  scripts/build-guest-image.sh /sandbox/spike-browser/guest-desktop

# En el Mac: los paquetes de ASP (make snapshot) al host; en el host, la release en /sandbox/spike-browser/release
sudo scripts/spike-browser/rig.sh up && sudo scripts/spike-browser/rig.sh tproxy
# En la VM: el instalador de un solo host desde la release, y luego
sudo bash rig-setup.sh
sudo bash tests.sh plain; sudo bash tests.sh switch; sudo bash tests.sh desktop   # y el resto de modos
```

`tests.sh` necesita `HOME` (la CLI guarda ahí las sesiones): bajo `systemd-run`, `--setenv=HOME=/root`. Al terminar, `sudo rig.sh down purge` y borrar `/sandbox/spike-browser`.
