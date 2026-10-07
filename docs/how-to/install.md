# Instalar ASP con el script

`install.sh` instala una [versión](release.md) en este host: el CLI, un plano de control o un nodo. Baja los paquetes de la release (`.deb` o `.rpm`; un tarball en cualquier otro Linux, y también en macOS para el CLI), los **comprueba contra el `SHA256SUMS`** de la release, los instala, escribe `/etc/asp/*.env` a partir de las variables de abajo y arranca el servicio. No arranca nada si el host no tiene systemd, y no pisa nunca un fichero de ajustes que ya tenga ajustes.

```bash
# el CLI
curl -fsSL https://github.com/luisgf/agent-sandbox-platform/releases/latest/download/install.sh | sudo sh

# un plano de control (en memoria; con INSTALL_ASP_DATABASE_URL=postgres://… guarda el estado)
curl -fsSL …/install.sh | sudo INSTALL_ASP_ROLE=server sh

# un nodo (necesita KVM): el token se saca en el plano de control con `asp node enroll-token --node-id <id>`
curl -fsSL …/install.sh | sudo INSTALL_ASP_ROLE=agent INSTALL_ASP_SERVER=https://cp.example:8443 \
     INSTALL_ASP_TOKEN=<token> sh
```

Léelo antes de ejecutarlo si quieres: es un único fichero de shell (`scripts/install.sh`), y cada release lo trae con su suma en `SHA256SUMS`.

## Variables

| Variable | Qué hace |
|---|---|
| `INSTALL_ASP_ROLE` | `cli` (por defecto), `server` (plano de control + CLI) o `agent` (nodo + CLI); se pueden juntar con comas |
| `INSTALL_ASP_VERSION` | una versión (`0.1.0`); por defecto la última release |
| `INSTALL_ASP_URL` | dónde están los ficheros de la release, sin `/` final (un espejo, o un directorio servido por HTTP; hace falta `INSTALL_ASP_VERSION`) |
| `INSTALL_ASP_METHOD` | `deb`, `rpm` o `tar`; por defecto el gestor de paquetes del host |
| `INSTALL_ASP_NO_START` | `1` instala y configura, no arranca nada |
| `INSTALL_ASP_FORCE` | `1` reemplaza un `/etc/asp/*.env` que ya tenga ajustes (deja el anterior como `.bak`) |
| **server** | |
| `INSTALL_ASP_LISTEN` | dónde escucha (por defecto `127.0.0.1:8080`; los nodos de otros hosts necesitan una dirección que alcancen, y TLS) |
| `INSTALL_ASP_DATABASE_URL` | un Postgres; sin él el estado se pierde al reiniciar |
| `INSTALL_ASP_SELF_SIGNED` | `1` crea un certificado TLS autofirmado (`/etc/asp/tls.crt`) para `INSTALL_ASP_TLS_SAN` (por defecto el nombre del host y `127.0.0.1`); los nodos lo toman como su `INSTALL_ASP_CA` |
| `INSTALL_ASP_TLS_CERT`, `INSTALL_ASP_TLS_KEY` | un certificado que ya tienes |
| **agent** | |
| `INSTALL_ASP_SERVER` | la URL del plano de control (obligatoria) |
| `INSTALL_ASP_TOKEN` | un token de alta de nodo (`asp node enroll-token`); el script lo saca de `/etc/asp/node-agent.env` cuando el nodo se ha registrado |
| `INSTALL_ASP_NODE_ID` | el id del nodo (por defecto el nombre del host) |
| `INSTALL_ASP_ENDPOINT` | cómo llega el plano de control al nodo (por defecto `https://<id>:9443`) |
| `INSTALL_ASP_CA` | un PEM con la CA del certificado TLS del plano de control |
| `INSTALL_ASP_SKIP_IMAGE` | `1` no baja el kernel y la imagen del guest de la release (`asp image pull`) |

## Qué deja en el host

- `asp` en `/usr/bin` (paquete) o `/usr/local/bin` (tarball); con el rol `server`, `asp-control-plane` y su unit; con `agent`, `asp-node-agent` y su unit.
- `/etc/asp/control-plane.env` o `node-agent.env`, de modo 0600. El plano de control guarda la clave de administración en **`/etc/asp/admin-key`** (0600): `export ASP_API_KEY=$(sudo cat /etc/asp/admin-key)`. Con TLS, el plano de control se ejecuta con el usuario de sistema `asp-control-plane`, que es el único, además de root, que lee `tls.key`.
- Un nodo necesita además `cloud-hypervisor` y `virtiofsd` (no están en los repositorios de las distribuciones): `sudo asp doctor` dice qué falta y cómo arreglarlo ([`troubleshooting.md`](troubleshooting.md)).
- `/usr/local/bin/asp-killall.sh` (para todas las VMs del nodo y quita lo que dejan) y `asp-uninstall.sh` (quita ASP; `--purge` borra también `/etc/asp` y `/var/lib/asp*`, con confirmación, y con ellos los discos de las sandboxes paradas y la CA del plano de control).

## Añadir un nodo

En el plano de control: `asp node enroll-token --node-id node2` (un token de un solo uso, atado a ese id). En el host nuevo, el tercer comando de arriba. Cuando el nodo se registra, `asp node list` lo muestra.
