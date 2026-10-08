# Instalar el plano de control a mano

El **plano de control** es un binario (`asp-control-plane`) con su estado en Postgres, o en un fichero SQLite si es un solo host. [El instalador](install.md) lo deja instalado (`INSTALL_ASP_ROLE=server`); esta guía son los pasos a mano y lo que el instalador no decide por ti: la base de datos, TLS y las claves. Todo en un host, sin configurar nada: [`asp-server`](single-host.md). Los nodos se instalan aparte ([instalar un nodo](install-node.md)).

## 1. La base de datos

Desde la raíz del repositorio, con Postgres de ejemplo (`docker compose up -d postgres`) o uno tuyo:

```bash
docker compose up -d postgres
export ASP_DATABASE_URL='postgres://asp:asp@127.0.0.1:5432/asp?sslmode=disable'
```

Las migraciones se aplican solas al arrancar el API si `ASP_DATABASE_URL` está puesta (están numeradas en `control-plane/migrations`).

**Postgres (o SQLite, en un solo host) es requisito para que parar conserve el disco** ([ADR-0012](../adr/0012-retained-disks.md)). Con el store en memoria, reiniciar el plano de control olvida las sandboxes y cada nodo borra sus discos; el plano de control lo avisa al arrancar. En un servidor que ya corre otras cosas (otra aplicación con su propio Postgres en el 5432, por ejemplo):

1. **Un Postgres propio de ASP**, no el de otra aplicación: un contenedor con los datos en un bind mount (en una partición con espacio, no en una `/var` pequeña), solo en loopback y con la política `unless-stopped`. La contraseña se genera en el momento y no se imprime:

   ```bash
   sudo install -d -o 999 -g 999 -m 700 /srv/asp/postgres
   PW=$(openssl rand -hex 24)
   printf 'POSTGRES_USER=asp\nPOSTGRES_DB=asp\nPOSTGRES_PASSWORD=%s\n' "$PW" \
     | sudo install -m 0600 -o root -g root /dev/stdin /etc/asp/postgres.env
   sudo docker run -d --name asp-postgres --restart unless-stopped --env-file /etc/asp/postgres.env \
     -p 127.0.0.1:5433:5432 -v /srv/asp/postgres:/var/lib/postgresql --memory 2g \
     --log-opt max-size=10m --log-opt max-file=3 postgres:18
   ```

   Un `docker volume prune` no toca un bind mount. CI prueba con Postgres 16. Con `docker compose up -d postgres` es más corto, pero deja la contraseña `asp` de ejemplo.
2. **`database_url` en un drop-in de la configuración del plano de control** (`/etc/asp/server.yaml.d/`, modo 0640 y del grupo `asp-control-plane`), nunca en la unit ni en el repositorio. Sin imprimir la contraseña:

   ```bash
   sudo install -m 0640 -o root -g asp-control-plane /dev/stdin /etc/asp/server.yaml.d/20-database.yaml <<EOF
   database_url: postgres://asp:$PW@127.0.0.1:5433/asp?sslmode=disable
   EOF
   unset PW
   ```

   La CA, la clave OIDC y la de atestación tienen que estar en almacenamiento persistente (`/var/lib/asp-control-plane`, por ejemplo): es lo que exige el modo producción (arranque con código 2 si apuntan a `/tmp`). Como Postgres es un contenedor y al arrancar el servidor puede tardar más que el plano de control, un drop-in de systemd (`/etc/systemd/system/asp-control-plane.service.d/postgres.conf`) lo hace esperar a Docker y reintentar sin tope: sin `StartLimitIntervalSec=0`, systemd se rinde tras 5 arranques fallidos en 10 s.

   ```ini
   [Unit]
   After=docker.service
   Wants=docker.service
   StartLimitIntervalSec=0

   [Service]
   RestartSec=5
   ```
3. **Reinicia el plano de control** sin sesiones activas: las migraciones se aplican solas (`using Postgres store` en el log). El node-agent se registra solo; las sandboxes que viviesen en memoria se pierden, y los discos de las que quedasen paradas los recoge el GC del nodo.
4. **Comprueba:** `asp sandbox list` vacío; arranca una sesión, páriala, `systemctl restart asp-control-plane`, y `asp session resume` debe funcionar y el disco seguir en `--disk-dir`.
5. **Copias:** `pg_dump` a un directorio de copias antes de desplegar migraciones nuevas.

Retención: `ASP_STOPPED_SANDBOX_TTL` (por defecto **7 días**; `0`/`off` conserva hasta borrar) y `ASP_MAX_STOPPED_PER_TENANT` (sin tope por defecto) acotan cuánto tiempo y cuántas sandboxes paradas guardan su disco ([`control-plane/README.md`](../../control-plane/README.md#retención-de-sandboxes-paradas)).

## 2. TLS, la CA y las credenciales

Los ajustes van en `/etc/asp/server.yaml.d/` (modo 0640, del grupo `asp-control-plane`; [el fichero de configuración](config-file.md)); las credenciales, en un fichero propio:

```yaml
# /etc/asp/server.yaml.d/10-site.yaml
listen_addr: ":8443"
oidc_issuer: https://cp.example.corp:8443     # cómo llegan a este plano de control quienes validan sus tokens
tls_cert: /var/lib/asp/certs/server.crt        # tu certificado TLS (de una CA que no firme nada más)
tls_key: /var/lib/asp/certs/server.key
ca_cert: /var/lib/asp/certs/ca.crt             # la CA de enrollment: firma los certificados de nodo (se crea si no existe)
ca_key: /var/lib/asp/certs/ca.key
client_ca: /var/lib/asp/certs/ca.crt           # la misma: cada ruta de nodo exige el certificado de ese nodo
egress_default_allow: false                    # un tenant sin reglas no alcanza nada
```

```yaml
# /etc/asp/server.yaml.d/20-secrets.yaml      (modo 0640; no se sube a ningún repositorio)
bootstrap_api_key: …secreto…        # la primera API key (ámbito platform): con ella se crean las demás con `asp apikey create`
node_bootstrap_token: …secreto…     # el token de enroll compartido; mejor, un token por nodo (`asp node enroll-token`)
```

- `tls_cert` y `tls_key` activan HTTPS. Con `client_ca` el servidor verifica el certificado de cliente **si lo hay** (`VerifyClientCertIfGiven`): el enroll sigue sin exigirlo, y register, heartbeat y el resto de rutas de nodo lo exigen por middleware y comparan su CN con el nodo para el que actúan. Con `ASP_MTLS_STRICT=1` el listener TLS lo exige siempre y el enroll va por otro listener (`ASP_ENROLL_LISTEN`, en claro y en loopback).
- La autenticación está siempre activa: sin ninguna API key y sin IdP el plano de control no arranca (código 2). `ASP_INSECURE_OPEN_API=1` es la salida explícita de un laboratorio.
- `ASP_AUTO_PROVISION` es `0` por defecto (el create deja la sandbox `requested` hasta que un nodo la reclama). **No lo pongas a `1` en un despliegue real**: la deja `running` sin ninguna VM.
- La CA, la clave OIDC y la de atestación (`ca_cert`, `ca_key`, `oidc_key`, `attest_key`) tienen que estar en almacenamiento persistente: es lo que exige el modo producción (el plano de control no arranca, código 2, si apuntan a `/tmp`, `/var/tmp`, `/dev/shm` o `$TMPDIR`). Las que falten se crean donde apuntes. Qué es el modo producción y cómo forzarlo en un laboratorio: `ASP_ALLOW_TMP_KEYS` en [la referencia](../reference/configuration/control-plane.md#nodos-ca-y-tls).
- Con un IdP: [conectar un IdP](idp.md). Los nodos de otros hosts, sus dos raíces de confianza y el firewall: [varios servidores](../ops-multi-node.md).

## 3. Arrancarlo y comprobar

```bash
sudo systemctl enable --now asp-control-plane.service
curl -fsS https://cp.example.corp:8443/healthz --cacert /var/lib/asp/certs/ca.crt   # ok
curl -sS -o /dev/null -w '%{http_code}\n' https://cp.example.corp:8443/v1/sandboxes --cacert /var/lib/asp/certs/ca.crt   # 401 sin credencial
```

Con Postgres, `journalctl -u asp-control-plane` dice `using Postgres store`; sin él, que el estado es en memoria y se pierde al reiniciar. Para desarrollar, sin systemd: `cd control-plane && go run ./cmd/api`.
