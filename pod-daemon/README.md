# pod-daemon

Daemon Rust dentro de la microVM. Sirve HTTP/1.1 JSON:

- `GET /healthz` → `{"status":"ok"}`
- `POST /v1/exec` body `{"cmd":["echo","hi"],"env":{},"cwd":"","as_root":false}` → `{"stdout","stderr","exit_code"}` (sync + timeout)

El exec acumulado lee stdout y stderr mientras el comando corre y escribe `stdin` desde otro hilo, así que no se bloquea por mucha salida ni por mucha entrada. Cuando el comando termina, espera como mucho 1 s a que se cierren sus pipes: un proceso que lanzó en segundo plano y que heredó stdout no lo retiene. Si vence `--exec-timeout-secs`, mata el comando y devuelve lo que llevaba escrito con `exit_code` 124 y el aviso al final de `stderr`.

Las peticiones acumuladas (`/healthz`, `/v1/exec` sin stream, `/v1/exec/stdin`) dejan la conexión abierta para la siguiente (HTTP/1.1 keep-alive) salvo que el cliente mande `Connection: close`; el node-agent reutiliza así la conexión vsock en vez de repetir el `CONNECT` en cada llamada. El stream siempre cierra la conexión al terminar.

El exec en streaming (`?stream=1`, también PTY) no tiene límite total: termina cuando el comando sale o cuando el cliente cierra la conexión (entonces mata el comando y su grupo de procesos). `--stream-idle-timeout-secs N` (0 por defecto) lo mata tras N segundos sin salida ni stdin, con `exit_code` 124.

## Quién ejecuta un comando y con qué límites

pod-daemon corre como root en el guest: tiene que poder lanzar comandos como otros usuarios, y como root cuando se pide. Pero **un comando no es root salvo que su petición diga `"as_root": true`** (`asp session exec --root`, `asp sandbox exec --root`):

| Petición | El comando corre como |
|---|---|
| sin `as_root` | el **dueño de `--workspace-dir`** (por defecto `/workspace`), si no es root: virtiofs no traduce ids, así que los ficheros que escribe conservan el uid del host. Con el gid del directorio, sin grupos suplementarios y con `HOME` (el de su cuenta, o `/tmp`) |
| sin `as_root`, workspace de root o sin workspace | la cuenta `--exec-user` (por defecto `sandboxd`, `HOME=/var/lib/sandboxd`). Si esa cuenta no existe en el guest el exec falla con un mensaje que lo dice: nunca cae a root en silencio |
| `as_root` | root |

`USER`, `LOGNAME` y `HOME` van antes que el `env` de la petición, que los puede pisar. El directorio de trabajo se cambia en el hijo **después** de cambiar de usuario, como `su`: el usuario tiene que poder entrar. Con un pod-daemon que no es root (las imágenes OpenRC y la del Dockerfile lo lanzan como `sandboxd`, y el dry-run en el host) nada cambia de usuario: los comandos corren como el propio daemon y `as_root` se rechaza con **403** (`as_root needs pod-daemon to run as root`) en lugar de ejecutarse sin serlo. `--exec-user none` devuelve el comportamiento anterior (todo con la identidad del daemon).

El PTY de un comando es del usuario con el que corre (`fchown` del slave), para que pueda reabrir su terminal.

Límites de **cada** comando, también con `as_root`:

| Flag | Defecto | Efecto |
|---|---|---|
| `--exec-max-procs` | 4096 | `RLIMIT_NPROC` (por usuario) y `pids.max` del cgroup; 0 = ninguno |
| `--exec-max-open-files` | 65536 | `RLIMIT_NOFILE`; 0 = no tocar |
| `--exec-core-dumps` | apagado | sin él, `RLIMIT_CORE` es 0: un fallo no escribe la memoria del proceso (secretos incluidos) junto al workspace |
| `--exec-max-memory-percent` | 80 | `memory.max` del cgroup, en % de la memoria del guest; 0 = ninguno |
| `--exec-cgroup` | `/sys/fs/cgroup/asp-exec` | cgroup2 que se crea al arrancar y al que cada comando se une (escribe 0 en `cgroup.procs` antes de cambiar de usuario). Vacío lo desactiva; sin cgroup2, sin los controladores o sin ser root se omite con un aviso |

Los rlimits se **bajan**, nunca se suben: blando y duro valen lo pedido, o el límite duro que el daemon ya tenía si es menor, así que el comando no puede levantarlos. Un fork bomb o una reserva descontrolada choca con el cgroup, no con el guest: el daemon, systemd y el proxy del agente SSH siguen vivos. Si un comando no puede unirse al cgroup o aplicar sus límites, **no se ejecuta** (error 500) en vez de correr sin ellos.

## Quién puede hablar con el daemon

Lo que llegue al listener ejecuta comandos, y con `as_root` como root. Por eso el listener contesta al host y a nadie más:

- **vsock** (productivo): solo contesta si el CID de origen es el del host (`ASP_HOST_CID`, 2). Un proceso del guest que marque el daemon por el transporte loopback de vsock viene del CID 1 y se descarta.
- **tcp**: por defecto `--tcp-addr auto[:PUERTO]` escucha en la dirección IPv4 de la interfaz que tiene la ruta por defecto (el TAP) y solo contesta a la **puerta de enlace** (el extremo del host del /30), o a `--tcp-peer IP`. Un proceso del guest que conecte a la dirección del propio guest llega desde esa dirección y se descarta. Una dirección comodín (`0.0.0.0`, `::`) se rechaza al arrancar: `--tcp-any-interface` la permite en un lab.
- **unix**: el socket queda con modo `0600`.

## Listen modes

| `--listen` | Flag extra | Uso |
|---|---|---|
| `unix` (default) | `--unix-socket` | Dry-run / lab en el host |
| `vsock` | `--vsock-port` (default **26500**) | **Productivo CH**: AF_VSOCK `CID_ANY:port` en guest; host diala hybrid UDS `CONNECT <port>` |
| `tcp` | `--tcp-addr` (default `auto`), `--tcp-peer` | Alternativa lab vía TAP (host diala IP guest, no vsock). Escucha solo en la dirección del TAP y solo contesta al host; ver *Quién puede hablar con el daemon* |

```bash
cargo test
cargo run -- --listen unix --unix-socket /tmp/pod-daemon.sock
# En guest (imagen CH):
cargo run -- --listen vsock --vsock-port 26500
# Alternativa TAP (auto = la dirección de la interfaz con la ruta por defecto):
cargo run -- --listen tcp --tcp-addr auto:26500
cargo run -- --help
```

Los RPC de `proto/pod_daemon.proto` siguen pendientes (sin tonic en este MVP).

## SSH / identity (guest → host CID 2)

Env: `ASP_HOST_CID=2` (hypervisor/host).

| Puerto | Uso |
|---|---|
| 26501 | SSH agent (node-agent `--host-vsock`) |
| 26502 | OIDC identity HTTP (`POST /v1/tokens/oidc`) |

Flags:

- `--ssh-auth-bridge` / `--ssh-auth-socket` / `--ssh-auth-sock` — path esperado de un socket **reenviado desde el host** (socat VSOCK-CONNECT:2:26501 o virtiofs). El proxy del agente (solo listar claves y firmar) vive en node-agent.
- `--identity-socket` — path unix local opcional; en productivo preferir dial vsock CID 2:26502.

Detalle: [`scripts/guest-vsock-notes.md`](../scripts/guest-vsock-notes.md).
