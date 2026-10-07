# Operación bare-metal con Cloud Hypervisor real (KVM)

Guía operativa para correr **agent-sandbox-platform** con Cloud Hypervisor (CH) real — **sin `--dry-run` / FakeVMM** — en un host Linux con KVM.

> Código **tal cual** (MVP solution-complete + hardening 2b–2e): multi-socket CH, vsock exec **26500**, host-vsock **26501/26502**, TAP auto, forward proxy + DNS sink, nft `asp_egress` soft|enforce, SSH guest auto, attest software, autodefensa del nodo/fence.

Smoke dry-run (sin KVM): [`mvp-smoke.md`](mvp-smoke.md). Arquitectura: [`architecture.md`](architecture.md). Roadmap: [`roadmap.md`](roadmap.md). Diagrama: [`diagram.svg`](diagram.svg).

### Precondiciones → resultado → fallos (bare-metal)

| | |
|---|---|
| **Precondiciones** | CPU con VT-x/AMD-V, `/dev/kvm` usable, usuario en grupo `kvm` (y `netdev` si crea TAP), binario `cloud-hypervisor`, `vmlinux` + `rootfs.img` en `/opt/sandbox/`, CAP_NET_ADMIN o root para TAP/nft enforce, CP alcanzable (TLS/mTLS). |
| **Resultado OK** | Sandbox `requested`→`running` vía reconciler; `asp sandbox exec` o `POST …/exec` devuelve stdout; guest tiene `SSH_AUTH_SOCK`; egress no allowlisted → 403; con nft enforce, dial directo 80/443/DNS no bypasea el proxy. |
| **Fallos típicos** | Sin KVM → CH aborta (usa dry-run); sin `--tap-auto` ni TAP manual → `vm.create` falla al abrir device; SoftFail nft → logs warn pero **no** hay frontera; un TAP que no se puede crear deja la sandbox en `failed`; `ASP_AUTO_PROVISION=1` → `running` mentiroso sin VMM; rootfs viejo sin `vsock-ssh-agent-proxy` → sin `SSH_AUTH_SOCK`; nft enforce sin `nft`/root → node-agent no arranca el redirect. |

---

## 1. Prerrequisitos

### Hardware / virtualización

| Requisito | Comprobación |
|---|---|
| CPU VT-x (Intel) o AMD-V | `grep -E 'vmx\|svm' /proc/cpuinfo` |
| Módulo KVM cargado | `lsmod \| grep kvm` |
| Nodo `/dev/kvm` usable | `ls -l /dev/kvm` (grupo `kvm`, modo `rw` para el usuario del node-agent) |
| `kvm-ok` (Ubuntu) | `sudo apt install cpu-checker && kvm-ok` |

**Nested virtualización:** si el host ASP es a su vez una VM (p. ej. lab en cloud), habilita nested en el hipervisor padre (`kvm-intel.nested=1` / `kvm-amd.nested=1`). Nested funciona para desarrollo; densidad y latencia son peores que bare metal. En producción corporativa preferir hosts físicos o VMs con passthrough de KVM ya validado.

### Paquetes Ubuntu / Debian

```bash
sudo apt update
sudo apt install -y \
  cpu-checker \
  qemu-utils \
  iproute2 \
  iptables \
  nftables \
  bridge-utils \
  curl \
  ca-certificates \
  jq \
  docker.io docker-compose-v2   # Postgres local vía compose
# Opcional: build de binarios ASP
# golang-go rustc cargo
```

El usuario del servicio node-agent debe pertenecer al grupo `kvm` (y normalmente `netdev` si crea TAPs):

```bash
sudo usermod -aG kvm,netdev "$USER"
# re-login o newgrp kvm
```

### Sin `/dev/kvm` en este entorno

Si faltan KVM/privilegios (CI, sandbox compartido, contenedor sin devices), **no** hace falta arrancar CH aquí: usa `--dry-run` + FakeVMM ([`mvp-smoke.md`](mvp-smoke.md)) y reserva esta guía para el host de lab/prod con KVM.

---

## 2. Instalar Cloud Hypervisor y assets guest

### 2.1 Binario `cloud-hypervisor`

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
sudo install -d -m 0755 /usr/local/bin /var/lib/asp/bin
curl -fsSL -o /tmp/cloud-hypervisor-static \
  "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VER}/cloud-hypervisor-static"
chmod +x /tmp/cloud-hypervisor-static
sudo mv /tmp/cloud-hypervisor-static /usr/local/bin/cloud-hypervisor
cloud-hypervisor --version
```

El flag `--ch-binary` / env `CLOUD_HYPERVISOR_BIN` apunta al binario que el node-agent **spawnea por sandbox** en el modo por defecto (`--ch-socket-dir`, sockets `/run/asp/ch-{sandboxID}.sock`). El override legacy `--ch-api-socket` sigue permitiendo un CH pre-arrancado (shared/debug) sin spawn. Ver §5.

### 2.2 Kernel + rootfs (virtio, vsock)

Necesitas:

1. **Kernel** Linux con virtio-blk, virtio-net, vsock (`CONFIG_VIRTIO_*`, `CONFIG_VHOST_VSOCK` / guest `CONFIG_VSOCKETS`). CH suele arrancar con un `vmlinux` (PVH) o firmware + kernel según perfil.
2. **Rootfs** raw/ext4 (o formato que CH acepte en `disks[].path`) con `pod-daemon` y red mínima.

Layout recomendado de assets bajo **`/var/lib/asp/`**, con symlinks a las rutas que el reconciler usa **hoy** (hardcodeadas):

| Ruta código (reconciler) | Uso |
|---|---|
| `/opt/sandbox/vmlinux` | `MicroVMConfig.KernelPath` |
| `/opt/sandbox/rootfs.img` | `MicroVMConfig.RootFSPath` |

```bash
sudo install -d -m 0755 \
  /var/lib/asp/{bin,images,kernels,certs,node-certs,run,tap} \
  /opt/sandbox \
  /run/asp \
  /run/cloud-hypervisor

# Ejemplo: copiar kernel/rootfs validados
sudo cp /path/to/vmlinux /var/lib/asp/kernels/vmlinux
sudo cp /path/to/rootfs.img /var/lib/asp/images/rootfs.img
sudo ln -sfn /var/lib/asp/kernels/vmlinux /opt/sandbox/vmlinux
sudo ln -sfn /var/lib/asp/images/rootfs.img /opt/sandbox/rootfs.img
```

Fuentes típicas de kernel/rootfs:

- Kernel branch Cloud Hypervisor (`ch_*` defconfig) → `vmlinux`.
- Rootfs: exportar la imagen OCI de [`images/guest`](../images/guest/) a ext4 (pipeline futuro), o una cloud image mínima convertida a raw (`qemu-img convert`) e inyectar `pod-daemon`.

**Cmdline** que envía el reconciler hoy:

```text
console=ttyS0 root=/dev/vda reboot=k panic=1
```

(Ajusta el guest para que `/dev/vda` sea el rootfs virtio-blk.)

### 2.3 Layout `/var/lib/asp/` (convención ops)

```text
/var/lib/asp/
  bin/                 # copias locales de cloud-hypervisor si no usas /usr/local/bin
  kernels/vmlinux
  images/rootfs.img
  certs/               # CA / TLS servidor del control-plane (corp)
  node-certs/          # default ASP_CERT_DIR del node-agent
  disks/               # --disk-dir: rootfs-{sandboxID}.img, copia privada por sandbox
  local-net/           # ASP_LOCAL_NET_KEY_DIR: clave WireGuard {sandboxID}.key por sesión local-net
  tap/                 # scripts/state de TAP (opcional)
/run/asp/              # sockets runtime: ch-{sandboxID}.sock (+ .lock de CH) + vsock-{sandboxID}.sock; node-agent.lock (§5.6)
/run/cloud-hypervisor/api.sock   # opcional: --ch-api-socket shared/legacy/debug
/opt/sandbox/{vmlinux,rootfs.img} → symlinks a /var/lib/asp/...
```

---

## 3. Red: TAP, NAT, DNS / egress

### 3.1 Modelo (ADR-0002)

Cada microVM debería tener **TAP + NAT en el host**. Egress HTTP/DNS deny-by-default vía proxies del nodo. El guest **no** recibe `NET_ADMIN`.

**Hoy en código:**

- El reconciler pone `TapDevice: "asp-" + shortID(sandbox_id)` (8 primeros chars del UUID) en el `vm.create` de CH.
- **`--tap-auto` / `ASP_TAP_AUTO=1`:** el reconciler crea el TAP (`ip tuntap add` + `link set up` + `addr add <host>/30`) antes de Start y lo borra en Stop. Cada sandbox recibe su propia /30 de `--guest-subnet` (default `10.200.0.0/16`): el TAP lleva la `.1` de esa /30 y el guest la `.2`, que el kernel configura con `ip=` en la cmdline (`CONFIG_IP_PNP`). Si un paso falla (sin `CAP_NET_ADMIN`, o el nombre ya existe) la sandbox pasa a `failed` con el motivo `tap: …` y se borra el TAP a medio crear; la VM no arranca sin red. Solo `--dry-run` tolera el fallo (warning y sigue).
- Sin `--tap-auto`, prepáralo a mano (sketch abajo) o el create fallará al abrir el device.
- Allowlist de tenant (`PUT /v1/tenants/{id}/egress`) + check API; **forward proxy HTTP(S)** (`--egress-proxy-listen`) y **DNS sink** (`--egress-dns-sink`) están listos (fase 2b). Guest: `HTTP_PROXY` → su gateway (la IP del TAP en su /30):8888.
- **nft redirect anti-bypass** (`--nft-egress-redirect`, modo `soft|enforce`) fuerza HTTP(S)+DNS por el proxy/sink (fase 2e, §8e). Tabla `asp_egress`.
- **Deny-by-default en nft:** con `--nft-egress-redirect`, la tabla `asp_egress` descarta todo lo que el guest manda salvo HTTP(S) y DNS redirigidos al proxy/sink: otros puertos, otros guests, servicios del host y orígenes falsificados. Solo se reenvía el túnel propio de una sesión local-net (`wg-asp-*`).
- **NAT/MASQUERADE** (`asp_nat`, §3.3) ya no hace falta para el egress público: el proxy sale desde el host. Si lo tienes de antes, el drop de `asp_egress` sigue mandando.

### 3.2 Sketch: crear TAP + IP host (manual o referencia de `--tap-auto`)

```bash
#!/usr/bin/env bash
# /var/lib/asp/tap/setup-tap.sh <tap-name> <host-ip/cidr>
set -euo pipefail
TAP="${1:?tap name}"   # reconciler: asp-<8chars>
HOST_CIDR="${2:-10.200.0.1/30}"   # --tap-auto: una /30 distinta por sandbox

sudo ip tuntap add dev "$TAP" mode tap user "$(id -un)"
sudo ip link set "$TAP" up
sudo ip addr add "$HOST_CIDR" dev "$TAP" 2>/dev/null || true
# --tap-auto pasa ip=<guest>::<host>:255.255.255.252::eth0:off en la cmdline.
```

Con `--tap-auto` el node-agent ejecuta el equivalente (usuario del proceso; suele necesitar capabilities o root).

### 3.3 Sketch: nftables MASQUERADE (NAT saliente)

```bash
#!/usr/bin/env bash
# Minimal NAT: guest subnet → interfaz de egress del host (ej. eth0)
set -euo pipefail
EGRESS_IF="${1:-eth0}"
GUEST_NET="${2:-10.200.0.0/24}"

sudo sysctl -w net.ipv4.ip_forward=1
sudo nft -f - <<NFT
table inet asp_nat {
  chain postrouting {
    type nat hook postrouting priority 100;
    ip saddr $GUEST_NET oifname "$EGRESS_IF" masquerade
  }
  chain forward {
    type filter hook forward priority 0; policy drop;
    iifname "tap-*" oifname "$EGRESS_IF" accept
    iifname "$EGRESS_IF" oifname "tap-*" ct state related,established accept
  }
}
NFT
```

Esto da **conectividad IP mínima**. No sustituye el proxy deny-default.

### 3.4 DNS / egress proxy (HTTP forward + DNS sink)

| Capa | Estado |
|---|---|
| Allowlist API + check en CP/node | **Listo** (fase 1d) |
| `--egress-enforce` → 403 en check denegado | **Listo** |
| `--egress-proxy-listen :8888` forward proxy (CONNECT + HTTP) | **Listo** (post-MVP) |
| `--egress-dns-sink :5353` NXDOMAIN non-allowlisted | **Listo** (opcional) |
| TAP create/delete | **`--tap-auto`** (soft-fail sin perms) |
| NAT + nftables host | **Ops manual** (esta sección) |
| nft redirect HTTP+DNS (`asp_egress`) | **Fase 2e** (`--nft-egress-redirect --nft-egress-mode=enforce`) |

#### Guest → host TAP proxy

El forward proxy escucha en el host (`:8888`). En el guest, apunta al gateway de su /30 (la `.1`; para el primer sandbox `10.200.0.1`):

```bash
# Sustituye 10.200.0.1 por el gateway del guest (ip route | grep default)
export HTTP_PROXY=http://10.200.0.1:8888
export HTTPS_PROXY=http://10.200.0.1:8888
export NO_PROXY=localhost,127.0.0.1
```

Allowlist efectiva (deny-by-default). El proxy y el DNS sink identifican el sandbox por la **IP de origen** (la /30 de su TAP); nunca leen cabeceras que escribe el guest:

1. Origen dentro de la /30 de un sandbox → el `egress_allowlist` que el CP adjuntó al último exec **de ese sandbox**. Antes del primer exec: deny.
2. Origen que no es un sandbox → env `ASP_EGRESS_ALLOWLIST_JSON` del node-agent.
3. Si no, default deny del proceso.

`X-ASP-Allowlist-JSON` y `X-ASP-Sandbox-ID` se borran de la petición reenviada y no cambian la decisión.

**Destinos que el proxy nunca marca.** La allowlist es del tenant y nombra hosts, pero un nombre puede resolver a cualquier cosa, también al propio nodo. El proxy corre en el netns del host, así que sin más un guest podría apuntar un nombre suyo a `127.0.0.1` y usarlo para llegar al API local del node-agent (exec sin autenticar en `:9100`), a los metadatos de la nube (`169.254.169.254`) o a la LAN del nodo. Por eso, tras resolver el nombre y sobre la dirección a la que va a conectar, el proxy rechaza (**403**, audit `destination_blocked`) loopback, link-local, no especificadas, multicast, rangos reservados, las direcciones del propio nodo y la red de los guests (`--guest-subnet`), aunque la allowlist del tenant diga que sí. Las redes privadas (RFC 1918, ULA, CGNAT) tampoco, salvo las que el operador abra con `--egress-allow-cidr` (p. ej. `--egress-allow-cidr=10.50.0.0/16` para un mirror interno); un tenant no puede abrirlas.

**Puertos.** Una regla sin `port` vale para 80 y 443; cualquier otro puerto necesita una regla que lo nombre (`{"host_pattern":"git.example.com","port":22}`). Una regla sin puerto dejaba pasar `CONNECT host:22`, `:5432`… a todo lo que el host sirviera.

Deny → HTTP **403**. El proxy se arranca con `--egress-proxy-listen` (recomendado junto a `--egress-enforce`). Hardening: rate-limit token-bucket por host/sandbox, límite del body de las peticiones (`ASP_EGRESS_MAX_BODY`, 413; las respuestas no se cortan), deny de schemes no-HTTP, audit JSON. MITM CONNECT bump **off** por defecto; solo con `--egress-mitm` / `ASP_EGRESS_MITM=1` + `--egress-mitm-ca` (corp caution).

#### DNS

**Opción A (recomendada con proxy HTTP):** usar solo el proxy para HTTP(S); bloquear UDP/53 saliente del guest hacia resolvers públicos con nftables (ops) para que el guest no bypassée por DNS directo a IPs.

**Opción B:** `--egress-dns-sink=:5353` — stub UDP que resuelve (LookupIP) solo hostnames allowlisted y responde **NXDOMAIN** al resto. Apunta `resolv.conf` del guest a la IP TAP del host (puerto 5353 vía DNAT, o escucha en `:53` si tienes CAP_NET_BIND_SERVICE).

```bash
# node-agent (ejemplo)
--egress-enforce \
--egress-proxy-listen=0.0.0.0:8888 \
--egress-dns-sink=0.0.0.0:5353
```

Camino mínimo recomendado hoy:

1. NAT (§3.3) para que el guest tenga ruta al proxy (no hace falta full Internet).
2. Allowlist del tenant + `ASP_EGRESS_DENY_DEFAULT=1`.
3. Node-agent `--egress-enforce --egress-proxy-listen=…` (+ DNS sink opcional).
4. Guest: `HTTP_PROXY`/`HTTPS_PROXY` → IP TAP host:8888.
5. (Endurecimiento) nft bloquear forward directo guest→WAN excepto hacia el proxy.

---

## 4. Control-plane

### 4.1 Postgres (compose)

Desde la raíz del repo:

```bash
docker compose up -d postgres
export DATABASE_URL='postgres://asp:asp@127.0.0.1:5432/asp?sslmode=disable'
```

Migraciones `001`–`020` se aplican al arrancar el API si `DATABASE_URL` está set (init, enrollment, egress, leases, attestation/fence, cert rotation, multi-user, idle, workspace, local-net, atributos de planificación del nodo, `agent_instance_id`, scope de API keys, tokens de enroll, caducidad del cert de nodo, túnel local-net asignado por el nodo, discos retenidos al parar — `deleting`/`deleted`, `boot_count`, `stopped_at`, `status_detail` —, espacio libre de disco del nodo, `booted_at`).

**Postgres es requisito para que parar conserve el disco** ([ADR-0012](adr/0012-retained-disks.md)). Con el store en memoria, reiniciar el plano de control olvida las sandboxes y cada nodo borra sus discos; el plano de control lo avisa al arrancar. En un servidor que ya corre otras cosas (ncc1701d comparte Docker con otra aplicación):

1. **Un Postgres propio de ASP**, no el de otra aplicación (en ncc1701d `infra-db-1` ya ocupa el `5432`). Lo que se hizo allí el 2026-10-07, con la contraseña generada en el momento y sin imprimirla:

   ```bash
   sudo install -d -o 999 -g 999 -m 700 /sandbox/asp-postgres        # datos fuera de /var (2,9 GB)
   umask 077
   printf 'POSTGRES_USER=asp\nPOSTGRES_DB=asp\nPOSTGRES_PASSWORD=%s\n' "$(openssl rand -hex 24)" > ~/.secrets/asp-postgres.env
   docker run -d --name asp-postgres --restart unless-stopped --env-file ~/.secrets/asp-postgres.env \
     -p 127.0.0.1:5433:5432 -v /sandbox/asp-postgres:/var/lib/postgresql --memory 2g \
     --log-opt max-size=10m --log-opt max-file=3 postgres:18
   ```

   Solo en loopback, con política `unless-stopped` y los datos en un bind mount (un `docker volume prune` no los toca). La imagen `postgres:18` ya estaba en el servidor; CI usa la 16. Con `docker compose up -d postgres` en una máquina sin otra base de datos es más corto, pero cambia la contraseña `asp` por una generada.
2. **`DATABASE_URL` en el fichero de secretos** que ya carga la unit del plano de control (`~/.secrets/asp-idp.env`, modo `0600`), nunca en la unit ni en el repo: `DATABASE_URL=postgres://asp:<contraseña>@127.0.0.1:5433/asp?sslmode=disable`. Las claves ya viven en `/var/lib/asp-control-plane`, que es lo que exige el modo producción (arranque con código 2 si apuntan a `/tmp`). Como Postgres es un contenedor y al arrancar el servidor puede tardar más que el plano de control, un drop-in (`/etc/systemd/system/asp-control-plane.service.d/postgres.conf`) lo hace esperar a Docker y reintentar sin tope: sin `StartLimitIntervalSec=0`, systemd se rinde tras 5 arranques fallidos en 10 s.

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
5. **Copias:** `pg_dump` a `~/asp-backup-<fecha>/` antes de desplegar migraciones nuevas.

Retención: `ASP_STOPPED_SANDBOX_TTL` (por defecto **7 días**; `0`/`off` conserva hasta borrar) y `ASP_MAX_STOPPED_PER_TENANT` (sin tope por defecto) acotan cuánto tiempo y cuántas sandboxes paradas guardan su disco ([`control-plane/README.md`](../control-plane/README.md#retención-de-sandboxes-paradas)).

### 4.2 TLS + client CA + bootstrap

```bash
# Persistencia de CA de enrollment (corp: fuera de /tmp)
export ASP_CA_CERT=/var/lib/asp/certs/ca.crt
export ASP_CA_KEY=/var/lib/asp/certs/ca.key

# TLS servidor
export ASP_TLS_CERT=/var/lib/asp/certs/server.crt
export ASP_TLS_KEY=/var/lib/asp/certs/server.key
# Misma CA de enrollment como client CA (VerifyClientCertIfGiven)
export ASP_CLIENT_CA=/var/lib/asp/certs/ca.crt

export ASP_BOOTSTRAP_API_KEY='…secreto-tenant…'     # Bearer API
export ASP_NODE_BOOTSTRAP_TOKEN='…secreto-nodo…'    # enroll
# (la autenticación está siempre activa: no hay variable que "forzarla")
export ASP_EGRESS_DENY_DEFAULT=1
export ASP_AUTO_PROVISION=0                         # obligatorio en bare-metal real
export ASP_OIDC_ISSUER='https://cp.ejemplo.corp:8443'
export LISTEN_ADDR=:8443

(cd control-plane && go run ./cmd/api)
# o binario empaquetado + systemd
```

Lab IdP (Keycloak realm `asp`, secretos en `~/.secrets/`, unit `asp-control-plane` en `127.0.0.1:18112`): ver [`ops-idp-keycloak-lab.md`](ops-idp-keycloak-lab.md) y plantilla [`scripts/systemd/asp-control-plane.service`](../scripts/systemd/asp-control-plane.service).


Notas TLS (código actual):

- `ASP_TLS_CERT` + `ASP_TLS_KEY` activan HTTPS.
- Con `ASP_CLIENT_CA`: `ClientAuth = VerifyClientCertIfGiven` (enroll sigue sin exigir client cert; register/heartbeat/oidc mint sí vía middleware).
- `ASP_AUTO_PROVISION=0` (default): Create deja `requested` para el reconciler. **No** uses `=1` en prod (stub sync → `running` sin VMM).

### 4.3 Health

```bash
curl -fsS https://127.0.0.1:8443/healthz --cacert /var/lib/asp/certs/ca.crt
# o HTTP lab: curl -fsS http://127.0.0.1:8080/healthz
```

---

## 5. Node-agent + Cloud Hypervisor

### 5.1 Cómo habla el cliente con CH (código real)

`internal/vmm/cloudhypervisor.go`:

- **Modo por defecto (per-sandbox):** `Start(sandbox)` spawnea
  `cloud-hypervisor --api-socket /run/asp/ch-{sandboxID}.sock` (`--ch-socket-dir`, default `/run/asp`),
  espera a que el socket acepte `vmm.ping`, luego `vm.create` + `vm.boot`.
  `Stop(id)` hace `vm.delete`, mata el proceso y borra el socket.
- **Modo shared/legacy:** si `--ch-api-socket` / `CH_API_SOCKET` está set, no spawnea;
  habla con un CH ya arrancado en ese socket (un VM a la vez; útil para debug).
- Rutas: `GET /api/v1/vmm.ping`, `PUT /api/v1/vm.create`, `PUT /api/v1/vm.boot`, `PUT /api/v1/vm.delete`, `PUT /api/v1/vm.pause`.
- `--dry-run` sigue usando `FakeVMM` (sin CH).

Flags relevantes (`cmd/node-agent/main.go`):

| Flag | Env | Default / notas |
|---|---|---|
| `--ch-socket-dir` | `CH_SOCKET_DIR` | `/run/asp` — sockets `ch-{sandboxID}.sock`; **default** cuando no dry-run. El agente lo crea con modo `0700` y aprieta uno existente que no sea de sistema (`/run`, `/tmp`…): los sockets de dentro dan autoridad sobre cada sandbox (exec como root en el guest, tokens de identidad, vhost-user de virtiofsd) |
| `--ch-api-socket` | `CH_API_SOCKET` | vacío — si set, override shared/legacy (sin spawn) |
| `--ch-binary` | `CLOUD_HYPERVISOR_BIN` | `cloud-hypervisor` — binario spawneado por sandbox |
| `--dry-run` | `DRY_RUN=1` | **omitir** en bare-metal real |
| `--disk-dir` | `ASP_DISK_DIR` | `/var/lib/asp/disks` — `rootfs-{sandboxID}.img` por sandbox. Parar la conserva si el plano de control manda `retained` ([ADR-0012](adr/0012-retained-disks.md)); borrar la sandbox la borra. Un GC del nodo borra las copias que ninguna sandbox reclama |
| `--stop-grace` | `ASP_STOP_GRACE` | `15s` — al parar conservando el disco, espera a que el guest se apague solo antes de la parada brusca |
| `--disk-min-free-mib` | `ASP_DISK_MIN_FREE_MIB` | `-1` (el doble de la imagen base): espacio libre mínimo en `--disk-dir` para clonar o reanudar; `0` no comprueba |
| `--reap-leftovers` | `ASP_REAP_LEFTOVERS` | `on` — al arrancar, borra lo que dejó un node-agent anterior (§5.6); `report` solo lo lista; `off` |
| `--reap-only` | | hace solo esa limpieza y sale (`ExecStopPost` de la unit) |
| `--print-measurement` | | imprime el SHA-256 del kernel y de la imagen base y la versión del hipervisor como entrada de `ASP_ATTEST_ALLOWED_IMAGES` del control plane, y sale |
| `--reconcile` | `ASP_RECONCILE=1` | poll work / claim / Start-Stop |
| `--enroll` | `ASP_ENROLL=1` | + `--bootstrap-token` |
| `--cert-dir` | `ASP_CERT_DIR` | `/var/lib/asp/node-certs` |
| `--mtls` | `ASP_MTLS=1` | client certs hacia CP |
| `--agent-listen` | `ASP_AGENT_LISTEN` | `127.0.0.1:9100` — API local; pide un bearer token (`--agent-token-file`) salvo `/healthz`; fuera de loopback no arranca salvo `--insecure-agent-listen` |
| `--agent-token-file` | `ASP_AGENT_TOKEN_FILE` | secreto de esa API; el nodo lo crea (0600) y el plano de control del mismo host lo lee con el mismo `ASP_AGENT_TOKEN_FILE`. Defecto `/var/lib/asp/agent.token` |
| `--workspace-root` | `ASP_WORKSPACE_ROOTS` | donde puede vivir el workspace de una sandbox: dentro de `<raíz>/<tenant>/` (enlaces resueltos). Defecto `/srv/asp/workspaces` (créalo: `install -d /srv/asp/workspaces/<tenant>`); sin raíz que exista no hay workspaces. `virtiofsd` corre con `--sandbox chroot` si el agente es root (`--virtiofsd-sandbox`) |
| `--vm-confine` | `ASP_VM_CONFINE` | `auto` (por defecto), `on` o `off`. Cada microVM y su `virtiofsd` corren en un servicio systemd transitorio propio (`asp-vm-<id>`, `asp-vm-<id>-fs`) con su cgroup y sus límites; con `auto`, cuando el host puede (root y systemd) y, si no, como hijos del agente diciendo por qué; `on` no arranca si no puede; `off` es el comportamiento anterior |
| `--vm-slice` | `ASP_VM_SLICE` | slice de esos servicios (`asp-vms.slice`) |
| `--vm-memory-overhead-mib` | `ASP_VM_MEMORY_OVERHEAD_MIB` | memoria que se suma a la del guest en el `MemoryMax` del servicio (por defecto 256) |
| `--vm-cpu-overhead-percent` | `ASP_VM_CPU_OVERHEAD_PERCENT` | porcentaje de una CPU que se suma a las vCPU en el `CPUQuota` (por defecto 50) |
| `--vm-tasks-max` | `ASP_VM_TASKS_MAX` | procesos e hilos máximos de un servicio de VM (por defecto 1024) |
| `--capacity-cpu` / `--capacity-mem-mib` | `ASP_CAPACITY_CPU` / `ASP_CAPACITY_MEM_MIB` | `-1` detecta del host, `0` no limita ([`ops-multi-node.md`](ops-multi-node.md)) |
| `--max-sandboxes` | `ASP_MAX_SANDBOXES` | `0` = sin tope |
| `--local-net-dial` | `ASP_LOCAL_NET_DIAL` | dirección que marca el portátil para local-net en este nodo |
| `--agent-tls-listen` | `ASP_AGENT_TLS_LISTEN` | vacío — p.ej. `0.0.0.0:9443`: `exec` con mTLS para un CP en otro host (ver 5.5) |
| `--endpoint` | `NODE_ENDPOINT` | lo que se anuncia al CP; por defecto `https://<hostname>:<puerto>` con `--agent-tls-listen` |
| `--control-plane-ca` | `ASP_CONTROL_PLANE_CA` | CA del cert TLS del CP (enroll y llamadas); por defecto `cert-dir/ca.crt`, la misma CA que firma los certificados de nodo: avisa si el CP es remoto. Pasa una CA que firme solo el cert del CP ([`ops-multi-node.md`](ops-multi-node.md#las-dos-raíces-de-confianza)) |
| `--enroll-url` | `ASP_ENROLL_URL` | URL de enroll si no es `--control-plane-url` (`ASP_MTLS_STRICT`) |
| `--pod-daemon-sock` | `ASP_POD_DAEMON_SOCK` | unix del pod-daemon (**host**, dry-run / fallback) |
| `--pod-daemon-port` | | `26500` — puerto guest vsock para CONNECT |
| `--egress-enforce` | `ASP_EGRESS_ENFORCE=1` | 403 en egress-check; intent para proxy |
| `--egress-proxy-listen` | `ASP_EGRESS_PROXY_LISTEN` | p.ej. `:8888` forward proxy HTTP(S) |
| `--egress-dns-sink` | `ASP_EGRESS_DNS_SINK` | p.ej. `:5353` UDP NXDOMAIN non-allowlisted |
| `--egress-allow-cidr` | `ASP_EGRESS_ALLOW_CIDRS` | redes privadas (CIDR o dirección, separadas por comas) a las que el proxy puede conectar, además de Internet. Loopback, link-local, el propio nodo y la red de los guests nunca |
| `--tap-auto` | `ASP_TAP_AUTO=1` | crea/borra `asp-{shortid}` en Start/Stop |
| `--host-vsock` | `ASP_HOST_VSOCK=1` | AF_VSOCK 26501 SSH + 26502 identity (guest→CID 2) |
| `--host-vsock-dir` | `ASP_HOST_VSOCK_DIR` | lab: unix `host-vsock-{port}.sock` en vez de AF_VSOCK |
| `--ssh-agent-bridge` | `ASP_SSH_AGENT_BRIDGE` | unix bridge + symlinks `ssh-agent-{id}.sock` |
| `--identity-listen` | `ASP_IDENTITY_LISTEN` | unix/TCP identity sin binding de sandbox: 403 salvo `--insecure-identity-sandbox-header` (lab). Los guests usan `{vsock}_26502` |

### 5.2 Arrancar CH (per-sandbox vs shared)

Un proceso CH = **una** VM (modelo OpenAPI de CH).

**Default — el node-agent spawnea por sandbox** (no hace falta systemd de CH). Los CH son hijos del agente: córrelo con la unit de §5.6 para que un reinicio no los deje huérfanos.

```bash
sudo install -d -m 0700 /run/asp   # solo root; el agente también lo deja así al arrancar
# El reconciler, al Start, ejecuta:
#   cloud-hypervisor --api-socket /run/asp/ch-{sandboxID}.sock
# y diala Ping → CreateVM → Boot. En Stop: Delete → kill → rm socket.
```

**Override shared/debug** — CH pre-arrancado + `--ch-api-socket`:

```bash
sudo install -d -m 0755 /run/cloud-hypervisor
sudo rm -f /run/cloud-hypervisor/api.sock
cloud-hypervisor --api-socket /run/cloud-hypervisor/api.sock &
curl --unix-socket /run/cloud-hypervisor/api.sock \
  http://localhost/api/v1/vmm.ping

# node-agent con:
#   --ch-api-socket=/run/cloud-hypervisor/api.sock
# (un sandbox a la vez en ese socket)
```

### 5.3 Enroll + reconciler (bare-metal)

```bash
export CONTROL_PLANE_URL='https://cp.ejemplo.corp:8443'
export ASP_NODE_BOOTSTRAP_TOKEN='…'
export ASP_CERT_DIR=/var/lib/asp/node-certs
export ASP_MTLS=1
export ASP_RECONCILE=1
export ASP_EGRESS_ENFORCE=1
export CH_SOCKET_DIR=/run/asp
# Bare-metal: no hace falta ASP_POD_DAEMON_SOCK (hybrid vsock). Dry-run sí.
# export ASP_POD_DAEMON_SOCK=/tmp/pod-daemon.sock

(cd node-agent && go run ./cmd/node-agent \
  --control-plane-url="$CONTROL_PLANE_URL" \
  --node-id="$(hostname -s)" \
  --ch-socket-dir="$CH_SOCKET_DIR" \
  --ch-binary=/usr/local/bin/cloud-hypervisor \
  --enroll --bootstrap-token="$ASP_NODE_BOOTSTRAP_TOKEN" \
  --cert-dir="$ASP_CERT_DIR" --mtls \
  --agent-listen=127.0.0.1:9100 \
  --reconcile --reconcile-interval=2s \
  --egress-enforce \
  --tap-auto \
  --host-vsock \
  --ssh-agent-bridge=/run/asp/ssh-agent.sock)
# sin --dry-run; sin --ch-api-socket → spawn per-sandbox
# identity del guest: {vsock}_26502 por sandbox (--host-vsock --reconcile); sin --identity-listen
```

En modo shared (`--ch-api-socket` set), si CH no escucha: warning `CH ping failed (is cloud-hypervisor running with --api-socket?)`. En modo per-sandbox no hay ping al arrancar (el Ping ocurre dentro de cada `Start`).

### 5.4 Multi-sandbox (hecho) y gaps restantes

| Comportamiento | FakeVMM (`--dry-run`) | CH real (código actual) |
|---|---|---|
| Sandboxes concurrentes | Sí (`Running[id]`) | **Sí** — un proceso CH + socket por sandbox (`--ch-socket-dir`) |
| Socket | N/A | `/run/asp/ch-{sandboxID}.sock` (o shared si `--ch-api-socket`) |
| Spawn CH por sandbox | N/A | **Implementado** (`--ch-binary`) |
| `Stop(id)` | Borra por id | `vm.delete` + kill proceso + rm socket de ese id |
| `VsockCID` | 3 | **Único** por sandbox (allocator desde CID 3; path `/run/asp/vsock-{id}.sock`) |

**Hecho en este parche:** spawn multi-socket por sandbox + `Stop(id)` alineado.

**Hecho en 1h:** CIDs vsock únicos + hybrid dialer exec (§6).

**Hecho en MVP solution-complete:** `--tap-auto` (`asp-{shortID}`); `--host-vsock` (26501/26502); symlinks SSH por sandbox.

**Gaps restantes:** TPM/SEV hardware attest; fencing BMC de producción validado end-to-end; bypass-proof nft solo demostrable con TAP/KVM real (CI = soft/dry-run). SSH guest auto + nft completo: §8e / ADR-0006. Cert rotation, mTLS strict, SSH confirm: §8d / ADR-0005.

Modo shared (`--ch-api-socket`) sigue siendo un sandbox a la vez — solo para debug.

---

### 5.5 Plano de control en otro host (mTLS en los dos sentidos)

Con el plano de control en otra máquina, el `exec` no puede ir al `--agent-listen` de loopback. El nodo abre `--agent-tls-listen` y el plano de control lo llama con mTLS ([ADR-0011](adr/0011-multi-node.md)):

```bash
# En el nodo: el cert de enroll sirve también como cert de servidor.
node-agent \
  --control-plane-url=https://cp.ejemplo.corp:8443 \
  --control-plane-ca=/etc/asp/cp-ca.pem \
  --enroll --bootstrap-token="$ASP_NODE_BOOTSTRAP_TOKEN" \
  --cert-dir=/var/lib/asp/node-certs --mtls \
  --agent-listen=127.0.0.1:9100 \
  --agent-tls-listen=0.0.0.0:9443 \
  --endpoint=https://node1.ejemplo.corp:9443 \
  --reconcile
```

- **Firewall:** el plano de control → el nodo, `9443/tcp`. El nodo → el plano de control, su puerto TLS. Nada más hacia `9100`.
- **El plano de control** comprueba que el cert del agente es de su CA y nombra al nodo (`ServerName` = node id), y presenta el suyo (CN `asp-control-plane`). El agente no acepta ningún otro certificado.
- **Nodos enrolados antes de este cambio:** su cert es solo de cliente. Con `--agent-tls-listen` el agente no arranca y pide re-enrolar (`--enroll`) o `POST /v1/nodes/{id}/rotate-cert`.
- **`http://` desde otro host** se rechaza en register (400) y en `exec` (502). Solo para laboratorio: `ASP_INSECURE_AGENT_HTTP=1` en el plano de control y `--insecure-agent-listen` en el nodo.
- **Identidad:** con `ASP_CLIENT_CA`, cada ruta de nodo compara el CN del cert con el nodo para el que actúa (403 si no coincide). Sin `--node-id`, el agente usa el CN de su cert.
- **Dos raíces de confianza:** la CA de enrollment firma los certificados de nodo; el certificado TLS del plano de control debería venir de otra CA y el nodo la fija con `--control-plane-ca`. El certificado de un nodo vale solo para su id (no para el endpoint que cite ni para el nombre del plano de control) y un id no puede ser una IP ni un nombre del certificado del plano de control ([`ops-multi-node.md`](ops-multi-node.md#las-dos-raíces-de-confianza)).

### 5.6 Servicio systemd y reinicios del agente

El node-agent guarda sus VMs solo en memoria y un proceso nuevo no las adopta: al registrarse con otro `agent_instance_id`, el plano de control pasa sus sandboxes `running`/`paused` a `stopped` con `stop_reason=node_agent_restarted`: la VM murió con el proceso, pero su disco sigue en el nodo y se conserva (`asp session resume` la arranca de nuevo; [ADR-0011](adr/0011-multi-node.md), [ADR-0012](adr/0012-retained-disks.md)). Lo que dejara el proceso anterior en el host ya no es de nadie. Dos defensas:

**1. La unit** [`scripts/systemd/asp-node-agent.service`](../scripts/systemd/asp-node-agent.service), con `KillMode=control-group`: al parar o reiniciar el servicio, y si el agente muere, systemd mata con él todos sus `cloud-hypervisor` y `virtiofsd`. Después, `ExecStopPost=-node-agent --reap-only` borra lo que tenían.

**Con confinamiento (`--vm-confine`, por defecto en un host con systemd y root)** cada VMM y cada `virtiofsd` corren en un servicio transitorio propio, así que ya no están en el cgroup del agente y `KillMode` no los alcanza. Lo sustituye el propio servicio de la VM: nace con `BindsTo=asp-node-agent.service`, de modo que systemd lo para cuando el agente para o muere (misma garantía: ninguna VM queda corriendo para nadie), con `TimeoutStopSec=15`. `ExecStopPost=--reap-only` sigue limpiando lo demás. Un agente lanzado a mano (no es un servicio) no ata sus VMs a nada: el siguiente arranque las limpia, como antes.

```bash
systemctl list-units 'asp-vm-*'               # una VM = asp-vm-<id>.service (y -fs si tiene workspace)
systemd-cgls /asp.slice                         # su cgroup, bajo asp-vms.slice
systemctl show asp-vm-<id> -p MemoryMax -p CPUQuotaPerSecUSec -p TasksMax
journalctl -u asp-vm-<id>                       # la salida de cloud-hypervisor, que antes se perdía
```

`MemoryMax` es la memoria de la VM más `--vm-memory-overhead-mib`; `CPUQuota`, sus vCPU más `--vm-cpu-overhead-percent`. Una VM que sobrepase su `MemoryMax` la mata el kernel: la sandbox falla, el resto del host no lo nota. El VMM arranca además con `--seccomp true` explícito (es su valor por defecto; así un cambio de ese defecto no apaga el filtro).

```bash
sudo install -m 0755 build/node-agent /usr/local/bin/node-agent
sudo install -d -m 0750 /etc/asp
sudo install -m 0600 /dev/null /etc/asp/node-agent.env
sudoedit /etc/asp/node-agent.env   # CONTROL_PLANE_URL, ASP_CONTROL_PLANE_CA, NODE_ENDPOINT… (ejemplo en la unit)
sudo cp scripts/systemd/asp-node-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now asp-node-agent.service
journalctl -u asp-node-agent -f
```

Primer arranque: añade `ASP_ENROLL=1` y `ASP_NODE_BOOTSTRAP_TOKEN=…` al env file y quítalos cuando el journal diga `enrolled`; el token no debe quedarse en el nodo. Las líneas del env file mandan sobre las `Environment=` de la unit.

Las dos unidades reintentan el arranque **sin tope** (`StartLimitIntervalSec=0`, cada 5 s): el agente sale si no puede registrarse, y con el límite por defecto de systemd (5 arranques en 10 s) un plano de control que tarde en responder al arrancar el servidor dejaría el nodo caído hasta arrancarlo a mano. Un nodo de laboratorio con el plano de control en el mismo host (HTTP por loopback, sin mTLS ni enroll) usa [`scripts/systemd/asp-node-agent-lab.service`](../scripts/systemd/asp-node-agent-lab.service), descrita en [`ops-idp-keycloak-lab.md`](ops-idp-keycloak-lab.md#systemd--asp-node-agentservice).

**2. Limpieza al arrancar** (`--reap-leftovers=on`, por defecto). Antes de abrir ningún socket y antes de registrarse, el agente busca lo que dejó un proceso anterior y lo borra. Cubre lo que la unit no evita: un agente lanzado a mano o una unit con `KillMode=process`. **Los discos no entran aquí**: una sandbox parada conserva el suyo ([ADR-0012](adr/0012-retained-disks.md)) y el reaper no sabría distinguirlo de un resto. Tras el primer sondeo de `/work`, el reconciler borra las copias de `--disk-dir` que no son de ninguna sandbox (ni asignada, ni retenida, ni en borrado).

| Resto | Cómo lo reconoce |
|---|---|
| Procesos `cloud-hypervisor` | argv `--api-socket {--ch-socket-dir}/ch-{id}.sock`. SIGTERM, 5 s, SIGKILL |
| Procesos `virtiofsd` | argv `--socket-path {--ch-socket-dir}/virtiofs-{id}.sock` |
| Túneles local-net | claves `{id}.key` en `ASP_LOCAL_NET_KEY_DIR` y devices WireGuard `wg-asp-*`. Borra el device, las `ip rule`, la tabla, las reglas FORWARD y las excepciones nft, como al parar la sandbox |
| TAPs | devices TUN/TAP `asp-{8 hex}` |
| Sockets y enlaces | `ch-`, `vsock-` (y sus `_26501`/`_26502`), `virtiofs-` y `ssh-agent-{id}.sock` en `--ch-socket-dir` |
| Locks y pid files | `ch-{id}.sock.lock` (de Cloud Hypervisor) y `virtiofs-{id}.sock.pid` (de virtiofsd) en `--ch-socket-dir`: ninguno de los dos los borra al salir. Se quedan si su socket es `--ch-api-socket` o si su proceso sigue vivo tras SIGKILL. Al parar una sandbox el agente también los borra, una vez ha salido el proceso |

- Solo toca nombres con un id de sandbox del plano de control (UUID en minúsculas) y con el tipo de fichero que crea el agente. Nunca toca el rootfs base, `--ch-api-socket`, el bridge SSH ni el socket de identidad.
- Un fallo no para el resto: sale como `host cleanup incomplete` en el log y el agente arranca igual. Cada resto borrado deja una línea `removing leftover of a previous node-agent`.
- `--reap-leftovers=report` solo lo lista; `off` lo desactiva. Con `--dry-run` solo informa, y no mira TAPs ni túneles del host.
- `--reap-only` hace solo la limpieza y sale (código 0 si todo se borró). Para ver qué queda en un nodo con el servicio parado: `sudo node-agent --reap-only --reap-leftovers=report`, con los mismos `--ch-socket-dir` y `--disk-dir` que el servicio si no son los de por defecto.
- **Un agente por host.** Mientras vive, el agente mantiene un `flock` sobre `{--ch-socket-dir}/node-agent.lock`. Un segundo agente en ese directorio no arranca (`refusing to start … is held by pid N`), y `--reap-only` tampoco corre. Los TAPs y túneles son de todo el host: dos agentes reales en una misma máquina (solo lab) necesitan directorios de sockets distintos y `--reap-leftovers=off`.
- Modo shared (`--ch-api-socket`): ese CH no es del agente y no se toca; su VM sigue ahí tras el reinicio (`vm.delete` a mano).
- **Actualizar el agente detiene sus VMs.** Haz `asp node cordon` y drena antes de `systemctl restart` ([`ops-multi-node.md`](ops-multi-node.md)).

## 6. Imagen guest y dataplane exec

### 6.1 `images/guest`

El [`Dockerfile`](../images/guest/Dockerfile) construye Debian bookworm-slim + `pod-daemon` (usuario `sandboxd`), unidad systemd, CMD vsock **26500**:

```bash
docker build -t agent-sandbox-guest -f images/guest/Dockerfile .
./scripts/build-guest-rootfs.sh /var/lib/asp/images/rootfs.img
sudo ln -sfn /var/lib/asp/images/rootfs.img /opt/sandbox/rootfs.img
```

Ver [`images/guest/README.md`](../images/guest/README.md). Firmar rootfs = ops/corp.

### 6.2 pod-daemon y vsock

| Modo | Estado |
|---|---|
| `--listen unix --unix-socket …` | **Implementado** (dry-run / smoke) |
| `--listen vsock --vsock-port 26500` | **Implementado** — AF_VSOCK `CID_ANY:26500` en guest |
| `--listen tcp --tcp-addr 0.0.0.0:26500` | **Implementado** — alternativa lab vía TAP (no vsock) |
| CID en CreateVM | Reconciler asigna CID **único ≥ 3**; `VsockPath: /run/asp/vsock-{sandboxID}.sock` |

### 6.3 Path exec productivo (hybrid vsock)

Cloud Hypervisor expone un **multiplexor UDS** en el host (`vsock.cid` + `vsock.socket` en `vm.create`). El protocolo host→guest (Firecracker-compatible):

```text
host: connect(/run/asp/vsock-{id}.sock)
host: write "CONNECT 26500\n"
CH  : reenvía a guest AF_VSOCK port 26500
guest pod-daemon: accept → HTTP
CH  : ACK "OK <host_port>\n" al host
host: HTTP POST /v1/exec sobre el mismo stream
```

Flujo completo:

```text
Cliente
  → POST /v1/sandboxes/{id}/exec          (control-plane)
  → POST {agent_endpoint}/v1/internal/exec (node-agent localhost)
  → Registry[sandbox_id] → HybridVsockDialer (CONNECT 26500)
  → guest pod-daemon POST /v1/exec
```

| Entorno | Dialer | Guest listen |
|---|---|---|
| Bare-metal CH | `HybridVsockDialer` (UDS + CONNECT) | `--listen vsock --vsock-port 26500` |
| Dry-run / smoke | `UnixDialer` (`--pod-daemon-sock`) | `--listen unix` en el host |
| Opcional AF_VSOCK host | `AFVsockDialer` (mdlayher/vsock) | mismo guest vsock; útil si el host expone `/dev/vsock` |

Flags node-agent: `--pod-daemon-port=26500` (default); `--pod-daemon-sock` solo dry-run/fallback.

### 6.4 Identity / SSH guest→host (`--host-vsock`)

```text
guest AF_VSOCK connect(cid=2, port=26501) → node-agent SSH agent pump
guest AF_VSOCK connect(cid=2, port=26502) → identity HTTP POST /v1/tokens/oidc
```

En CH el VMM conecta esas llamadas a `{vsock}_26501` / `{vsock}_26502` de la sandbox; el token es siempre el de esa sandbox y un `X-ASP-Sandbox-ID` de otra da 403 ([ADR-0003](adr/0003-identity.md) § 2).

También: unix `--ssh-agent-bridge` / `--identity-listen` (este sin binding de sandbox: solo lab, con `--insecure-identity-sandbox-header`); reconciler crea `/run/asp/ssh-agent-{id}.sock` → bridge.

**Fase 2e — SSH auto en guest:** habilita `ssh-agent-vsock.service` en la imagen (vsock CID2:26501 → `/run/agent-sandbox/ssh-agent.sock`). Flag host `--guest-ssh-agent-auto` (default con `--host-vsock`). Virtiofs = alternativa ops manual. Ver [`guest-vsock-notes.md`](../scripts/guest-vsock-notes.md), [`why-2e-ssh-guest-mount.md`](why-2e-ssh-guest-mount.md).

Kernel guest: `CONFIG_VIRTIO_VSOCKETS`. Host hybrid exec: CH muxer UDS (sin `/dev/vsock`). Host `--host-vsock`: necesita `/dev/vsock` o `--host-vsock-dir`.

---

## 7. Checklist de verificación

Ejecutar en el host KVM (no en un entorno sin `/dev/kvm`).

1. **KVM**
   - [ ] `kvm-ok` OK
   - [ ] `ls -l /dev/kvm` accesible por el usuario ASP
2. **CH**
   - [ ] `cloud-hypervisor --version` (en `PATH` o `--ch-binary`)
   - [ ] `/run/asp` writable por el usuario del node-agent (modo per-sandbox)
   - [ ] Tras Start: socket `/run/asp/ch-{id}.sock` + `vmm.ping` vía curl unix-socket OK
   - [ ] (Solo shared) proceso con `--api-socket` + ping al arrancar
3. **Assets**
   - [ ] `/opt/sandbox/vmlinux` y `/opt/sandbox/rootfs.img` resolubles
   - [ ] TAP `asp-<short>` vía `--tap-auto` o creado a mano antes del claim
4. **Claves persistentes**
   - [ ] `ASP_CA_CERT`, `ASP_CA_KEY`, `ASP_OIDC_KEY`, `ASP_ATTEST_KEY` (control plane) y `--cert-dir` (node-agent) fuera de `/tmp`, `/var/tmp`, `/dev/shm` y `$TMPDIR`. En modo producción el arranque falla si no (`ASP_ALLOW_TMP_KEYS=1` lo fuerza); en lab solo avisa.
5. **Control-plane**
   - [ ] Postgres up; `ASP_AUTO_PROVISION=0`
   - [ ] `GET /healthz` OK; TLS/mTLS según corp
6. **Node-agent**
   - [ ] Enroll + register; certs en `ASP_CERT_DIR`
   - [ ] `--reconcile` sin `--dry-run`; opcional `--tap-auto --host-vsock`
   - [ ] ping CH sin warning persistente (solo shared)
7. **Ciclo sandbox**
   - [ ] `POST /v1/sandboxes` → `requested`, colocada en un nodo con hueco (503 si ninguno cabe)
   - [ ] Reconciler claim → `starting` → VMM Start → `running`
   - [ ] `GET /v1/sandboxes/{id}/events` muestra transiciones
   - [ ] `POST .../exec` vía hybrid vsock (guest `--listen vsock`) o unix en dry-run
   - [ ] (opcional) guest dial CID 2:26502 identity / 26501 SSH
   - [ ] `POST /v1/sandboxes/{id}/stop` → `stopping` → el guest se apaga → `stopped` (disco en `--disk-dir`); `POST …/start` → `running` con `boot_count` 2 y lo escrito antes de parar; `DELETE` → `deleting` → `deleted` (disco borrado)
8. **Egress (política)**
   - [ ] `PUT /v1/tenants/{id}/egress` + check allow/deny
   - [ ] Node `--egress-enforce`
9. **Varios nodos (si aplica)** — [`ops-multi-node.md`](ops-multi-node.md)
   - [ ] `asp node list`: cada nodo `SCHEDULABLE yes`, con la capacidad esperada
   - [ ] CP en otro host: `--agent-tls-listen` en el nodo y exec por mTLS (sin `9100` abierto)
   - [ ] `asp node cordon` → las sandboxes nuevas van a otro nodo; `uncordon` lo devuelve
   - [ ] Unit `scripts/systemd/asp-node-agent.service` instalada (`KillMode=control-group`)

Script de referencia dry-run (no CH): `./scripts/smoke-reconcile.sh`.

---

## 8. Hardening corporativo

| Control | Acción |
|---|---|
| mTLS nodos | `ASP_TLS_*` + `ASP_CLIENT_CA` + node `--mtls`; bootstrap token solo en enroll bootstrap |
| API keys | siempre exigidas (sin ellas ni IdP el CP no arranca); `ASP_BOOTSTRAP_API_KEY` en secret manager, no en git, solo para crear las demás con `asp apikey create` y rotarla o revocarla después (`asp apikey rotate|revoke`). `ASP_INSECURE_OPEN_API=1` solo en labs |
| Auto-provision | **`ASP_AUTO_PROVISION=0`** (nunca stub sync en prod) |
| Egress | `ASP_EGRESS_DENY_DEFAULT=1`; allowlist por tenant; `--egress-enforce`; NAT deny-forward default (§3) |
| Secretos | CA/keys en `/var/lib/asp/certs` mode `0600`; rotación = fase 2 |
| Superficie CH | Dir de sockets `/run/asp` root:kvm `0750`; un socket por sandbox |
| Guest | Imagen mínima, sin claves, sin `NET_ADMIN`; pod-daemon usuario no root |
| Observabilidad | Journal `sandbox_events` / `node_events`; no loguear bodies ni tokens |
| Nested | Solo lab; prod en bare metal o KVM dedicado |

---



## 8b. Autodefensa multi-nodo (soft fencing) y rotación OIDC

### Conjunto asignado

- Cada `GET /v1/nodes/{id}/work` trae `assigned`: las sandboxes colocadas en ese nodo que lo siguen ocupando. El reconciler para cualquier VM local que no esté ahí (fallada por nodo perdido, destruida o de otro nodo), sin reportar estado. Un 409 al reportar `running` también la para.
- Sustituye a los leases por sandbox (`node_lease_until`, migración `004`, y `POST …/renew-lease`), retirados en 2026-10: `renew-lease` responde 410 y la columna ya no se escribe. En estado estable el nodo solo hace el sondeo de `/work` y el heartbeat.
- Ningún nodo reclama sandboxes de otro: un nodo caído lo detecta el monitor del plano de control ([ADR-0011](adr/0011-multi-node.md), [`ops-multi-node.md`](ops-multi-node.md)): `offline` a los `ASP_NODE_STALE_AFTER` (90 s); fencing y sandboxes → `failed` (`node_lost`) a los `ASP_NODE_FAILOVER_AFTER` (5 min).
- El destino de fencing de un nodo (`fence_endpoint`, `fence_token`) lo fija un admin en el plano de control (`asp node fence set`, §8c), nunca el nodo: un agente comprometido podría apuntarlo al BMC de otro servidor.

**Límite split-brain (honesto):** la autodefensa del nodo **no** es STONITH. Un nodo particionado sigue corriendo sus VMs hasta que vuelve y ve que ya no están en su conjunto `assigned`, o hasta el fencing. Mitigación: `ASP_FENCE_PROVIDER` (ver §8c).

### Rotación de clave OIDC

1. Genera nueva RSA PEM; configura `ASP_OIDC_KEY=/path/new.pem` y `ASP_OIDC_KEY_PREV=/path/old.pem`.
2. Reinicia control-plane: JWKS publica **ambas** (`kid` actual + previo); mint usa solo la actual.
3. Tras TTL de tokens antiguos (default 5m) + margen de caché JWKS de clientes, quita `ASP_OIDC_KEY_PREV` y reinicia.


## 8c. Remote attestation, fencing y proxy hardening (Fase 2c)

### Remote attestation (MVP software)

1. El control plane solo acepta claves que conoce; la que trae el bundle (`public_key_pem`) no vale. Con mTLS (`ASP_CLIENT_CA` y `https://`) el node-agent firma con la clave de su certificado de nodo y no hace falta nada más. Sin mTLS, comparte `ASP_ATTEST_KEY` (PEM ECDSA P-256) entre node-agent y control-plane, o da de alta la clave pública de cada nodo en `ASP_ATTEST_TRUSTED_PUBS`.
2. Tras `running`, el reconciler firma `BootStatement` y hace `POST /v1/sandboxes/{id}/attest`. El statement lleva el SHA-256 del kernel (`kernel_digest`), el de la imagen base de la que se copió el disco (`image_digest`), la versión del hipervisor y si es un arranque nuevo o un `resume`; los calcula el nodo ([ADR-0003](adr/0003-identity.md)).
   - **Lista de imágenes permitidas:** `node-agent --print-measurement` (en el nodo, con el kernel y la imagen que usa) imprime una entrada `{name, kernel, rootfs, vmm}`; ponla en un fichero `{"images":[…]}` y apunta `ASP_ATTEST_ALLOWED_IMAGES` a él en el control plane. El fichero se relee al cambiar. Con lista, una evidencia de una imagen que no esté se rechaza (400, evento `sandbox.attestation_refused`) y no hay claim; sin lista se guardan los digests que declare el nodo, sin comprobarlos.
   - **Al reconstruir la imagen** cambia el digest: añade la entrada nueva antes de actualizar los nodos, y quita la vieja cuando no queden sandboxes de ella.
   - **Orden de actualización:** el control plane primero; un nodo nuevo firma campos que uno anterior no conoce y su firma no cuadra.
3. Consulta: `GET /v1/sandboxes/{id}/attestation`; verificación sin store: `POST /v1/attestation/verify`.
4. Mint OIDC incluye `x_asp_attestation` si la evidencia está dentro de `ASP_ATTEST_MAX_AGE` (default 10m): `measured`, `allowlisted`, `image_name`, `image_digest`, `kernel_digest`, `vmm_version` y `boot`, además del nodo y la clave que firmó. Con lista, sin entrada vigente no hay claim.
5. Hardware TPM/SEV: implementar la interfaz `Attestor` (plug-in futuro); el MVP es `SoftwareAttestor`.

### STONITH / FenceProvider

| `ASP_FENCE_PROVIDER` | Comportamiento |
|---|---|
| `noop` / vacío | Sin fence (default) |
| `http_webhook` | `POST` JSON `{action:power_off,node_id}` a `nodes.fence_endpoint`; Bearer `fence_token` |
| `redfish` | Stub HTTP basic → `{endpoint}/redfish/v1/Systems/1/Actions/ComputerSystem.Reset` |
| `ipmi` | Exec `ipmitool … chassis power off` si existe; **SoftFail** si no |

El destino de cada nodo lo configura un admin (IdP admin o API key de plataforma), no el nodo:

```bash
asp node fence set ncc1701d --endpoint https://bmc.example/redfish --token-env BMC_PW   # lee BMC_PW del entorno del CP al fencear
asp node fence set ncc1701d --endpoint 10.0.0.9 --token-file /etc/asp/bmc.pw             # o de un fichero del host del CP
echo -n "$PW" | asp node fence set ncc1701d --endpoint … --token-stdin                     # o se guarda en la base de datos
asp node fence clear ncc1701d
```

(API: `PUT`/`DELETE /v1/nodes/{id}/fence`.) El endpoint y el token nunca salen en ninguna respuesta; `GET /v1/nodes` solo dice `fence_configured`. Con `--token-env`/`--token-file` el secreto no llega a Postgres; un `ipmitool` recibe la contraseña por `IPMI_PASSWORD`, no por la línea de comandos. Los campos `fence_endpoint`/`fence_token` que mande un agente al registrarse se ignoran, y `ASP_FENCE_ENDPOINT`/`ASP_FENCE_TOKEN` del node-agent ya no hacen nada (avisa en el log). Cuando el monitor da un nodo por perdido y tiene sandboxes, el CP llama al provider (una vez por caída) antes de marcarlas `failed`. Si el fencing falla, se registra `node.fence_failed` y se marcan igual.

> **Ops:** STONITH real exige BMC out-of-band (Redfish/IPMI alcanzable aunque el host esté hung). Una fila en Postgres **no** apaga VMs huérfanas.

### Proxy hardening

- Rate limit: token bucket (`DefaultRate`/`DefaultBurst`) keyed por `X-ASP-Sandbox-ID` + host.
- Body limit: `ASP_EGRESS_MAX_BODY` (default 8MiB).
- Schemes: solo `http`/`https`.
- Audit: línea JSON `egress_audit`.
- MITM: `--egress-mitm --egress-mitm-ca=/path/ca.pem` (default **off**).

## 8d. Fase 2d — certs, mTLS estricto, SSH confirm, nft redirect

### Rotación / revocación de certs de nodo

**Por qué:** un cert robado no debe seguir hablando al CP. **Qué ganamos:** rotate + revoke + middleware.

```bash
# Emitir cert nuevo (API key de plataforma o admin del IdP; el bootstrap token no sirve, solo enrola.
# Un nodo con mTLS se renueva solo con su certificado vigente)
curl -fsS -X POST -H "Authorization: Bearer $ASP_BOOTSTRAP_API_KEY" \
  https://cp:8080/v1/nodes/$NODE_ID/rotate-cert | jq .
# Instalar PEMs en --cert-dir del node-agent y reiniciar agente

# Revocar nodo (bloquea fingerprint actual): admin del IdP o API key de plataforma,
# no el bootstrap token, que tienen todos los nodos
curl -fsS -X POST -H "Authorization: Bearer $ASP_BOOTSTRAP_API_KEY" \
  https://cp:8080/v1/nodes/$NODE_ID/revoke
```

Migración `006` añade `cert_serial`, `revoked_at`, tabla `node_cert_revocations`.

### mTLS estricto

```bash
export ASP_MTLS_STRICT=1
export ASP_ENROLL_LISTEN=127.0.0.1:8081   # plaintext solo enroll
# Listener TLS principal: RequireAndVerifyClientCert
# Enroll / re-enroll: http://127.0.0.1:8081/v1/nodes/enroll (bootstrap token)
# Rotate con cert vigente + API key sigue en el listener TLS.
```

### SSH agent confirmation

Multi-user (ADR-0007 fase 4): `ASP_SSH_AGENT_SOCK_TEMPLATE=/run/asp/ssh-agents/{owner_sub}.sock` + `ASP_MULTI_USER=1` (confirm default-on). Ops debe crear el UDS con las keys del usuario; path ausente → FakeAgent. Sin template = bridge global legacy (no multi-user-safe).


```bash
node-agent ... --ssh-agent-confirm --host-vsock --reconcile
# Antes de que el guest de sb-1 firme (por su {vsock}_26501):
curl -fsS -X POST http://127.0.0.1:9100/v1/internal/ssh-agent/approve \
  -H "Authorization: Bearer $(sudo cat /var/lib/asp/agent.token)" \
  -d '{"ttl_seconds":60,"sandbox_id":"sb-1"}'
# Sin approve → SignRequest = SSH_AGENT_FAILURE
```

La aprobación solo vale para la sandbox que nombra. `--ssh-agent-bridge` y el listener host-vsock global no saben qué guest llama: con `--ssh-agent-confirm` deniegan toda firma, salvo `--insecure-ssh-agent-global-approvals` (lab). Por cualquier ruta, el guest solo puede listar claves y firmar: añadir, borrar o bloquear claves del agente del host devuelve `SSH_AGENT_FAILURE`.

### nftables anti-bypass (sketch 2d → completo en §8e)

```bash
./scripts/nftables-egress-redirect.sh dry-run \
  --guest-subnet 10.200.0.0/16 --proxy-port 8888 --dns-sink-port 5353
```

Detalle histórico 2d: [`adr/0005-fase-2d-hardening.md`](adr/0005-fase-2d-hardening.md). Completo: §8e.

## 8e. Fase 2e — nft redirect completo + SSH guest auto

### nft Enforce (bare-metal)

**Por qué / Qué ganamos:** ver [`why-2e-nft-redirect.md`](why-2e-nft-redirect.md).

```bash
# Dry-run (sin root) — debe listar tabla asp_egress, 80/443 y DNS 53
./scripts/nftables-egress-redirect.sh dry-run \
  --guest-subnet 10.200.0.0/16 --proxy-port 8888 --dns-sink-port 5353

# Apply Enforce (root + nft + TAP)
sudo ./scripts/nftables-egress-redirect.sh apply --mode enforce \
  --guest-subnet 10.200.0.0/16 --proxy-port 8888 \
  --dns-sink-port 5353 --dns-action redirect

# Desde node-agent:
node-agent ... \
  --egress-proxy-listen=:8888 --egress-dns-sink=:5353 \
  --nft-egress-redirect --nft-egress-mode=enforce \
  --nft-http-ports=80,443 --guest-subnet=10.200.0.0/16

# SoftFail (CI / sin CAP_NET_ADMIN):
node-agent ... --nft-egress-redirect --nft-egress-mode=soft
```

**Enforce requiere:** root, binario `nft`, iface TAP con el subnet guest, proxy y (si redirect) DNS sink escuchando. Sin eso, usa `soft`.

### SSH agent auto en guest

**Por qué / Qué ganamos:** ver [`why-2e-ssh-guest-mount.md`](why-2e-ssh-guest-mount.md).

```bash
# Host
node-agent ... --host-vsock --ssh-agent-bridge=/run/asp/ssh-agent.sock --guest-ssh-agent-auto
# (guest-ssh-agent-auto ya default on con --host-vsock)

# Guest (imagen con ssh-agent-vsock.service):
#   SSH_AUTH_SOCK=/run/agent-sandbox/ssh-agent.sock
#   dial → vsock://2:26501
```

ADR: [`adr/0006-fase-2e-nft-ssh-guest.md`](adr/0006-fase-2e-nft-ssh-guest.md).

## 9. Troubleshooting

| Síntoma | Causa probable | Qué mirar |
|---|---|---|
| `kvm-ok` FAIL / no `/dev/kvm` | VT-x/AMD-V off o nested no habilitado | BIOS; `lsmod kvm`; permisos grupo `kvm` |
| `CH ping failed` | Solo modo shared: CH no corre o path distinto | `ps aux \| grep cloud-hypervisor`; `CH_API_SOCKET` vs `--api-socket` |
| `wait for CH API` / spawn fail | Binario ausente, `/run/asp` no writable, KVM | `--ch-binary`; `ls -ld /run/asp`; `/dev/kvm` |
| `vm.create` error TAP | TAP inexistente o sin permiso | `--tap-auto` o script §3.2; `netdev` / CAP_NET_ADMIN |
| `vm.create` kernel/rootfs | Rutas `/opt/sandbox/*` rotas | Symlinks §2.2; cmdline `root=/dev/vda` |
| Segundo sandbox `failed` (shared) | `--ch-api-socket` = un VM | Quita el flag; usa `--ch-socket-dir` (default) |
| Create → `running` sin VMM | `ASP_AUTO_PROVISION=1` | Pon `=0` y usa `--reconcile` |
| Enroll 401 | Token / TLS | `ASP_NODE_BOOTSTRAP_TOKEN`; enroll **no** lleva client cert |
| Register/heartbeat 401 mTLS | Sin client cert | `--enroll` previo; `--mtls` + `cert-dir`; `ASP_CLIENT_CA` en CP |
| Exec 502 / connection refused | `agent_endpoint` o pod-daemon | Nodo registered con `http://127.0.0.1:9100`; `--pod-daemon-sock`; §6.3 |
| Exec OK en host pero no en guest | guest sin `--listen vsock` o puerto ≠ 26500 | Arranca pod-daemon vsock en guest; verifica CONNECT ACK; §6.3 |
| Egress “allow” pero tráfico sale | Guest no usa HTTP_PROXY / DNS bypass | Apunta proxy §3.4; nft bloquear UDP53/WAN directo |
| Permission denied cert-dir | `/var/lib/asp/node-certs` no writable | `mkdir` + owner; o `ASP_CERT_DIR` writable |
| CID vsock conflict | (resuelto) allocator ≥3 | Si ves colisión, bug en `allocCID`; revisa handles |
| `refusing to start … node-agent.lock is held by pid N` | Ya corre un node-agent (o un `--reap-only`) con ese `--ch-socket-dir` | `systemctl status asp-node-agent`; `ps -p N`; no arranques otro agente a mano junto al servicio |
| `host cleanup incomplete` al arrancar | La limpieza de §5.6 no pudo parar o borrar algo (permisos, proceso en estado D) | El error nombra el recurso; `node-agent --reap-only --reap-leftovers=report` lista lo que queda |
| `cloud-hypervisor` o TAP `asp-*` huérfanos tras reiniciar el agente | Agente fuera de systemd, unit con `KillMode=process`, o `--reap-leftovers=off` | Usa la unit de §5.6; el siguiente arranque los borra |

---


---

## 10. Procedimiento end-to-end (checklist ops)

1. Instalar CH pinneado + assets (`vmlinux`, `rootfs.img` vía `build-guest-rootfs.sh`) → symlinks `/opt/sandbox/*`.
2. Postgres + control-plane con `ASP_AUTO_PROVISION=0`, TLS/mTLS, bootstrap tokens. Claves fuera de `/tmp`: `ASP_CA_CERT`, `ASP_CA_KEY`, `ASP_OIDC_KEY` y `ASP_ATTEST_KEY` en almacenamiento persistente (p. ej. `/var/lib/asp`; las que falten se crean ahí). Con `DATABASE_URL`, `ASP_TLS_CERT`, `ASP_CLIENT_CA` o `ASP_IDP_REQUIRED=1` el control plane no arranca (código 2) si alguna está en un directorio temporal, salvo `ASP_ALLOW_TMP_KEYS=1`.
3. Node-agent: `--enroll --mtls --reconcile --tap-auto --host-vsock --ssh-agent-bridge=… --egress-enforce` (sin `--dry-run`), con `--cert-dir` (y `ASP_ATTEST_KEY` o `--egress-mitm-ca` si los usas) fuera de `/tmp`: un nodo de producción no arranca con ellos en un directorio temporal, salvo `ASP_ALLOW_TMP_KEYS=1`. Como servicio: [`scripts/systemd/asp-node-agent.service`](../scripts/systemd/asp-node-agent.service) (§5.6).
4. `POST /v1/sandboxes` → reconciler claim → TAP `asp-*` → CH spawn → `running`.
5. `POST /v1/sandboxes/{id}/exec` → hybrid CONNECT 26500 → guest pod-daemon.
6. Desde guest: dial CID 2 ports 26501/26502 (o socat); mint OIDC / SSH agent.
7. `POST /v1/sandboxes/{id}/stop` → apagado del guest → delete TAP + sockets → `stopped`, con el disco conservado; `DELETE` → además borra el disco → `deleted`.
8. Pack release: `make pack` → `/workspace/agent-sandbox-platform-release.tar.gz`.

Smokes dry-run (sin KVM): `make smoke`.

## Workspace del host (virtiofs)

`asp session start --workspace /ruta` persiste `workspace_host_path`. Si no está vacío, el node-agent arranca `virtiofsd` (binario Rust, `--virtiofsd-bin` / `VIRTIOFSD_BIN`) con un socket por sandbox y `vm.create` incluye `fs` tag `workspace`. Sin el binario el sandbox pasa a `failed`. Sin workspace no hay `fs`.

La imagen guest de este corte monta sola: `workspace-virtiofs.service` hace `mkdir -p /workspace` y `mount -t virtiofs workspace /workspace`, y sale 0 si el tag no está (el boot no se para). Hay que **reconstruir** el rootfs (`./scripts/build-guest-rootfs.sh`) para que una imagen ya desplegada lo lleve. Hasta entonces, dentro de la VM:

```sh
mkdir -p /workspace
mount -t virtiofs workspace /workspace
```

Sin ese mount (imagen vieja) el exec ve el disco del guest. FakeVMM no bootea; los tests afirman socket, tag, y que el helper sale 0 si `mount` falla. Detalle: [`ops-asp-session.md`](ops-asp-session.md), [`why-virtiofs-pty.md`](why-virtiofs-pty.md).

El exec con PTY también viaja por el vsock **26500** (`POST /v1/exec?stream=1` y `POST /v1/exec/stdin`). La imagen tiene que llevar el pod-daemon de este corte; si no, no hay `ready` y el node-agent degrada a un JSON final reescrito como un solo burst. El stream no tiene timeout total en el guest: `--exec-timeout-secs` (default 30) solo limita el exec acumulado, y `--stream-idle-timeout-secs` añade, si se pone, un límite por inactividad.

## Referencias rápidas

- Cliente CH: `node-agent/internal/vmm/cloudhypervisor.go`
- Flags: `node-agent/cmd/node-agent/main.go`, [`node-agent/README.md`](../node-agent/README.md)
- Reconciler + paths: `node-agent/internal/reconciler/reconciler.go`
- CP env: [`control-plane/README.md`](../control-plane/README.md)
- Smoke dry-run: [`mvp-smoke.md`](mvp-smoke.md)
- Red ADR: [`adr/0002-networking.md`](adr/0002-networking.md)
