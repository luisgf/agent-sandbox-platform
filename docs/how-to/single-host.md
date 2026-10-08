# ASP en un solo host

Un servidor Linux con KVM, tres comandos hasta la primera sesión, sin editar ningún fichero:

```bash
curl -fsSL https://github.com/luisgf/agent-sandbox-platform/releases/latest/download/install.sh | sudo INSTALL_ASP_ROLE=standalone sh
sudo asp session start
sudo asp session exec --cmd 'id'
```

El instalador baja y comprueba los paquetes (`asp`, `asp-control-plane`, `asp-node-agent` y `asp-server`, [install.md](install.md)), instala Cloud Hypervisor (y `virtiofsd`, donde la distribución lo tiene), baja el kernel y la imagen del guest de esa versión, y arranca el servicio `asp-server`. Éste hace lo que antes eran los 24 pasos de [instalar un nodo](install-node.md) y [el plano de control](install-control-plane.md): la base de datos, las claves, el certificado TLS, la clave de administración y el token del nodo; arranca un plano de control y un nodo, los mantiene en marcha y deja configurado el comando `asp` de ese host.

Sin systemd, o con el tarball, es el mismo programa: `sudo asp-server` (ver abajo).

## Qué hace `asp-server`

Guarda todo bajo `--data-dir` (`/var/lib/asp`). Lo que no existe lo crea en el primer arranque; lo que existe lo respeta, y por eso puede reiniciarse sin que cambie nada:

| Fichero | Qué es | Modo |
|---|---|---|
| `server/asp.db` | el estado del plano de control: sandboxes, nodos, claves, eventos ([SQLite](../../control-plane/README.md), sin servidor de base de datos) | 0600 |
| `server/ca.crt`, `ca.key` | la CA que firma los certificados de los nodos | 0600 la clave |
| `server/oidc-key.pem`, `attest-key.pem` | las claves con que el plano de control firma tokens y verifica atestaciones | 0600 |
| `server/tls.crt`, `tls.key` | el certificado TLS (autofirmado, ECDSA P-256, 10 años) y su clave | 0644 / 0600 |
| `server/admin-key` | la clave de administración (alcance de plataforma) | 0640 `root:asp` (0600 sin grupo `asp`) |
| `server/node-token` | el secreto con que se alta el nodo de este host | 0600 |
| `server/asp.yaml` | cómo llega el comando `asp` a este servidor (URL, certificado, fichero de la clave) | 0644 |
| `agent.token` | el secreto de la API local del nodo, que lee el plano de control | 0640 `root:asp-control-plane` |
| `node-certs/`, `disks/`, `local-net/` | el certificado del nodo, los discos de las sandboxes, las claves de local-net | 0700 |

Cómo arranca los programas:

- **El plano de control** corre como el usuario `asp-control-plane` (que crea el paquete) y escucha en `127.0.0.1:8443` con TLS, **autenticación siempre activa** y mTLS para los nodos. Su base de datos es `server/asp.db`.
- **El nodo** corre como root (necesita TAPs, nftables, KVM) con los ajustes del paquete ([agent.yaml](config-file.md)); se da de alta con el plano de control por mTLS, como cualquier otro nodo.
- Espera a que el plano de control responda antes de arrancar el nodo; si uno muere, lo reinicia con una espera creciente; si uno **rechaza sus ajustes** (sale con código 2) no lo reinicia, para y dice cuál (`sudo asp doctor` dice qué le falta al host). Al parar, **para el nodo y luego el plano de control**. Las VMs no se paran: viven en sus propias units y el siguiente arranque las adopta ([ADR-0014](../adr/0014-vms-outlive-the-agent.md)).
- Un host sin `/dev/kvm` corre solo el plano de control (y lo dice): otros hosts pueden unirse. `--profile lab` arranca un nodo **sin VMs** (`--dry-run`: las sandboxes son registros, no hay `exec`) para probar el flujo sin KVM.

## Usar `asp` en este host

`asp-server` escribe `/etc/asp/asp.yaml` (si no existía; nunca lo pisa) con la URL, el certificado y el **fichero** de la clave de administración, no la clave:

```yaml
control_plane_url: https://127.0.0.1:8443
ca_file: /var/lib/asp/server/tls.crt
api_key_file: /var/lib/asp/server/admin-key
```

Lo lee root y los miembros del grupo `asp`: `sudo usermod -aG asp $USER` (y volver a entrar). Quien no pueda leer el fichero de la clave recibe un error que lo dice. Si ya había un `/etc/asp/asp.yaml`, el de este servidor queda en `/var/lib/asp/server/asp.yaml` (`export ASP_CONFIG=/var/lib/asp/server/asp.yaml`). `asp config show` dice de dónde viene cada ajuste.

Las sandboxes de un tenant sin reglas de egress **no salen a ninguna parte** (el valor por defecto de un plano de control con base de datos). Para permitir destinos:

```bash
sudo curl --cacert /var/lib/asp/server/tls.crt -H "Authorization: Bearer $(sudo cat /var/lib/asp/server/admin-key)" \
  -X PUT https://127.0.0.1:8443/v1/tenants/default/egress -d '{"rules":[{"host_pattern":"*.github.com","port":443,"enabled":true}]}'
```

## Añadir un host

El plano de control escucha solo en el loopback hasta que se le diga otra cosa. Para que otros hosts se unan, que escuche en una dirección que alcancen y que su certificado la nombre:

```yaml
# /etc/asp/standalone.yaml.d/10-site.yaml
listen: 0.0.0.0:8443
tls_san: [asp.example.com]
```

(o `INSTALL_ASP_LISTEN=0.0.0.0:8443 INSTALL_ASP_TLS_SAN=asp.example.com` al instalar). `asp-server` hace un certificado nuevo si el actual no cubre un nombre, y lo dice: los nodos que confiaban en el anterior hay que darles éste. Después:

```bash
sudo asp node enroll-token --node-id host2      # en el servidor: imprime el token y el comando para el otro host
```

```bash
# en el host nuevo (necesita KVM): el comando que imprimió asp node enroll-token
curl -fsSL …/install.sh | sudo INSTALL_ASP_ROLE=agent INSTALL_ASP_SERVER=https://asp.example.com:8443 \
     INSTALL_ASP_TOKEN=<token> INSTALL_ASP_CA_SHA256=<huella> sh
```

El instalador lee el certificado del servidor, **lo rechaza si su SHA-256 no es la huella** y solo entonces lo toma como confianza. `asp node list` muestra el nodo nuevo.

## Ajustes

`/etc/asp/standalone.yaml` y los de `standalone.yaml.d/` ([el fichero de configuración](config-file.md)); las claves son las banderas de `asp-server` con guiones bajos:

| Bandera | Qué hace |
|---|---|
| `--data-dir` | dónde vive todo (`/var/lib/asp`) |
| `--listen` | dónde escucha el plano de control (`127.0.0.1:8443`) |
| `--tls-san` | más nombres o direcciones para el certificado |
| `--node-id` | el id del nodo de este host (por defecto el nombre del host seguido de `-node`: el nombre del host está en el certificado del plano de control, y éste no admite un nodo que lleve un nombre de su certificado) |
| `--no-agent` | solo el plano de control |
| `--profile` | `default` o `lab` |
| `--control-plane`, `--node-agent` | los programas (por defecto, junto a `asp-server`, en el `PATH` o donde los paquetes) |
| `--user`, `--group` | la cuenta del plano de control (`asp-control-plane`) y el grupo que lee la clave (`asp`) |
| `--cli-config` | dónde dejar la configuración de `asp` de este host (`/etc/asp/asp.yaml`; `-` ninguna) |

Todo lo demás se ajusta como siempre: el plano de control lee `/etc/asp/server.yaml` y `server.yaml.d/`, el nodo `/etc/asp/agent.yaml`, y el entorno de `asp-server` se hereda (gana a los ficheros). Lo que decide `asp-server` (dónde escucha, las rutas de las claves y de los discos, la clave de administración, el token del nodo) lo pone en el entorno de los programas y no lo cambia un fichero; la base de datos es lo único que el entorno puede reemplazar: con `ASP_DATABASE_URL=postgres://…` en la unit (`systemctl edit asp-server`), el plano de control usa ese Postgres y no `asp.db`.

## Copias de seguridad

Lo que importa es `server/` (la base de datos **y** las claves: sin `ca.key` los nodos enrolados dejan de reconocerse), `node-certs/` y `agent.token`. Los discos de las sandboxes paradas están en `disks/`. Con el servicio parado basta copiar el directorio; en caliente, la base de datos se copia con `sqlite3 /var/lib/asp/server/asp.db ".backup /var/backups/asp.db"` y las claves con `cp -p`.

## Límites

- **Un único plano de control**: SQLite es un fichero de un host. Para más de un plano de control (alta disponibilidad), Postgres.
- `asp-server` corre como root, como el nodo; el plano de control que arranca, no. Para endurecer la unit del plano de control (`ProtectSystem`, `NoNewPrivileges`…) hay que usar sus servicios por separado ([instalar el plano de control](install-control-plane.md) y [un nodo](install-node.md)): no se instalan juntos, y la unit de `asp-server` declara `Conflicts=` con ellos.
- Las claves de este host (la CA incluida) están en el mismo disco que los datos.

## Sin systemd, o a mano

```bash
sudo asp-server                                   # /var/lib/asp, el loopback, perfil por defecto
asp-server --profile lab --data-dir ./asp-data --listen 127.0.0.1:18443 --cli-config -   # sin KVM ni root
ASP_CONFIG=./asp-data/server/asp.yaml asp node list
```

`scripts/smoke-standalone.sh` hace exactamente eso con los binarios del repositorio (lo corre CI): comprueba que el comando llega con la configuración que deja `asp-server`, que sin ella no confía en el certificado, que una sesión arranca, los modos de los ficheros, la huella de `enroll-token`, el orden de parada y que todo sobrevive a un reinicio.

## Quitarlo

`sudo asp-uninstall.sh` (o `--purge`, que borra también `/etc/asp` y `/var/lib/asp*`: la base de datos, las claves, los discos de las sandboxes paradas).
