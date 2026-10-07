<!-- Generado por `make docs` a partir de `asp <orden> -h` y del uso de `asp` (cli/cmd/asp). No lo edites a mano: un test falla si no coincide con el código. -->
# Referencia de `asp`

`asp` es la línea de órdenes de la API del plano de control. Uso: `asp [--config FILE] <orden> …`; `asp <orden> -h` imprime la ayuda de una orden, y esta página sale de ese texto. Las opciones se escriben con uno o dos guiones (`-tenant` o `--tenant`). `asp sandbox exec`, `asp sandbox run` y `asp session exec` terminan con el código de salida del comando que ejecutaron; `1` es un fallo de `asp` o de la API, y `2`, un uso incorrecto.

Las variables de entorno y el fichero de configuración del cliente (la URL del plano de control, las credenciales, el directorio de sesiones) están en [configuración de `asp`](configuration/cli.md).

## Opciones comunes

Las órdenes que hablan con el plano de control (todas las de `sandbox`, `session`, `node` y `apikey`) aceptan estas opciones; cada orden de más abajo lista solo las suyas.

| Opción | Tipo | Qué hace |
|---|---|---|
| `--api-key` | string | Bearer API key (default: env `ASP_API_KEY`). |
| `--control-plane-url` | string | Control-plane base URL (env `ASP_CONTROL_PLANE_URL`) (default "http://127.0.0.1:8080"). |
| `--cp-url` | value | Deprecated: use `--control-plane-url` (default http://127.0.0.1:8080). |
| `--id-token` | string | IdP access token (default: env `ASP_ID_TOKEN`). |
| `--json` | bool | Print raw JSON to stdout. |
| `--tenant` | string | tenant_id for `create/list/run` (env `ASP_TENANT`); empty: the caller's tenant, or the control plane's default. |
| `--timeout` | duration | Wait timeout for running state (default 1m0s). |

## sandbox

### `asp sandbox create`

```
asp sandbox create [flags]
```

| Opción | Tipo | Qué hace |
|---|---|---|
| `--cpu-millis` | int | cpu_millis (default 1000). |
| `--image` | string | image_ref (default "debian:bookworm-slim"). |
| `--memory-mib` | int | memory_mib (default 512). |
| `--node-id` | string | Pin to this node (default: the scheduler picks one with room). |
| `--vmm-profile` | string | vmm_profile (default "cloud-hypervisor"). |

Acepta además las [opciones comunes](#opciones-comunes).

### `asp sandbox get`

```
asp sandbox get <id>
```

Acepta además las [opciones comunes](#opciones-comunes).

### `asp sandbox list`

```
asp sandbox list [--tenant] [--all]
```

| Opción | Tipo | Qué hace |
|---|---|---|
| `--all` | bool | Include deleted sandboxes (history). |

Acepta además las [opciones comunes](#opciones-comunes).

### `asp sandbox exec`

```
asp sandbox exec <id> (--cmd '…' | -- argv…) [--cwd DIR] [--root]
```

| Opción | Tipo | Qué hace |
|---|---|---|
| `--cmd` | string | Command string (quoted words). |
| `--cwd` | string | Working directory in guest. |
| `--exec-timeout` | duration | How long the command may run (default: the control plane's limit for a buffered exec, 10m; longer needs asp session exec, which streams). |
| `--root` | bool | Run as root in the guest (default: as the owner of the workspace, or the guest's default user). |

Acepta además las [opciones comunes](#opciones-comunes).

### `asp sandbox stop`

```
asp sandbox stop <id>
```

Power off, keep the disk.

Acepta además las [opciones comunes](#opciones-comunes).

### `asp sandbox start`

```
asp sandbox start <id>
```

Resume a stopped sandbox on its disk.

Acepta además las [opciones comunes](#opciones-comunes).

### `asp sandbox delete`

```
asp sandbox delete <id>
```

Delete the sandbox and its disk.

Acepta además las [opciones comunes](#opciones-comunes).

### `asp sandbox run`

```
asp sandbox run (--cmd '…' | -- argv…) [--root] [flags]
```

| Opción | Tipo | Qué hace |
|---|---|---|
| `--cmd` | string | Command string (quoted words). |
| `--cpu-millis` | int | cpu_millis (default 1000). |
| `--cwd` | string | Working directory in guest. |
| `--exec-timeout` | duration | How long the command may run (default: the control plane's limit for a buffered exec, 10m; longer needs asp session exec, which streams). |
| `--image` | string | image_ref (default "debian:bookworm-slim"). |
| `--keep` | bool | Do not destroy sandbox on exit. |
| `--memory-mib` | int | memory_mib (default 512). |
| `--node-id` | string | Pin to this node (default: the scheduler picks one with room). |
| `--root` | bool | Run as root in the guest (default: as the owner of the workspace, or the guest's default user). |
| `--vmm-profile` | string | vmm_profile (default "cloud-hypervisor"). |

Acepta además las [opciones comunes](#opciones-comunes).

## session

### `asp session start`

```
asp session start [--name] [--local-net] [flags]
```

| Opción | Tipo | Qué hace |
|---|---|---|
| `--cpu-millis` | int | cpu_millis (default 1000). |
| `--force` | bool | Delete any sandbox recorded in the session file (with its disk), then start a new one; refuses a stopped sandbox whose disk is kept unless `--yes`. |
| `--image` | string | image_ref (default "debian:bookworm-slim"). |
| `--local-net` | bool | Send this session's default route through a tunnel the local agent opens (default off). |
| `--local-net-allow` | string | Rejected in v1 (no per-CIDR config). |
| `--memory-mib` | int | memory_mib (default 512). |
| `--name` | string | Session name (file `<session-dir>/<name>.json`) (default "default"). |
| `--node-id` | string | Pin to this node (default: the scheduler picks one with room). |
| `--session-dir` | string | Directory of named sessions (env `ASP_SESSION_DIR`) (default "/home/user/.cache/asp/sessions"). |
| `--session-file` | string | Explicit session JSON; overrides `--name` (env `ASP_SESSION_FILE` if this flag is omitted). |
| `--vmm-profile` | string | vmm_profile (default "cloud-hypervisor"). |
| `--workspace` | string | Absolute host directory to export with virtiofsd (guest image mounts tag workspace on `/workspace`; older images need mount -t virtiofs). |
| `--yes` | bool | With `--force`, delete a stopped sandbox even though its disk is kept (asp session resume would get it back). |

Acepta además las [opciones comunes](#opciones-comunes).

### `asp session exec`

```
asp session exec [--name] [--local-net] [flags]
```

| Opción | Tipo | Qué hace |
|---|---|---|
| `--buffered` | bool | Wait for the full JSON exec body instead of streaming NDJSON. |
| `--cmd` | string | Command string (quoted words). |
| `--cwd` | string | Working directory in guest. |
| `--exec-timeout` | duration | With `--buffered` or `--json`: how long the command may run (default: the control plane's limit for a buffered exec, 10m). A stream, the default, has no limit. |
| `--name` | string | Session name (file `<session-dir>/<name>.json`) (default "default"). |
| `--no-pty` | bool | Stream without a guest PTY (piped stdin gets a real EOF instead of Ctrl-D). |
| `--root` | bool | Run as root in the guest (default: as the owner of the workspace, or the guest's default user). |
| `--session-dir` | string | Directory of named sessions (env `ASP_SESSION_DIR`) (default "/home/user/.cache/asp/sessions"). |
| `--session-file` | string | Explicit session JSON; overrides `--name` (env `ASP_SESSION_FILE` if this flag is omitted). |

Acepta además las [opciones comunes](#opciones-comunes).

### `asp session status`

```
asp session status [--name] [--local-net] [flags]
```

| Opción | Tipo | Qué hace |
|---|---|---|
| `--name` | string | Session name (file `<session-dir>/<name>.json`) (default "default"). |
| `--session-dir` | string | Directory of named sessions (env `ASP_SESSION_DIR`) (default "/home/user/.cache/asp/sessions"). |
| `--session-file` | string | Explicit session JSON; overrides `--name` (env `ASP_SESSION_FILE` if this flag is omitted). |

Acepta además las [opciones comunes](#opciones-comunes).

### `asp session stop`

```
asp session stop [--name] [--local-net] [flags]
```

| Opción | Tipo | Qué hace |
|---|---|---|
| `--name` | string | Session name (file `<session-dir>/<name>.json`) (default "default"). |
| `--no-wait` | bool | Return once the stop is requested, without waiting for the sandbox to be stopped. |
| `--session-dir` | string | Directory of named sessions (env `ASP_SESSION_DIR`) (default "/home/user/.cache/asp/sessions"). |
| `--session-file` | string | Explicit session JSON; overrides `--name` (env `ASP_SESSION_FILE` if this flag is omitted). |

Acepta además las [opciones comunes](#opciones-comunes).

### `asp session resume`

```
asp session resume [--name] [--local-net] [flags]
```

| Opción | Tipo | Qué hace |
|---|---|---|
| `--name` | string | Session name (file `<session-dir>/<name>.json`) (default "default"). |
| `--session-dir` | string | Directory of named sessions (env `ASP_SESSION_DIR`) (default "/home/user/.cache/asp/sessions"). |
| `--session-file` | string | Explicit session JSON; overrides `--name` (env `ASP_SESSION_FILE` if this flag is omitted). |

Acepta además las [opciones comunes](#opciones-comunes).

### `asp session rm`

```
asp session rm [--name] [--local-net] [flags]
```

| Opción | Tipo | Qué hace |
|---|---|---|
| `--local` | bool | Clear the state file only; do not call DELETE (the sandbox and its disk are kept). |
| `--name` | string | Session name (file `<session-dir>/<name>.json`) (default "default"). |
| `--session-dir` | string | Directory of named sessions (env `ASP_SESSION_DIR`) (default "/home/user/.cache/asp/sessions"). |
| `--session-file` | string | Explicit session JSON; overrides `--name` (env `ASP_SESSION_FILE` if this flag is omitted). |

Acepta además las [opciones comunes](#opciones-comunes).

### `asp session local-net`

```
asp session local-net — full-tunnel handshake (ADR-0010)

  asp session local-net up   [--name NAME]
  asp session local-net down [--name NAME]

The session must have been started with --local-net. up writes a WireGuard
private key mode 0600 next to the session file (not inside it) and heartbeats
the control plane. When wireguard-tools and CAP_NET_ADMIN are present, up
creates the client device with ip+wg and does not install a host default route.
Otherwise it prints the exact commands. down deletes that device and detaches:
the node blackholes egress and does not fall back to the public proxy.
```

## auth

### `asp auth login`

```
asp auth login [flags]
```

| Opción | Tipo | Qué hace |
|---|---|---|
| `--cache` | string | Token cache path (default "/home/user/.cache/asp/id_token.json"). |
| `--grant` | string | password\|client_credentials (default: auto). |
| `--print-env` | bool | Print export `ASP_ID_TOKEN=…` to stdout. |
| `--print-token` | bool | Print access_token only to stdout. |
| `--secrets` | string | Credentials KEY=VALUE file (default "/home/user/.secrets/asp-keycloak-lab.txt"). |

### `asp auth logout`

```
asp auth logout [flags]
```

| Opción | Tipo | Qué hace |
|---|---|---|
| `--cache` | string | Token cache path (default "/home/user/.cache/asp/id_token.json"). |

### `asp auth status`

```
asp auth status [flags]
```

| Opción | Tipo | Qué hace |
|---|---|---|
| `--cache` | string | Token cache path (default "/home/user/.cache/asp/id_token.json"). |
| `--json` | bool | JSON status. |

## node

### `asp node list`

```
asp node list [--json]
```

Acepta además las [opciones comunes](#opciones-comunes).

### `asp node cordon`

```
asp node cordon <id>
```

Acepta además las [opciones comunes](#opciones-comunes).

### `asp node uncordon`

```
asp node uncordon <id>
```

Acepta además las [opciones comunes](#opciones-comunes).

### `asp node doctor`

```
asp node doctor <id> [--json]
```

The node checks itself: KVM, hypervisor, images, disk, nftables, clock...

Acepta además las [opciones comunes](#opciones-comunes).

### `asp node enroll-token`

```
asp node enroll-token [--node-id ID] [--ttl 1h] [--json]
```

| Opción | Tipo | Qué hace |
|---|---|---|
| `--node-id` | string | Pin the token to this node id (needed to re-key a node that is enrolled). |
| `--ttl` | duration | How long the token stays valid (at most 168h) (default 1h0m0s). |

Acepta además las [opciones comunes](#opciones-comunes).

### `asp node fence set`

```
asp node fence set <id> --endpoint URL [--token-env NAME | --token-file /abs/path | --token-stdin]
```

| Opción | Tipo | Qué hace |
|---|---|---|
| `--endpoint` | string | Where the control plane asks for the power-off: webhook URL, Redfish base URL or IPMI host. |
| `--token-env` | string | Credential: the control plane's environment variable NAME. |
| `--token-file` | string | Credential: an absolute path on the control plane's host. |
| `--token-stdin` | bool | Read the credential from standard input. |

Acepta además las [opciones comunes](#opciones-comunes).

### `asp node fence clear`

```
asp node fence clear <id>
```

Acepta además las [opciones comunes](#opciones-comunes).

## doctor

### `asp doctor`

The same on this host, as root, without starting the agent.

```
usage: asp doctor [--env-file FILE] [--node-agent PATH] [--json] [-- node-agent flags...]
Runs the node-agent's host checks with this node's settings: the node-agent reads /etc/asp/agent.yaml
(and agent.yaml.d/) itself. Flags after -- go to the node-agent (--config FILE, --disk-dir, ...).
```

| Opción | Tipo | Qué hace |
|---|---|---|
| `--env-file` | string | KEY=VALUE settings for the node-agent, for a unit that loads an EnvironmentFile (the node-agent reads `/etc/asp/agent.yaml` itself). |
| `--json` | bool | Print the report as JSON. |
| `--node-agent` | string | The node-agent binary (default: node-agent on PATH, then `/usr/local/bin/node-agent`). |

## image

### `asp image pull`

```
asp image pull [--version V] [--dir D] [--link D|--no-link]
```

Install the guest kernel and image of a release, checked against its SHA256SUMS.

| Opción | Tipo | Qué hace |
|---|---|---|
| `--base-url` | string | Where the release's files are (default: this project's GitHub release of `--version`). |
| `--dir` | string | Where sets are installed: `<dir>/<version>`, and `<dir>/current` for the last (default "/var/lib/asp/images"). |
| `--json` | bool | Print the result as JSON. |
| `--link` | string | Directory the node-agent reads by default; vmlinux, rootfs.img, SHA256SUMS and image.json are linked there (a file there that is not a link is left alone) (default "/opt/sandbox"). |
| `--no-link` | bool | Do not link anything into `--link`. |
| `--timeout` | duration | Give up after this long (default 30m0s). |
| `--version` | string | Release to install, e.g. 0.1.0 (default: the version of this asp, when it is a release). |

### `asp image verify`

```
asp image verify [dir]
```

Check an installed kernel and image against their SHA256SUMS (default `/opt/sandbox`).

| Opción | Tipo | Qué hace |
|---|---|---|
| `--json` | bool | Print the result as JSON. |

## apikey

### `asp apikey create`

```
asp apikey create --name N [--tenant T] [--scope tenant|platform] [--ttl 30d]
```

| Opción | Tipo | Qué hace |
|---|---|---|
| `--name` | string | Key name, unique within the tenant (letters, digits, . _ -). |
| `--scope` | string | Tenant (default) or platform (every tenant and the node routes; platform keys only). |
| `--ttl` | string | How long the key lasts, e.g. 720h or 30d (default: no expiry). |

Acepta además las [opciones comunes](#opciones-comunes).

### `asp apikey list`

```
asp apikey list [--tenant T] [--json]
```

Acepta además las [opciones comunes](#opciones-comunes).

### `asp apikey revoke`

```
asp apikey revoke <id>
```

Acepta además las [opciones comunes](#opciones-comunes).

### `asp apikey rotate`

```
asp apikey rotate <id>
```

Acepta además las [opciones comunes](#opciones-comunes).

## config

### `asp config show`

Where the settings come from: the config files, the environment, the defaults.

```
usage: asp config show [--effective] [--component asp|server|agent] [--config FILE]
Shows where each setting comes from: the files, the environment, the defaults.
  --effective         also list the settings nothing sets, with their defaults
  --component NAME    asp (the default) shows this command's own settings; server and agent show what the
                      merged files of the control plane (/etc/asp/server.yaml) or the node-agent
                      (/etc/asp/agent.yaml) say
  --config FILE       read this file instead of /etc/asp/asp.yaml and ~/.config/asp/asp.yaml
```

## version

### `asp version`

```
asp version [--json]
```

| Opción | Tipo | Qué hace |
|---|---|---|
| `--json` | bool | Print the build information as JSON. |
