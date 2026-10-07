# Node agent

Agente privilegiado en cada nodo de sandboxes. Habla con Cloud Hypervisor vía HTTP sobre Unix socket, se enrolla/registra en el control plane (mTLS opcional), expone proxy localhost de exec/egress-check hacia `pod-daemon`, identity proxy OIDC, bridge de SSH agent, **host-vsock** guest→host y **TAP auto**.

## Flags

| Flag | Env | Default |
|---|---|---|
| `--control-plane-url` | `CONTROL_PLANE_URL` | `http://127.0.0.1:8080` |
| `--node-id` | `NODE_ID` | CN del cert enrolado en `--cert-dir`; si no hay, hostname |
| `--ch-socket-dir` | `CH_SOCKET_DIR` | `/run/asp` — sockets `ch-{id}.sock` (default per-sandbox spawn) |
| `--ch-api-socket` | `CH_API_SOCKET` | vacío — shared/legacy override (sin spawn) |
| `--ch-binary` | `CLOUD_HYPERVISOR_BIN` | `cloud-hypervisor` |
| `--dry-run` | `DRY_RUN=1` | false — usa `FakeVMM` |
| `--reap-leftovers` | `ASP_REAP_LEFTOVERS` | `on` — al arrancar, antes de registrarse, para y borra lo que dejó un node-agent anterior: `cloud-hypervisor`/`virtiofsd` de `--ch-socket-dir`, sus sockets, TAPs `asp-*`, túneles `wg-asp-*`. No toca las copias de `--disk-dir`: las borra el GC del reconciler (ver `--disk-dir`). `report` solo lo lista; `off`. Con `--dry-run` solo informa ([bare-metal §5.6](../docs/bare-metal-ch.md#56-servicio-systemd-y-reinicios-del-agente)) |
| `--reap-only` | | hace solo esa limpieza y sale; se niega si corre un agente con ese `--ch-socket-dir` (lock `node-agent.lock`) |
| `--print-measurement` | | imprime el SHA-256 del kernel y de la imagen base y la versión del hipervisor como entrada de `ASP_ATTEST_ALLOWED_IMAGES` del control plane, y sale |
| `--enroll` | `ASP_ENROLL=1` | enrollment al arrancar. Si el plano de control responde que el nodo ya está enrolado (409), sigue con el certificado de `--cert-dir` cuando es de este nodo y no ha caducado |
| `--enroll-token` | `ASP_NODE_ENROLL_TOKEN` | token de enroll de un solo uso (`asp node enroll-token`); uno fijado a este nodo le cambia la clave aunque esté enrolado |
| `--bootstrap-token` | `ASP_NODE_BOOTSTRAP_TOKEN` | token compartido de labs: enrola un id sin certificado o un nodo revocado, nunca re-enrola uno vivo |
| `--cert-dir` | `ASP_CERT_DIR` | dir de client certs (`/var/lib/asp/node-certs`; si no se puede escribir, uno temporal) |
| | `ASP_ALLOW_TMP_KEYS` | `1`: un nodo de producción arranca aunque `--cert-dir`, `ASP_ATTEST_KEY` o `--egress-mitm-ca` estén en un directorio temporal |
| `--mtls` | `ASP_MTLS=1` | exigir client certs |
| `--control-plane-ca` | `ASP_CONTROL_PLANE_CA` | CA del cert TLS del CP (enroll y llamadas); por defecto `cert-dir/ca.crt`, la misma CA que firma los certificados de nodo: avisa si el CP es remoto. Pasa una CA que firme solo el cert del CP ([`ops-multi-node.md`](../docs/ops-multi-node.md#las-dos-raíces-de-confianza)) |
| `--enroll-url` | `ASP_ENROLL_URL` | URL de enroll si no es `--control-plane-url` (`ASP_MTLS_STRICT`) |
| `--agent-listen` | `ASP_AGENT_LISTEN` | `127.0.0.1:9100` — API local (exec, egress-check, approve). Pide `Authorization: Bearer <token>` salvo `GET /healthz`; fuera de loopback no arranca |
| `--api-key-file` | `ASP_NODE_API_KEY_FILE` | Fichero con la API key (ámbito `platform`) que el nodo manda al CP en cada llamada salvo enroll; también `ASP_NODE_API_KEY`. Hace falta con un CP por HTTP plano, donde no hay certificado de cliente: el CP rechaza toda llamada de nodo sin credencial. Con mTLS (`https://` + `ASP_CLIENT_CA`) el certificado basta |
| `--agent-token-file` | `ASP_AGENT_TOKEN_FILE` | Fichero con el secreto de esa API. Se crea (0600) si no existe, con 32 bytes aleatorios. El plano de control del mismo host lee el mismo fichero (`ASP_AGENT_TOKEN_FILE`) y debe poder leerlo: si no corre como root, crea el fichero antes (`install -m 0640 -o root -g <grupo> …`). Defecto `/var/lib/asp/agent.token`, o `$TMPDIR/asp-agent.token` si ese directorio no es escribible (labs sin root; avisa en el log) |
| `--insecure-agent-listen` | `ASP_INSECURE_AGENT_LISTEN=1` | permite `--agent-listen` fuera de loopback (solo lab) |
| `--workspace-root` | `ASP_WORKSPACE_ROOTS` | Directorios (separados por comas) bajo los que puede vivir el workspace de una sandbox: debe estar dentro de `<raíz>/<tenant>/`, con los enlaces simbólicos resueltos. El path viene del spec de la sandbox, así que sin esto cualquiera que pueda crear una exportaría los discos y las claves del nodo. Defecto `/srv/asp/workspaces`; si no existe, ninguna sandbox puede tener workspace |
| `--guest-kernel` / `--guest-rootfs` | `ASP_GUEST_KERNEL` / `ASP_GUEST_ROOTFS` | `/opt/sandbox/vmlinux` y `/opt/sandbox/rootfs.img`. El kernel (un `vmlinux` sin comprimir) que arranca toda VM y la imagen base de la que se copia el disco de cada sandbox (nunca se arranca ella misma). Una prueba con otra imagen (un guest nuevo, un kernel distinto) no necesita tocar `/opt/sandbox`; la lista de imágenes permitidas de la atestación (`--print-measurement`) mide estos mismos ficheros |
| `--virtiofsd-sandbox` | `ASP_VIRTIOFSD_SANDBOX` | `--sandbox` de virtiofsd: `chroot` (lo confina al workspace), `namespace` o `none`. Defecto `chroot` si el agente es root, `none` si no |
| `--vm-survive-restart` | `ASP_VM_SURVIVE_RESTART` | `true` por defecto: una VM confinada sigue corriendo cuando el agente para o se reinicia, y el proceso siguiente la adopta ([ADR-0014](../docs/adr/0014-vms-outlive-the-agent.md)); cada VM en marcha deja un registro en `{--ch-socket-dir}/state/`. `=false` ata el servicio de cada VM al del agente (systemd las para con él), como antes |
| `--vm-unprivileged` | `ASP_VM_UNPRIVILEGED` | `auto` (por defecto), `on` u `off`. Cada `cloud-hypervisor` corre como un usuario sin privilegios propio (`--vm-uid-base` + CID), sin capabilities y en una unit sin sockets IP ni escritura fuera de sus ficheros ([ADR-0015](../docs/adr/0015-unprivileged-vmm.md)). `auto` cuando el host puede (lo prueba abriendo como ese usuario lo que el VMM necesita), si no como root diciendo por qué; `on` no arranca si no puede. Necesita `--vm-confine`. `virtiofsd` sigue siendo root |
| `--vm-uid-base` | `ASP_VM_UID_BASE` | primer id de usuario de los VMM; el de la VM con CID `n` corre como base más `n` (`1879048192` por defecto) |
| `--vm-run-dir` | `ASP_VM_RUN_DIR` | un subdirectorio por VM (de su usuario, con sus sockets), 0711. Por defecto `--ch-socket-dir` + `-vm` (`/run/asp-vm`) |
| `--vm-confine` | `ASP_VM_CONFINE` | `auto` (por defecto), `on` o `off`. Cada microVM y su `virtiofsd` corren en un servicio systemd transitorio propio (`asp-vm-<id>`, `asp-vm-<id>-fs`) con su cgroup y sus límites; con `auto`, cuando el host puede (root y systemd) y, si no, como hijos del agente diciendo por qué; `on` no arranca si no puede; `off` es el comportamiento anterior |
| `--vm-slice` | `ASP_VM_SLICE` | slice de esos servicios (`asp-vms.slice`) |
| `--vm-memory-overhead-mib` | `ASP_VM_MEMORY_OVERHEAD_MIB` | memoria que se suma a la del guest en el `MemoryMax` del servicio (por defecto 256) |
| `--vm-cpu-overhead-percent` | `ASP_VM_CPU_OVERHEAD_PERCENT` | porcentaje de una CPU que se suma a las vCPU en el `CPUQuota` (por defecto 50) |
| `--vm-tasks-max` | `ASP_VM_TASKS_MAX` | procesos e hilos máximos de un servicio de VM (por defecto 1024) |
| `--capacity-cpu` | `ASP_CAPACITY_CPU` | `-1` = núcleos del host; `0` = no limita |
| `--capacity-mem-mib` | `ASP_CAPACITY_MEM_MIB` | `-1` = `MemTotal − max(1 GiB, 10 %)`; `0` = no limita |
| `--max-sandboxes` | `ASP_MAX_SANDBOXES` | `0` = sin tope |
| `--local-net-dial` | `ASP_LOCAL_NET_DIAL` | `host[:puerto]` que marca el portátil para local-net en este nodo |
| `--agent-tls-listen` | `ASP_AGENT_TLS_LISTEN` | vacío — `exec` con mTLS para un CP en otro host (p.ej. `0.0.0.0:9443`); solo acepta el cert del CP |
| `--endpoint` | `NODE_ENDPOINT` | anunciado al CP; por defecto `https://<hostname>:<puerto>` con `--agent-tls-listen`, si no `http://<agent-listen>` |
| `--pod-daemon-sock` | `ASP_POD_DAEMON_SOCK` | unix sock de pod-daemon (dry-run / fallback) |
| `--pod-daemon-port` | | `26500` — puerto guest vsock/TCP para HTTP |
| `--egress-enforce` | `ASP_EGRESS_ENFORCE=1` | 403 en egress-check denegado |
| `--egress-proxy-listen` | `ASP_EGRESS_PROXY_LISTEN` | forward proxy HTTP(S) (p.ej. `:8888`). `ASP_EGRESS_MAX_BODY` (8 MiB) limita el body de las peticiones HTTP (413); las respuestas pasan enteras, como por un túnel CONNECT. No reenvía cabeceras hop-by-hop (`Connection`, `Upgrade`, `Keep-Alive`, `Proxy-Authorization`…) |
| `--egress-allow-cidr` | `ASP_EGRESS_ALLOW_CIDRS` | Redes privadas (CIDR o dirección, separadas por comas) a las que el proxy puede conectar, además de Internet. El proxy comprueba la dirección tras resolver el nombre y nunca marca loopback, link-local, multicast, reservadas, las direcciones del propio nodo ni la red de los guests, diga lo que diga la allowlist; las redes privadas solo si están aquí |
| `--egress-dns-sink` | `ASP_EGRESS_DNS_SINK` | UDP DNS sink (p.ej. `:5353`): NXDOMAIN para nombres no permitidos |
| `--egress-mitm` | `ASP_EGRESS_MITM=1` | CONNECT TLS bump (default off; corp caution) |
| `--egress-mitm-ca` | `ASP_EGRESS_MITM_CA` | PEM CA MITM (generate/load) |
| `--ssh-agent-bridge` | `ASP_SSH_AGENT_BRIDGE` | unix sock bridge → `SSH_AUTH_SOCK` / FakeAgent. Solo reenvía listar claves y firmar |
| `--identity-listen` | `ASP_IDENTITY_LISTEN` | unix `.sock` o TCP para `POST /v1/tokens/oidc`; sin binding de sandbox: 403 salvo `--insecure-identity-sandbox-header` |
| `--default-sandbox-id` | `ASP_SANDBOX_ID` | sandbox de las peticiones sin `X-ASP-Sandbox-ID` en listeners sin binding; solo con `--insecure-identity-sandbox-header` |
| `--insecure-identity-sandbox-header` | `ASP_INSECURE_IDENTITY_SANDBOX_HEADER=1` | los listeners de identidad sin binding (`--identity-listen`, host-vsock global) toman la sandbox de `X-ASP-Sandbox-ID`: quien llegue a ellos pide el token de cualquier sandbox (solo lab) |
| `--reconcile` | `ASP_RECONCILE=1` | poll work / claim / Start-Stop VMM |
| `--reconcile-interval` | | default `2s` |
| `--reconcile-workers` | `ASP_RECONCILE_WORKERS` | `4` — sandboxes que el reconciler arranca o para a la vez. Un arranque lento (timeout de CH, copia de un rootfs grande) ya no retrasa al resto; una misma sandbox nunca la llevan dos workers, y un `stopping` que llega durante su arranque se atiende al terminar este. `1` con `--ch-api-socket`. Un pánico en un worker se registra con su traza y termina solo esa tarea: la sandbox queda `failed` (primer arranque), `stopped` con su disco (reanudación o parada) o `deleted`, con `status_detail=panic: …`; el agente y las demás VMs siguen. Lo mismo vale para un sondeo y para las conexiones del agente SSH del guest |
| `--guest-ready-timeout` | `ASP_GUEST_READY_TIMEOUT` | `120s`. Tiempo máximo que espera un arranque a que el pod-daemon de la VM responda a `/healthz` antes de informar `running`. CH está arriba antes de que el guest arranque (unos 3 s en un host KVM), y `asp sandbox run` hace el exec en cuanto ve `running`. Pasado ese tiempo el arranque **falla**: se para la VM, se registra en el log lo último que el guest escribió en su consola serie (el agente guarda los últimos 32 KiB en memoria, nunca en disco) y se informa `failed` con `status_detail=guest_not_ready: … (console: …)`, y se borra el disco recién copiado; una reanudación vuelve a `stopped` con su disco (`resume failed: guest_not_ready: …`). Antes informaba `running` igualmente, y solo el exec fallaba. `0`: informa en cuanto arranca la VMM. Sin efecto con `--dry-run` |
| `--tap-auto` | `ASP_TAP_AUTO=1` | crea/borra TAP `asp-{shortid}` con su propia /30 de `--guest-subnet`. Si el TAP no se puede crear (permisos, nombre ya en uso) la sandbox pasa a `failed`; solo `--dry-run` sigue sin TAP. La cmdline del guest lleva además su hostname (`asp-{shortid}`), su resolver (el gateway, si el DNS sink es alcanzable) y `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` (si hay `--egress-proxy-listen`): `docs/bare-metal-ch.md` §3.4 |
| `--disk-dir` | `ASP_DISK_DIR` | `/var/lib/asp/disks` — copia privada del rootfs por sandbox (`rootfs-{id}.img`); no aplica con `--dry-run`. **Parar la conserva** si el plano de control manda la lista `retained` en `/work` ([ADR-0012](../docs/adr/0012-retained-disks.md)); borrar la sandbox, un arranque fallido o un plano de control que no la manda (anterior) la borran. Tras un sondeo correcto, como mucho una vez por minuto, el nodo borra las copias que no son de ninguna sandbox: ni asignadas, ni retenidas, ni en borrado, ni en manos de este agente |
| `--stop-grace` | `ASP_STOP_GRACE` | `15s`. Al parar conservando el disco, el nodo pide al guest `sync; systemctl poweroff --no-block` por el pod-daemon y espera hasta este tiempo a que Cloud Hypervisor salga; si no sale, parada brusca con un aviso. El botón ACPI no sirve con la imagen actual (sin `logind`). `0`: parada brusca; no aplica con `--dry-run` |
| `--disk-min-free-mib` | `ASP_DISK_MIN_FREE_MIB` | `-1`: el doble del tamaño de la imagen base. Si `--disk-dir` tiene menos espacio libre, no clona ni reanuda una sandbox (`failed`, `disk: N MiB free…`). `0`: no comprueba |
| `--host-vsock` | `ASP_HOST_VSOCK=1` | AF_VSOCK 26501 SSH + 26502 identity (guest CID 2) |
| `--host-vsock-dir` | `ASP_HOST_VSOCK_DIR` | lab: unix bajo este dir en vez de AF_VSOCK |
| `--ssh-agent-confirm` | `ASP_SSH_AGENT_CONFIRM` | exige approve one-shot con el `sandbox_id` que va a firmar; default on si multi-user/template (`=0` fuerza off) |
| `--insecure-ssh-agent-global-approvals` | `ASP_INSECURE_SSH_AGENT_GLOBAL_APPROVALS=1` | con confirm, acepta approves sin `sandbox_id`; solo los usan `--ssh-agent-bridge` y el host-vsock global, para el primer guest que firme (solo lab) |
| `--ssh-agent-sock-template` | `ASP_SSH_AGENT_SOCK_TEMPLATE` | path template por sandbox (`{owner_sub}`/`{sandbox_id}`); missing → FakeAgent |
| `--multi-user` | `ASP_MULTI_USER=1` (o `ASP_IDP_REQUIRED=1`) | perfil multi-user: confirm default-on |
| `--metrics-listen` | `ASP_METRICS_LISTEN` | `host:puerto` donde servir `GET /metrics` (Prometheus), p. ej. `127.0.0.1:9102`. Sin autenticación: solo loopback salvo `--insecure-obs-listen`. Apagado por defecto. Catálogo en [`docs/how-to/monitoring.md`](../docs/how-to/monitoring.md) |
| `--pprof-listen` | `ASP_PPROF_LISTEN` | Igual para `/debug/pprof/` |
| `--insecure-obs-listen` | `ASP_INSECURE_OBS_LISTEN=1` | Permite las dos anteriores fuera de loopback (sin autenticación; pon delante un proxy que autentique) |
| `--egress-nft-redirect` / `--nft-egress-redirect` | `ASP_EGRESS_NFT_REDIRECT` / `ASP_NFT_EGRESS_REDIRECT` | nftables `asp_egress` HTTP+DNS redirect. **Default: activo** con `--egress-proxy-listen` fuera de `--dry-run`; `=false` lo apaga (el proxy es voluntario y el agente lo avisa). Con redirect y `--nft-dns-action=redirect` el DNS sink arranca en `:5353` si no lo nombras. El script va dentro del binario (`ASP_NFT_SCRIPT` nombra uno propio) |
| `--nft-egress-mode` | `ASP_NFT_EGRESS_MODE` | `enforce` (default: un nodo que no puede aplicar las reglas no arranca) \| `soft` (arranca sin forzar el egress, con aviso; default con `--dry-run`). El nodo informa `egress_enforced=true` al registrarse solo con el proxy escuchando y las reglas puestas en `enforce` |
| `--nft-http-ports` | `ASP_NFT_HTTP_PORTS` | default `80,443` |
| `--nft-dns-action` | `ASP_NFT_DNS_ACTION` | `redirect` (default) \| `drop` |
| `--guest-ssh-agent-auto` | `ASP_GUEST_SSH_AGENT_AUTO` | default on with `--host-vsock` / bridge |
| `--guest-subnet` | `ASP_GUEST_SUBNET` | `10.200.0.0/16` — pool de /30 por sandbox (TAP `.1`, guest `.2`) y match de las reglas nft |

```bash
go test ./...
go run ./cmd/node-agent \
  --control-plane-url=http://127.0.0.1:8080 --node-id=dev-node \
  --dry-run --reconcile --enroll --bootstrap-token=dev \
  --cert-dir=/tmp/asp-node-certs --agent-listen=127.0.0.1:9100 \
  --pod-daemon-sock=/tmp/pod-daemon.sock \
  --egress-enforce \
  --ssh-agent-bridge=/tmp/asp-ssh-agent.sock \
  --identity-listen=/tmp/asp-identity.sock --insecure-identity-sandbox-header \
  --host-vsock --host-vsock-dir=/tmp/asp-hv \
  --tap-auto
```

Política de egress ([ADR-0002](../docs/adr/0002-networking.md)): cada sondeo de `/work` trae la política efectiva del tenant de cada sandbox asignada y su versión. El reconciler la aplica antes del primer arranque (el guest no espera a un exec para tener red) y otra vez cuando cambia la versión, así que un `PUT /v1/tenants/{id}/egress` llega a las sandboxes en marcha en el siguiente sondeo. Sin política todavía, deny-default.

Endpoints internos (`--agent-listen`, loopback): `GET /healthz`, `POST /v1/internal/exec`, `POST /v1/internal/egress-check` (`{"host":…, "sandbox_id":…}` evalúa la política que el proxy aplica ahora a esa sandbox), `POST /v1/internal/ssh-agent/approve` (con confirm; `sandbox_id` obligatorio, 400 sin él; body/header `actor_sub` audit; devuelve un `approval_id` de auditoría).

`--agent-tls-listen` solo sirve `GET /healthz`, `POST /v1/internal/exec` y `POST /v1/internal/exec/stdin` al plano de control (CN `asp-control-plane`). Necesita un cert de nodo con uso de servidor: los enrolados antes de [ADR-0011](../docs/adr/0011-multi-node.md) deben re-enrolar o rotar.

Identidad del guest ([ADR-0003](../docs/adr/0003-identity.md) § 2): el token es siempre el de la sandbox de la conexión. Con `--host-vsock --reconcile` cada sandbox tiene su `{vsock}_26502`, y el proxy liga ese id a cada petición; si el guest manda un `X-ASP-Sandbox-ID` de otra sandbox, 403. `--identity-listen` y los listeners host-vsock globales no saben qué guest llama: devuelven 403 salvo con `--insecure-identity-sandbox-header` (lab), que vuelve a confiar en la cabecera.

ADR-0007 fase 4 (SSH scoped): con template, cada sandbox usa su HostSock (sin fallback a `SSH_AUTH_SOCK` global). Ops provisiona las keys en esa ruta; ASP no spawnea agents.

Agente SSH ([ADR-0003](../docs/adr/0003-identity.md) § 1, [ADR-0005](../docs/adr/0005-fase-2d-hardening.md) § 3): todas las rutas pasan por un proxy que lee mensaje a mensaje y solo reenvía `REQUEST_IDENTITIES`, `SIGN_REQUEST` y la extensión `query`. Añadir, borrar o bloquear claves recibe `SSH_AGENT_FAILURE` sin llegar al agente del host. Con `--ssh-agent-confirm`, cada acceptor `{vsock}_26501` solo firma con aprobaciones de su sandbox.

Claves persistentes: un nodo de producción (sin `--dry-run`, y con `--agent-tls-listen`, `--mtls` o un certificado en `--cert-dir`) no arranca (código 2) si `--cert-dir`, `ASP_ATTEST_KEY` (cuando se define) o `--egress-mitm-ca` (con `--egress-mitm`) están en `/tmp`, `/var/tmp`, `/dev/shm` o `$TMPDIR`: un reinicio borraría el certificado del nodo, y el bootstrap token no re-enrola un nodo vivo. `ASP_ALLOW_TMP_KEYS=1` lo fuerza; en lab solo avisa. Sin `ASP_ATTEST_KEY`, un nodo de producción firma las atestaciones solo con la clave de su certificado y no crea una clave temporal.

Renovación del certificado de nodo ([ADR-0005](../docs/adr/0005-fase-2d-hardening.md) § 1): con mTLS contra un control plane `https://`, el agente revisa su certificado al arrancar y cada 24 h. Con menos de un tercio de vida por delante pide uno nuevo con `POST /v1/nodes/{id}/rotate-cert`, autenticado por el certificado actual, lo escribe en `--cert-dir` de forma atómica y lo usa sin reiniciar (cliente del control plane, listener `--agent-tls-listen` y firma de atestaciones). Si falla con menos de 30 días por delante, avisa en el log en cada intento.

Atestación de arranque ([ADR-0003](../docs/adr/0003-identity.md) § 2): tras `running` el reconciler firma un `BootStatement` y lo envía al control plane, que solo acepta claves que conoce. Con mTLS a un control plane `https://` firma con la clave del certificado de nodo (`--cert-dir`/`client.key`); si el control plane la rechaza, prueba con `ASP_ATTEST_KEY` (por defecto `$TMPDIR/asp-attest-key.pem`, el mismo fichero que usa el control plane en un lab de un host).

El statement lleva medidas reales: el SHA-256 del kernel (`/opt/sandbox/vmlinux`), el de la imagen base de la que se copió el disco de la sandbox (`/opt/sandbox/rootfs.img`), la versión de `--ch-binary` y `boot=new|resume`. Los hashes se cachean por tamaño, mtime e inodo y se calculan en segundo plano al arrancar; el de la imagen se guarda junto al disco (`rootfs-<id>.base-sha256`), así que un `resume` declara de qué imagen salió su disco. Si no se puede leer un fichero, la sandbox arranca igual y la evidencia sale sin digests. `--print-measurement` imprime la entrada que hay que añadir a `ASP_ATTEST_ALLOWED_IMAGES` del control plane y sale (no se registra ni toca nada).

VM que muere sola ([ADR-0012](../docs/adr/0012-retained-disks.md) § 9): el agente espera el proceso de cada Cloud Hypervisor. Si acaba sin que una parada lo pidiera (OOM killer, `kill`, un fallo, el guest apagándose), en el sondeo siguiente informa la sandbox `stopped` con `status_detail=vmm_exited: <cómo acabó> after <cuánto vivió>` y conserva su disco, de modo que `asp session resume` la recupera; si el plano de control no responde, repite el informe antes de liberar nada. Una VM que muere mientras arranca falla el arranque en el acto con esa causa, sin esperar `--guest-ready-timeout`. Con `--vm-confine` el proceso es `systemd-run --wait` y su error trae el resultado de la unit (`Finished with result: signal`, `oom-kill`).

Local-net ([ADR-0010](../docs/adr/0010-on-demand-local-net.md)): el reconciler aplica el plan de cada sesión (unos 15 procesos `ip`/`wg`/`iptables`/`nft`) solo cuando cambia, publica la clave pública del nodo una vez por sandbox, y cada 30 s comprueba con un `ip link show` que el túnel `wg-asp-*` sigue ahí; si no, lo vuelve a crear.

Guest→host: ver [`scripts/guest-vsock-notes.md`](../scripts/guest-vsock-notes.md).

Fase 2e (nft + SSH guest auto): [`docs/why-2e-nft-redirect.md`](../docs/why-2e-nft-redirect.md), [`docs/why-2e-ssh-guest-mount.md`](../docs/why-2e-ssh-guest-mount.md), ADR-0006.
