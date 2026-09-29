# pod-daemon

Daemon Rust dentro de la microVM. Sirve HTTP/1.1 JSON:

- `GET /healthz` → `{"status":"ok"}`
- `POST /v1/exec` body `{"cmd":["echo","hi"],"env":{},"cwd":""}` → `{"stdout","stderr","exit_code"}` (sync + timeout)

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

- `--ssh-auth-bridge` / `--ssh-auth-socket` / `--ssh-auth-sock` — path esperado de un socket **reenviado desde el host** (socat VSOCK-CONNECT:2:26501 o virtiofs). El byte-pump vive en node-agent.
- `--identity-socket` — path unix local opcional; en productivo preferir dial vsock CID 2:26502.

Detalle: [`scripts/guest-vsock-notes.md`](../scripts/guest-vsock-notes.md).
