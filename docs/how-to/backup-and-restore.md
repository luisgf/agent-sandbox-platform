# Copias de seguridad y restauración

Qué hay que guardar de un despliegue de ASP, cómo, y qué pasa al restaurarlo. El estado está en tres sitios con consecuencias distintas: la **base de datos** del plano de control, sus **claves** (la CA de los nodos, la firma de tokens) y lo que cada **nodo** guarda en su disco. Lo más importante de esta página es lo que hacen los nodos con una base restaurada: [se obedece a la base, no a lo que hay en el nodo](#qué-hacen-los-nodos-con-una-base-restaurada).

> **Ensayado.** Los comandos de SQLite y de Postgres de esta página se ejecutaron contra el plano de control real: SQLite con `.backup` (y la prueba de que un `cp` en caliente no sirve), y Postgres 18 en dos contenedores desechables (`pg_dump` en caliente, `pg_restore` en una base nueva, un plano de control arrancado sobre ella). También se ensayó, sobre un nodo real (una VM Ubuntu con KVM anidado y `asp-server` con SQLite, dos sandboxes en marcha), restaurar una copia hecha antes de que existiera la segunda: las VMs siguieron al parar `asp-server`; al arrancar con la base vieja el nodo **paró la VM de la sandbox posterior a la copia en 5 s y borró su disco a los 75 s**, y la anterior siguió y contestó. Los escenarios de pérdida de claves y de nodos se describen a partir del código; no se han ensayado de punta a punta.

## Qué hay que guardar

| Qué | Dónde | Si se pierde |
|---|---|---|
| **La base de datos** | Postgres (`database_url`), o `asp.db` de SQLite (`asp-server`: `/var/lib/asp/server/asp.db`) | Se pierden las sandboxes y su historial de eventos, los nodos dados de alta, las claves de API (se guardan con hash), las reglas de egress, los tokens de enroll y las atestaciones. Con las claves y sin la base, los nodos se vuelven a registrar solos, pero no hay ninguna sandbox |
| **La CA de los nodos** | `ca.crt` y `ca.key` (`ca_cert`, `ca_key`; en el paquete, `/var/lib/asp-control-plane/`) | **Sin la CA ningún nodo enrolado se reconoce**: hay que re-enrolarlos todos. Y si falta **uno solo** de los dos ficheros, el plano de control crea una CA nueva y escribe encima del que quedaba |
| **Las claves de firma** | `oidc-key.pem` (tokens de workload) y `attest-key.pem` (atestación), junto a la CA | Con claves nuevas cambia el `kid`: los servicios que guardaron el JWKS tienen que volver a leerlo, y las atestaciones antiguas no se pueden volver a verificar con la clave nueva |
| **El certificado TLS del plano de control** | `tls_cert`, `tls_key` (el que tú le das, o el autofirmado que hace `asp-server`) | Se emite otro; si es autofirmado, los nodos y los clientes tienen que volver a confiar en él |
| **Los ajustes** | `/etc/asp/` entero: `server.yaml`, `server.yaml.d/`, `agent.yaml`, `agent.yaml.d/`, `standalone.yaml`, `asp.yaml`, `admin-key` | Se vuelven a escribir; la clave de administración (`admin-key`) y los secretos en los drop-ins (la URL de la base de datos, el IdP) solo están ahí |
| **En cada nodo: su identidad** | `/var/lib/asp/node-certs/` (certificado y clave), `/var/lib/asp/agent.token` | Se vuelve a dar de alta con un token de enroll fijado a ese id (`asp node enroll-token --node-id <id>`); las VMs no se pierden |
| **En cada nodo: los discos** | `--disk-dir` (`/var/lib/asp/disks/rootfs-<id>.img`) | Se pierde el sistema de ficheros de las sesiones de ese nodo, también el de las paradas. **No se copian en caliente**: ver [los discos](#los-discos-y-los-workspaces) |
| **En cada nodo: los workspaces** | el directorio que compartiste con `--workspace` (por defecto bajo `/srv/asp/workspaces/<tenant>/`) | El trabajo del agente, si lo dejó ahí. Es un directorio normal del host: cópialo como cualquier otro |

Lo que **no** hay que copiar: `/run/asp*` (sockets y registros de las VMs que un agente nuevo adopta; es un `tmpfs`), `~/.cache/asp/sessions/` (los punteros de sesión de cada cliente) y el almacén en memoria (no es un despliegue: se pierde al reiniciar).

> **Trata la copia como un secreto.** La CA permite emitir un nodo; la clave OIDC, tokens de workload; y la base puede guardar credenciales de fencing si le diste el secreto (`asp node fence set --token-stdin`; con `--token-env` o `--token-file` el secreto no llega a la base). Cifra las copias y no las dejes en el mismo disco que lo que copian.

## Hacer la copia

### Postgres

`pg_dump` toma una instantánea coherente **sin parar el plano de control**:

```bash
# la contraseña, en ~/.pgpass o PGPASSFILE: no en la línea de comandos
pg_dump --format=custom --file "/var/backups/asp/asp-$(date +%F).dump" \
  "postgres://asp@db.example.corp:5432/asp?sslmode=require"
```

Para recuperar hasta el último segundo hace falta el archivado continuo de Postgres (WAL), que no es cosa de ASP: [documentación de Postgres](https://www.postgresql.org/docs/current/continuous-archiving.html).

### SQLite (`asp-server`)

La base está en modo WAL: lo último que se escribió puede estar en `asp.db-wal` y **no** en `asp.db`. Un `cp asp.db` con el servicio en marcha copia un fichero al que le faltan cosas: en el ensayo, con un plano de control recién arrancado, la copia **no tenía ni las tablas**. Usa la copia en línea de SQLite:

```bash
sudo sqlite3 /var/lib/asp/server/asp.db ".backup '/var/backups/asp/asp.db'"
sqlite3 /var/backups/asp/asp.db "PRAGMA integrity_check"        # ok
```

Con el servicio parado (`systemctl stop asp-server`) basta con copiar `asp.db`: al cerrarse limpio se vuelca el WAL y solo queda ese fichero.

### Claves y ajustes

```bash
sudo tar -C / --acls --xattrs -cpf - var/lib/asp-control-plane etc/asp \
  | gpg --encrypt --recipient ops@example.corp > "asp-claves-$(date +%F).tar.gpg"
```

Cambian poco (la CA, nunca; la clave OIDC, si la rotas con `ASP_OIDC_KEY_PREV`): cópialas una vez, y de nuevo cada vez que cambien. Con `asp-server`, `server/` de su `--data-dir` ya tiene la base **y** las claves: sin `ca.key` los nodos enrolados dejan de reconocerse ([un solo host](single-host.md#copias-de-seguridad)).

### Los discos y los workspaces

El disco de una sandbox es una imagen ext4 de una VM: copiarlo mientras la VM escribe da algo parecido a una imagen tras un corte de luz (ext4 lo recupera al montar, pero puede faltar lo último). Una sandbox **parada** (`asp session stop`) cerró limpiamente su sistema de ficheros: copiar su disco es seguro.

```bash
sudo cp --sparse=always /var/lib/asp/disks/rootfs-<id>.img /var/backups/asp/disks/      # una sandbox parada
sudo mount -o ro,loop,noload /var/backups/asp/disks/rootfs-<id>.img /mnt                # para sacar ficheros de ella
```

La copia de todos los discos casi nunca merece la pena: una parada caduca a los 7 días (`ASP_STOPPED_SANDBOX_TTL`) y el disco de una sandbox es el sistema del agente, no donde debería estar lo que quieres conservar. **Lo que importa se guarda en el workspace** (un directorio del host, que copias como cualquier otro) o se sube a un repositorio.

### Cada cuánto

- **La base de datos:** a diario, y siempre justo antes de actualizar ([actualizar](upgrade.md)).
- **Las claves y los ajustes:** una vez, y cada vez que cambien.
- **Los discos:** solo los que sepas que importan, parados.

## Restaurar

### Qué hacen los nodos con una base restaurada

Un nodo hace lo que dice la base, sin preguntar:

- una VM que corre en el nodo y que la base no le asigna (el conjunto `assigned` de cada sondeo) **se detiene**;
- un disco de `--disk-dir` que ninguna sandbox de la base reclama (ni asignada, ni retenida, ni en curso) **se borra**, como muy tarde un minuto después de su primer sondeo con éxito;
- un nodo que la base no conoce **se registra otra vez** solo, si su certificado lo firmó la CA.

Es decir: **restaurar una base más vieja que lo que hay en los nodos destruye las sandboxes y los discos que se crearon después de la copia.** Y revierte lo demás: una clave de API que revocaste después vuelve a autenticar (en el ensayo, una clave revocada tras la copia daba `200` otra vez), un nodo que revocaste deja de estar revocado, las reglas de egress vuelven a las de entonces.

Si no hay más remedio que restaurar una base vieja, antes de arrancar el plano de control restaurado, **en cada nodo**:

```bash
sudo systemctl stop asp-node-agent        # las VMs confinadas siguen; el agente nuevo las adoptará... y parará las que la base no conozca
sudo cp -a --sparse=always /var/lib/asp/disks /var/backups/asp/disks-antes-de-restaurar
```

Los discos de las VMs que corren se copian como tras un corte de luz. Después arranca el plano de control, y luego los agentes. Las sandboxes de después de la copia ya no existen para ASP, pero sus ficheros se pueden sacar montando la imagen copiada (`mount -o ro,loop,noload`).

### El plano de control en otro servidor (con la base intacta)

La base no se pierde si está en otro host (Postgres) o si copias el directorio de datos. Instala el plano de control, restaura las claves y los ajustes **antes de arrancarlo**, y arráncalo:

```bash
sudo apt install ./asp-control-plane_X.Y.Z_linux_amd64.deb
sudo tar -C / --acls --xattrs -xpf claves.tar        # /var/lib/asp-control-plane y /etc/asp
sudo systemctl start asp-control-plane
```

> **Restaura las claves antes de arrancar.** Si `ca_cert`, `ca_key`, `oidc_key` o `attest_key` apuntan a un fichero que no existe, el plano de control **crea unas nuevas** sin protestar: nada falla, y ningún nodo vuelve a reconocerse. Con la CA es peor: si falta solo `ca.key` (o solo `ca.crt`), crea una CA nueva y **sobrescribe también el que quedaba**.

Los nodos lo encuentran solos mientras la URL sea la misma (`control_plane_url`) y el certificado TLS valga para ella; reintentan hasta que contesta y no paran nada mientras tanto. Si la URL cambia, cámbiala en `agent.yaml.d/` de cada nodo y reinicia el agente: sus VMs siguen.

### Restaurar la base de datos

1. **Decide qué copia** y cuánto se pierde (arriba). Si hay una más reciente, usa esa.
2. **Para el plano de control** (`systemctl stop asp-control-plane`).
3. **Restaura en una base nueva**, no encima de la actual (volver atrás es solo cambiar `database_url`):

   ```bash
   createdb --owner asp asp_restaurada
   pg_restore --no-owner --dbname "postgres://asp@db.example.corp:5432/asp_restaurada" /var/backups/asp/asp-2026-10-08.dump
   # y en /etc/asp/server.yaml.d/20-database.yaml: database_url: …/asp_restaurada
   ```

   Con SQLite, copia la copia sobre `asp.db` (con el servicio parado) y quita los `asp.db-wal` y `asp.db-shm` que hubiera.
4. **Arráncalo.** Con la base restaurada ya aplicó todas las migraciones que tenía (`schema_migrations`); las que falten las aplica él al arrancar. En el log: `using Postgres store migrations=ok`.
5. **Comprueba:** `asp node list` (los nodos con `LAST SEEN` reciente), `asp sandbox list`, y `asp apikey list` — para ver **qué revirtió**: las claves creadas después de la copia no están, las revocadas después vuelven a valer (revócalas otra vez), y un nodo que revocaste hay que revocarlo otra vez (`POST /v1/nodes/{id}/revoke`).

### Si pierdes una clave

- **La CA** (`ca.crt` y `ca.key`; con o sin la base): es la raíz de confianza de los nodos. Arranca el plano de control con una CA nueva y **da de alta cada nodo otra vez** con un token fijado a él (`asp node enroll-token --node-id <id>` y `asp-node-agent --enroll …`; [añadir un servidor](../ops-multi-node.md#añadir-un-servidor)). Las VMs de los nodos siguen mientras tanto; los nodos no se pueden comunicar con el plano de control hasta que tengan su certificado nuevo.
- **`oidc-key.pem`**: se crea otra y cambia el `kid`. Los tokens de workload que se firmaron con la vieja dejan de verificar; los servicios que guardaron el JWKS tienen que volver a pedirlo (`/oidc/jwks.json`). Para no cortar nada al rotarla a propósito se usa `ASP_OIDC_KEY_PREV` (mantiene la anterior en el JWKS).
- **`attest-key.pem`**: con mTLS el nodo firma con la clave de su propio certificado y no hace falta; sin mTLS hay que compartir otra con los nodos (`ASP_ATTEST_KEY` o `ASP_ATTEST_TRUSTED_PUBS`). Las atestaciones guardadas se quedan, pero ya no se pueden verificar con la clave perdida.
- **El certificado TLS del plano de control**: emite otro con los mismos nombres. Si lo firmaba una CA tuya, nada más; si es autofirmado, hay que volver a darlo a clientes y nodos (`ca_file` del cliente, `--control-plane-ca` del nodo).
- **`agent.token` de un nodo**: es el secreto de la API local del agente en ese host; se crea otro al arrancar si falta, y hay que dárselo al plano de control del mismo host (`ASP_AGENT_TOKEN_FILE`).

### Si pierdes un nodo

Sus sandboxes pasan a `failed` con `node_lost` a los 5 minutos de callar (y el plano de control intenta apagarlo si hay un *fencing* configurado), y los discos que tenía solo se recuperan si el disco del servidor sigue vivo: no hay migración entre nodos. Para reemplazarlo, da de alta un servidor nuevo con otro id ([añadir un servidor](../ops-multi-node.md#añadir-un-servidor)) y revoca el viejo (`POST /v1/nodes/{id}/revoke`). Si el servidor vuelve, sus VMs ya no figuran como asignadas y se paran, y sus discos sin dueño se borran.

## Comprobar que la copia sirve

Una copia que nunca se ha restaurado es una esperanza. Restáurala en una base **aparte**, con un plano de control sin nodos, y míralo:

```bash
createdb --owner asp asp_prueba
pg_restore --no-owner --dbname "postgres://asp@db.example.corp:5432/asp_prueba" /var/backups/asp/asp-2026-10-08.dump

export KEY=$(openssl rand -hex 16)
ASP_DATABASE_URL="postgres://asp@db.example.corp:5432/asp_prueba?sslmode=require" ASP_LISTEN_ADDR=127.0.0.1:18080 \
  ASP_BOOTSTRAP_API_KEY="$KEY" ASP_NODE_FAILOVER_AFTER=off ASP_ALLOW_TMP_KEYS=1 asp-control-plane      # en una terminal

curl -fsS http://127.0.0.1:18080/healthz
curl -fsS -H "Authorization: Bearer $KEY" 'http://127.0.0.1:18080/v1/sandboxes?include_deleted=1'      # las sandboxes de la copia
dropdb asp_prueba
```

**Nunca arranques un segundo plano de control sobre la base de producción para probar**: cada uno corre su monitor de nodos. Ni siquiera sobre una copia, sin `ASP_NODE_FAILOVER_AFTER=off` y sin `ASP_FENCE_PROVIDER` (déjalo sin poner): el monitor del plano de prueba daría por perdidos los nodos de la copia, que no le hablan, y con un provider de fencing configurado intentaría apagarlos de verdad.
