# Node agent

Agente privilegiado en cada nodo de sandboxes. Habla con Cloud Hypervisor vía HTTP sobre Unix socket, se enrolla/registra en el control plane (mTLS opcional), expone proxy localhost de exec/egress-check hacia `pod-daemon`, identity proxy OIDC, bridge de SSH agent, **host-vsock** guest→host y **TAP auto**.

## Configuración

Cada ajuste es una opción de la línea de órdenes, una variable de entorno `ASP_*` y una clave del fichero `/etc/asp/agent.yaml`, con una sola regla de nombres (`internal/settings`; el test `TestEverySettingHasOneNameWithThePrefix` la comprueba) y esta prioridad: opción > variable > fichero > valor por defecto. **La lista completa, con valor por defecto y descripción, se genera del código: [configuración del node-agent](../docs/reference/configuration/node-agent.md).** `asp-node-agent -h` imprime las mismas descripciones, y `--print-config` lo que vale cada ajuste en este nodo y de dónde viene. Las reglas comunes a todos los programas (booleanos, nombres que cambiaron) están en [Configuración](../docs/reference/configuration.md), y el fichero, con ejemplos, en [el fichero de configuración](../docs/how-to/config-file.md).

`--reap-only` y `--print-measurement` son acciones, no ajustes: solo tienen opción.

## Ejecutarlo en desarrollo

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

## Comportamiento

**Trabajadores del reconciler** (`--reconcile-workers`). Un arranque lento (timeout de CH, copia de un rootfs grande) no retrasa al resto. Una misma sandbox nunca la llevan dos trabajadores, y un `stopping` que llega durante su arranque se atiende al terminar este. Un pánico en un trabajador se registra con su traza y termina solo esa tarea: la sandbox queda `failed` (primer arranque), `stopped` con su disco (reanudación o parada) o `deleted`, con `status_detail=panic: …`; el agente y las demás VMs siguen. Lo mismo vale para un sondeo y para las conexiones del agente SSH del guest.

**Un arranque que no llega a estar listo** (`--guest-ready-timeout`). CH está arriba antes de que el guest arranque (unos 3 s en un host KVM) y `asp sandbox run` hace el exec en cuanto ve `running`, así que el agente espera a que el pod-daemon de la VM responda a `/healthz` antes de informar `running`. Pasado el plazo el arranque **falla**: se para la VM, se registra en el log lo último que el guest escribió en su consola serie (el agente guarda los últimos 32 KiB en memoria, nunca en disco), se informa `failed` con `status_detail=guest_not_ready: … (console: …)` y se borra el disco recién copiado. Una reanudación vuelve a `stopped` con su disco (`resume failed: guest_not_ready: …`).

**Parar conservando el disco** (`--stop-grace`). El nodo pide al guest `sync; systemctl poweroff --no-block` por el pod-daemon y espera a que Cloud Hypervisor salga; si no sale a tiempo, parada brusca con un aviso. El botón ACPI no sirve con la imagen actual (no tiene `logind`).

Política de egress ([ADR-0002](../docs/adr/0002-networking.md)): cada sondeo de `/work` trae la política efectiva del tenant de cada sandbox asignada y su versión. El reconciler la aplica antes del primer arranque (el guest no espera a un exec para tener red) y otra vez cuando cambia la versión, así que un `PUT /v1/tenants/{id}/egress` llega a las sandboxes en marcha en el siguiente sondeo. Sin política todavía, deny-default. El proxy no reenvía cabeceras hop-by-hop (`Connection`, `Upgrade`, `Keep-Alive`, `Proxy-Authorization`…) y limita el cuerpo de las peticiones HTTP (`ASP_EGRESS_MAX_BODY`, 413); las respuestas pasan enteras, como por un túnel CONNECT.

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

Redirect de nftables y agente SSH en el guest: [ADR-0006](../docs/adr/0006-fase-2e-nft-ssh-guest.md).
