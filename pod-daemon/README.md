# pod-daemon

Daemon Rust dentro de la microVM. Sirve HTTP/1.1 JSON:

- `GET /healthz` → `{"status":"ok"}`
- `POST /v1/exec` body `{"cmd":["echo","hi"],"env":{},"cwd":""}` → `{"stdout","stderr","exit_code"}` (sync + timeout)

El exec acumulado lee stdout y stderr mientras el comando corre y escribe `stdin` desde otro hilo, así que no se bloquea por mucha salida ni por mucha entrada. Cuando el comando termina, espera como mucho 1 s a que se cierren sus pipes: un proceso que lanzó en segundo plano y que heredó stdout no lo retiene. Si vence `--exec-timeout-secs`, mata el comando y devuelve lo que llevaba escrito con `exit_code` 124 y el aviso al final de `stderr`.

Las peticiones acumuladas (`/healthz`, `/v1/exec` sin stream, `/v1/exec/stdin`) dejan la conexión abierta para la siguiente (HTTP/1.1 keep-alive) salvo que el cliente mande `Connection: close`; el node-agent reutiliza así la conexión vsock en vez de repetir el `CONNECT` en cada llamada. El stream siempre cierra la conexión al terminar.

El exec en streaming (`?stream=1`, también PTY) no tiene límite total: termina cuando el comando sale o cuando el cliente cierra la conexión (entonces mata el comando y su grupo de procesos). `--stream-idle-timeout-secs N` (0 por defecto) lo mata tras N segundos sin salida ni stdin, con `exit_code` 124.

## Listen modes

| `--listen` | Flag extra | Uso |
|---|---|---|
| `unix` (default) | `--unix-socket` | Dry-run / lab en el host |
| `vsock` | `--vsock-port` (default **26500**) | **Productivo CH**: AF_VSOCK `CID_ANY:port` en guest; host diala hybrid UDS `CONNECT <port>` |
| `tcp` | `--tcp-addr` (default `0.0.0.0:26500`) | Alternativa lab vía TAP (host diala IP guest, no vsock) |

```bash
cargo test
cargo run -- --listen unix --unix-socket /tmp/pod-daemon.sock
# En guest (imagen CH):
cargo run -- --listen vsock --vsock-port 26500
# Alternativa TAP:
cargo run -- --listen tcp --tcp-addr 0.0.0.0:26500
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
