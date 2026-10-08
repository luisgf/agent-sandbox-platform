# API del plano de control

> Esta página se genera de [`openapi.yaml`](../../control-plane/internal/api/openapi.yaml) con `make docs`: no se edita a mano. Un test compara el documento con las rutas, los tipos y los estados que devuelve el código, así que no se queda atrás.

Referencia de la API HTTP del plano de control, bajo `/v1`. El plano de control sirve el mismo documento como JSON en `GET /openapi.json`, sin credenciales, para generar un cliente (`oapi-codegen`, `openapi-generator`) o abrirlo en Swagger UI o Redoc. Los textos del documento están en inglés, como los mensajes de error de la API.

## Convenciones

The HTTP API of the Agent Sandbox Platform control plane: it creates sandboxes (microVMs) on
the nodes, runs commands in them, and manages the nodes, API keys and egress policy.

This document is written by hand and checked against the code: a test fails when a route of the
control plane is missing here (or the other way round), when a field of a JSON body differs
from the Go type that carries it, or when a handler can answer a status that is not listed.
The reference page `docs/reference/api.md` is generated from it (`make docs`). While ASP is
0.x, a minor release may change the API; [CHANGELOG.md](https://github.com/luisgf/agent-sandbox-platform/blob/main/CHANGELOG.md) says so.

### Authentication

Every route except the public ones needs `Authorization: Bearer <token>`. A token is one of:

* an **API key** (`asp_` and 40 hexadecimal digits), made with `POST /v1/api-keys`. A *tenant*
  key acts only within its tenant. A *platform* key sees every tenant and may call the node
  routes, which a tenant key never can.
* an **IdP access token**: a JWT of the OIDC provider the control plane trusts
  (`ASP_IDP_ISSUER`). The caller's tenant, subject, e-mail and role come from its claims. With
  `ASP_IDP_REQUIRED=1` the routes for people accept nothing else.

Node agents authenticate on the node routes with their client certificate over mutual TLS
(the control plane has `ASP_CLIENT_CA`): the certificate names the node (CN, OU `nodes`) and a
node can act only for itself. Without a client CA a platform key does instead.

Without any key or IdP the control plane refuses every request unless it runs with
`ASP_INSECURE_OPEN_API=1`: a laptop and the dry-run smokes, never a shared host.

#### Roles

With an IdP token the role decides what the caller may do. `admin` can do everything of a
tenant and manage nodes, API keys and the egress policy. `operator` lists and gets every
sandbox of the tenant, lists nodes, and acts on sandboxes it does not own only with the grants
`exec-any` and `destroy-any`. `user` creates sandboxes and acts on its own. `viewer` only
reads. An API key has no role: it is trusted with everything within its scope, except that
only a platform key manages nodes and API keys.

#### Tenants

A caller confined to a tenant (a tenant key, an IdP token) sees that tenant only: a sandbox of
another tenant answers 404 as if it did not exist, and naming another tenant answers 403.

### Errors

An error is always `{"error": "<message>"}` with one of these statuses; the message is meant
for people and may change.

| Status | Meaning |
|---|---|
| 400 | The body is not JSON, a field is missing or out of range, or a parameter is invalid |
| 401 | No credential, an unknown or revoked one, or an IdP token that is not valid |
| 403 | The credential is valid but the role, the tenant or the node identity does not allow it |
| 404 | The sandbox, node, key or tenant does not exist (or is another tenant's) |
| 409 | The request conflicts with the state of the resource: a sandbox that is not running, a node that is revoked or cannot take sandboxes |
| 413 | The body is larger than the limit (1 MiB; 64 KiB for the small node calls) |
| 502, 504 | The node agent did not answer, or a buffered exec exceeded its time limit |
| 503 | Not configured on this control plane (attestation, OIDC, enrollment), or **no node has room**: then `Retry-After: 30` and `reasons` count why each node was skipped |

### Sandbox states

`requested` (placed on a node, waiting for it) → `starting` (the node claimed it) → `running`.
A stop goes `stopping` → `stopped`, and **keeps the disk** until a resume (`POST …/start`) or
a delete; a stopped sandbox expires after a retention period. A delete goes `deleting` →
`deleted` and removes the disk; the row stays for the audit trail. `failed` is a sandbox the
node could not start, or lost with its node (`stop_reason` says which). `scheduled` and
`paused` are accepted states a node may report; the control plane does not use them today.

### Commands and streaming

`POST /v1/sandboxes/{id}/exec` runs a command and answers once it exits (JSON), or with
`?stream=1` (or `Accept: application/x-ndjson`) streams its output as it comes: one JSON object
per line, see `ExecStreamEvent`. A streamed command has no time limit; a buffered one is
cut at `ASP_BUFFERED_EXEC_TIMEOUT` (10 minutes) and answers 504.

## Quién puede llamar a qué

Cada operación lleva un valor de acceso:

| Acceso | Quién puede llamar |
|---|---|
| `public` | Cualquiera, sin credencial. |
| `tenant` | Una clave de API o un token del IdP, dentro de su tenant. Con un token del IdP decide el rol: cada operación dice cuál. |
| `operator` | Rol `admin` u `operator` del IdP, o una clave de plataforma. |
| `admin` | Rol `admin` del IdP, o una clave de plataforma. |
| `node` | Un node-agent: su certificado de cliente (mTLS) o una clave de plataforma. |

Con un token del IdP, el rol acota lo que se puede hacer dentro de cada acceso. Cómo se conecta un IdP: [conectar un IdP](../how-to/idp.md).

## Operaciones

### Sandboxes

Create a sandbox, look at it, stop it, resume it and delete it.

| Operación | Qué hace | Acceso |
|---|---|---|
| [`POST /v1/sandboxes`](#createsandbox) | Create a sandbox | `tenant` |
| [`GET /v1/sandboxes`](#listsandboxes) | List sandboxes | `tenant` |
| [`GET /v1/sandboxes/{id}`](#getsandbox) | Get a sandbox | `tenant` |
| [`DELETE /v1/sandboxes/{id}`](#deletesandbox) | Delete a sandbox and its disk | `tenant` |
| [`POST /v1/sandboxes/{id}/stop`](#stopsandbox) | Stop a sandbox and keep its disk | `tenant` |
| [`POST /v1/sandboxes/{id}/start`](#startsandbox) | Resume a stopped sandbox | `tenant` |
| [`GET /v1/sandboxes/{id}/events`](#listsandboxevents) | The audit trail of a sandbox | `tenant` |

#### `POST /v1/sandboxes` <a id="createsandbox"></a>

**Create a sandbox**. Acceso: `tenant`. Autenticación: clave de API o token del IdP.

Places a new sandbox on a node with room for it and answers 201 with the sandbox in state
`requested`: the node claims it on its next poll and boots it, so poll `GET /v1/sandboxes/{id}`
until it is `running`. With `ASP_AUTO_PROVISION=1` (dry-run, no nodes) the control plane moves
it to `running` itself.

If no node has room the answer is **503** with `Retry-After: 30` and `reasons`; naming a
node (`node_id`) that cannot take sandboxes is a **409**. The scheduler is described in ADR-0011.

`tenant_id` defaults to the caller's tenant, or to `ASP_DEFAULT_TENANT` (`default`) for a
caller that sees every tenant. An IdP caller needs the role `user`, `operator` or `admin` and
becomes the owner (`owner_sub`, `owner_email` come from its token; naming another owner is
403). An API key cannot name an owner either: the sandbox has none and the key is the actor.
When `ASP_WORKSPACE_ROOTS` is set, `workspace_host_path` must be inside `<root>/<tenant>` for one of the roots.
`local_net` opts into the tunnel; v1 has no policy for it, so the keys `local_net_policy`,
`prefixes`, `cidrs`, `cidr`, `ports`, `routes`, `exceptions` and `local_net_allow` are 400,
and a request that says it is from the guest (`X-ASP-Caller: guest`) may not set it (403).

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `X-ASP-Actor-Sub` | cabecera | string | The actor to record in the audit trail. Honoured only in the open lab (`ASP_INSECURE_OPEN_API=1`), where nothing authenticates the caller; anyone else is attributed from their credential and the header is ignored. |

**Cuerpo** (obligatorio): [CreateSandboxRequest](#schema-createsandboxrequest)

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 201 | The sandbox, placed on a node. | [Sandbox](#schema-sandbox) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 409 | The node named in `node_id` does not exist, is revoked, or cannot take sandboxes (it is cordoned, offline, or does not accept work). | [Error](#schema-error) |
| 413 | The body is larger than the limit (1 MiB; 64 KiB for the calls of nodes and keys). | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |
| 503 | No node can take it. `reasons` counts, for each reason, how many nodes were skipped for it. Capacity frees as sandboxes stop: try again after `Retry-After`. Cabecera `Retry-After`: Seconds to wait before trying again. Always `30`. | [PlacementError](#schema-placementerror) |

#### `GET /v1/sandboxes` <a id="listsandboxes"></a>

**List sandboxes**. Acceso: `tenant`. Autenticación: clave de API o token del IdP.

The sandboxes of a tenant, most recent first. A caller confined to a tenant gets that tenant
whatever it asks (asking for another is 403); one that sees every tenant gets all of them,
or those of `tenant_id`. The deleted ones are history and are left out unless
`include_deleted` is `1`. With an IdP token the role `user` sees only its own sandboxes;
`admin`, `operator` and `viewer` see the tenant's.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `tenant_id` | consulta | string | Only this tenant's. A caller confined to a tenant gets its own whatever it asks, and naming another tenant is 403. |
| `include_deleted` | consulta | string: `1` \| `true` | `1` or `true` also lists the deleted sandboxes, which are history. |

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The sandboxes. | [SandboxList](#schema-sandboxlist) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

#### `GET /v1/sandboxes/{id}` <a id="getsandbox"></a>

**Get a sandbox**. Acceso: `tenant`. Autenticación: clave de API o token del IdP.

The sandbox and the state its node last reported. An IdP caller needs to be its owner or an `admin`, `operator` or `viewer`.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the sandbox. |

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The sandbox. | [Sandbox](#schema-sandbox) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

#### `DELETE /v1/sandboxes/{id}` <a id="deletesandbox"></a>

**Delete a sandbox and its disk**. Acceso: `tenant`. Autenticación: clave de API o token del IdP.

Deletes the VM and its disk. The sandbox goes to `deleting`, and to `deleted` when its node
reports it removed (at once when no node holds anything). The row stays for the audit trail.
Deleting a sandbox that is being or already deleted changes nothing and answers 200.
An IdP caller needs to be the owner, an `admin`, or an `operator` with `destroy-any`.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the sandbox. |

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The sandbox, `deleting` or `deleted`. | [Sandbox](#schema-sandbox) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

#### `POST /v1/sandboxes/{id}/stop` <a id="stopsandbox"></a>

**Stop a sandbox and keep its disk**. Acceso: `tenant`. Autenticación: clave de API o token del IdP.

Powers the VM off and keeps its disk (ADR-0012): the sandbox goes to `stopping`, and to
`stopped` when its node reports it (at once if it was never claimed). `POST …/start`
boots it again on that disk; a stopped sandbox is deleted after the retention period
(`ASP_STOPPED_SANDBOX_TTL`, 7 days) or beyond the per-tenant cap. Stopping a sandbox that is
already stopping or stopped changes nothing and answers 200; one that failed or is being
deleted cannot be stopped (409).
An IdP caller needs to be the owner, an `admin`, or an `operator` with `destroy-any`.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the sandbox. |

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The sandbox, `stopping` or `stopped`. | [Sandbox](#schema-sandbox) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 409 | The request conflicts with the state of the resource. The message says how. | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

#### `POST /v1/sandboxes/{id}/start` <a id="startsandbox"></a>

**Resume a stopped sandbox**. Acceso: `tenant`. Autenticación: clave de API o token del IdP.

Boots a `stopped` sandbox again, on the node that holds its disk: it goes back to `requested`
and `boot_count` goes up by one. It takes room on that node like a create does, so it can answer
**503** (no room there) and needs the right to create as well as the right to stop: the owner,
an `admin` or an `operator` with `destroy-any`, who also may create. A sandbox that is already
starting or running is left as it is (200); one that is stopping, failed or deleted cannot be resumed
(409), and neither can one whose node cannot take sandboxes.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the sandbox. |

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The sandbox, back in `requested`. | [Sandbox](#schema-sandbox) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 409 | The request conflicts with the state of the resource. The message says how. | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |
| 503 | No node can take it. `reasons` counts, for each reason, how many nodes were skipped for it. Capacity frees as sandboxes stop: try again after `Retry-After`. Cabecera `Retry-After`: Seconds to wait before trying again. Always `30`. | [PlacementError](#schema-placementerror) |

#### `GET /v1/sandboxes/{id}/events` <a id="listsandboxevents"></a>

**The audit trail of a sandbox**. Acceso: `tenant`. Autenticación: clave de API o token del IdP.

Every step of the sandbox's life in order, with who did it (`actor`, `actor_sub`). Same access as getting the sandbox.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the sandbox. |

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The events, oldest first. | [SandboxEventList](#schema-sandboxeventlist) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

### Exec

Run commands in a running sandbox, buffered or streamed.

| Operación | Qué hace | Acceso |
|---|---|---|
| [`POST /v1/sandboxes/{id}/exec`](#execsandbox) | Run a command in a sandbox | `tenant` |
| [`POST /v1/sandboxes/{id}/exec/stdin`](#execstdin) | Send input to a streamed command | `tenant` |

#### `POST /v1/sandboxes/{id}/exec` <a id="execsandbox"></a>

**Run a command in a sandbox**. Acceso: `tenant`. Autenticación: clave de API o token del IdP.

Runs `cmd` in a running sandbox, through the control plane to the agent of its node and the
daemon in the guest. The control plane sends the tenant's egress policy along.

**Buffered** (the default): the answer comes when the command exits, as `ExecResponse` (the exit code
of a command that fails is in `exit_code`, the HTTP status is still 200). It is cut at the smaller of
`timeout_seconds` and `ASP_BUFFERED_EXEC_TIMEOUT` (10 minutes by default; a `timeout_seconds` of 0 means
that limit): then 504. **Streamed** (`?stream=1`, or `Accept: application/x-ndjson`): the answer is
`application/x-ndjson`, one `ExecStreamEvent` per line, as the command writes, and ends with
an `exit` event. A stream has no time limit and ends when the command does, or when the caller
disconnects (the command is killed).

With `pty` the guest runs the command under a pseudoterminal; with `pty` or `stdin_stream` the
first event is `ready` with an `exec_id`, and the caller sends input and resizes with
`POST …/exec/stdin`. `stdin` is the input of a buffered command, given up front. By default a
command runs as the owner of the workspace, or as the guest's default user; `as_root` runs it as root.

A command is activity: it keeps the sandbox from being stopped for idleness while it runs,
and a `sandbox.exec` event records it (how many arguments it had and its exit code; never the command line).
Only a `running` sandbox can run a command: any other state is 409 and the message says what to do.
An IdP caller needs to be the owner, an `admin`, or an `operator` with `exec-any`. The actor
in the audit trail is the caller's credential; `actor_sub` and `X-ASP-Actor-Sub` count only
in the open lab, where nothing authenticates.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the sandbox. |
| `stream` | consulta | string: `1` \| `true` \| `yes` | `1`, `true` or `yes` streams the output as NDJSON; `Accept: application/x-ndjson` does the same. |
| `X-ASP-Actor-Sub` | cabecera | string | The actor to record in the audit trail. Honoured only in the open lab (`ASP_INSECURE_OPEN_API=1`), where nothing authenticates the caller; anyone else is attributed from their credential and the header is ignored. |

**Cuerpo** (obligatorio): [ExecRequest](#schema-execrequest)

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The result of a buffered command, or the events of a streamed one. | `application/json`: [ExecResponse](#schema-execresponse); `application/x-ndjson`: [ExecStreamEvent](#schema-execstreamevent), uno por línea |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 409 | The sandbox is not running (the message says what to do), or the agent has no guest for it. | [Error](#schema-error) |
| 413 | The body is larger than the limit (1 MiB; 64 KiB for the calls of nodes and keys). | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |
| 502 | The node agent did not answer, or refused the control plane. The message says which. | [Error](#schema-error) |
| 504 | A buffered command exceeded its time limit. Stream it, or raise `timeout_seconds` up to `ASP_BUFFERED_EXEC_TIMEOUT`. | [Error](#schema-error) |

#### `POST /v1/sandboxes/{id}/exec/stdin` <a id="execstdin"></a>

**Send input to a streamed command**. Acceso: `tenant`. Autenticación: clave de API o token del IdP.

Sends keystrokes or data (`data`), the end of input (`close`), or a new terminal size
(`rows`, `cols`) to a command started with `pty` or `stdin_stream` whose stream answered
`ready`. It does not start a command. A successful call counts as activity, so a terminal that
is waiting for input is not reaped. Same access as `exec`.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the sandbox. |

**Cuerpo** (obligatorio): [ExecStdinRequest](#schema-execstdinrequest)

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The input was delivered. | [ExecStdinAck](#schema-execstdinack) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 409 | The sandbox is not running, or no command with that `exec_id` is running in it. | [Error](#schema-error) |
| 413 | The body is larger than the limit (1 MiB; 64 KiB for the calls of nodes and keys). | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |
| 502 | The node agent did not answer, or refused the control plane. The message says which. | [Error](#schema-error) |

### Local network

The opt-in tunnel that sends a sandbox's traffic out through the caller's own network (ADR-0010).

| Operación | Qué hace | Acceso |
|---|---|---|
| [`POST /v1/sandboxes/{id}/local-net/grant`](#issuelocalnetgrant) | Get a grant for the local-net tunnel | `tenant` |
| [`POST /v1/sandboxes/{id}/local-net/heartbeat`](#heartbeatlocalnet) | Bring the tunnel up, or keep it up | `tenant` |
| [`DELETE /v1/sandboxes/{id}/local-net/attach`](#detachlocalnet) | Take the tunnel down | `tenant` |

#### `POST /v1/sandboxes/{id}/local-net/grant` <a id="issuelocalnetgrant"></a>

**Get a grant for the local-net tunnel**. Acceso: `tenant`. Autenticación: clave de API o token del IdP.

For a sandbox created with `local_net: true`: issues a one-time `grant` (valid 10 minutes) that the
caller's CLI shows to `…/local-net/heartbeat` to bring the tunnel up, with where to dial and what
the node published for it. It does not turn `local_net` on. Only the owner may take it
(403 for anyone else, an `admin` included); a guest may not at all.
The node's `listen_port` and tunnel addresses are 0 and empty until the node has published them: retry.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the sandbox. |

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The grant. | [LocalNetGrant](#schema-localnetgrant) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 409 | The sandbox did not ask for `local_net`, or it is not active. | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

#### `POST /v1/sandboxes/{id}/local-net/heartbeat` <a id="heartbeatlocalnet"></a>

**Bring the tunnel up, or keep it up**. Acceso: `tenant`. Autenticación: clave de API o token del IdP.

The caller's CLI presents the `grant` and its WireGuard public key (32 bytes, base64): a valid pair
moves `local_net_state` to `up`. It does not count as activity. A grant that expired withdraws the
tunnel (egress stays closed, it does not fall back to the public proxy) and answers 401, as does a
grant that is wrong. Only the owner may call it.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the sandbox. |

**Cuerpo** (obligatorio): [LocalNetHeartbeatRequest](#schema-localnetheartbeatrequest)

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The sandbox, with `local_net_state` `up`. | [Sandbox](#schema-sandbox) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 409 | The request conflicts with the state of the resource. The message says how. | [Error](#schema-error) |
| 413 | The body is larger than the limit (1 MiB; 64 KiB for the calls of nodes and keys). | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

#### `DELETE /v1/sandboxes/{id}/local-net/attach` <a id="detachlocalnet"></a>

**Take the tunnel down**. Acceso: `tenant`. Autenticación: clave de API o token del IdP.

Withdraws the tunnel: `local_net_state` becomes `withdrawn`. `local_net` stays true, so the sandbox's
egress stays closed instead of going back to the node's public proxy. Only the owner may call it.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the sandbox. |

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The sandbox, with `local_net_state` `withdrawn`. | [Sandbox](#schema-sandbox) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 409 | The request conflicts with the state of the resource. The message says how. | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

### Egress policy

The hosts a tenant's sandboxes may reach.

| Operación | Qué hace | Acceso |
|---|---|---|
| [`GET /v1/tenants/{id}/egress`](#gettenantegress) | A tenant's egress rules and the policy they make | `tenant` |
| [`PUT /v1/tenants/{id}/egress`](#puttenantegress) | Replace a tenant's egress rules | `tenant` |
| [`POST /v1/tenants/{id}/egress/check`](#checktenantegress) | Would a tenant's policy let a host through? | `tenant` |

#### `GET /v1/tenants/{id}/egress` <a id="gettenantegress"></a>

**A tenant's egress rules and the policy they make**. Acceso: `tenant`. Autenticación: clave de API o token del IdP.

The stored rules of a tenant and the effective policy the nodes apply: `deny-default` with those
rules when at least one is enabled, else `allow-all` only if the control plane runs with
`ASP_EGRESS_DEFAULT_ALLOW`, else `deny-default` with no rules (nothing leaves). Needs the IdP role `admin`,
or an API key of the tenant (or a platform key).

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the tenant. |

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The rules and the policy. | [EgressRules](#schema-egressrules) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

#### `PUT /v1/tenants/{id}/egress` <a id="puttenantegress"></a>

**Replace a tenant's egress rules**. Acceso: `tenant`. Autenticación: clave de API o token del IdP.

Replaces the whole rule set (not a patch). A rule allows a host (`api.example.com`, or `*.example.com` for
its subdomains and the domain itself) on a `port`; one without `port` allows the web ports 80 and 443,
and other ports need a rule that names them. `enabled` defaults to true. Nodes pick the new policy
up on their next work poll, running sandboxes included.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the tenant. |

**Cuerpo** (obligatorio): [PutEgressRequest](#schema-putegressrequest)

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The rules as stored, and the policy. | [EgressRules](#schema-egressrules) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 413 | The body is larger than the limit (1 MiB; 64 KiB for the calls of nodes and keys). | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

#### `POST /v1/tenants/{id}/egress/check` <a id="checktenantegress"></a>

**Would a tenant's policy let a host through?**. Acceso: `tenant`. Autenticación: clave de API o token del IdP.

Evaluates a host (and port) against the tenant's effective policy, with the same matching the nodes use. For testing a policy before relying on it.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the tenant. |

**Cuerpo** (obligatorio): [EgressCheckRequest](#schema-egresscheckrequest)

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The verdict and the policy it came from. | [EgressCheck](#schema-egresscheck) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 413 | The body is larger than the limit (1 MiB; 64 KiB for the calls of nodes and keys). | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

### Attestation

Signed evidence of what a node booted a sandbox from.

| Operación | Qué hace | Acceso |
|---|---|---|
| [`GET /v1/sandboxes/{id}/attestation`](#getattestation) | The boot evidence of a sandbox | `tenant` |
| [`POST /v1/attestation/verify`](#verifyattestation) | Verify boot evidence without storing it | `tenant` |

#### `GET /v1/sandboxes/{id}/attestation` <a id="getattestation"></a>

**The boot evidence of a sandbox**. Acceso: `tenant`. Autenticación: clave de API o token del IdP.

The latest signed statement a node made about what it booted the sandbox from, with whether it is
`fresh`, whether the node `measured` the kernel and image (hashed them) and, when the control plane has an
image allowlist, whether the digests are on it (`allowlisted`, `image_name`). The statement is the node's
own word, signed by it: a node the control plane trusts, not a hardware root of trust.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the sandbox. |

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The evidence. | [AttestationResult](#schema-attestationresult) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 404 | The sandbox does not exist, or it has no attestation yet. | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

#### `POST /v1/attestation/verify` <a id="verifyattestation"></a>

**Verify boot evidence without storing it**. Acceso: `tenant`. Autenticación: clave de API o token del IdP.

Checks the signature and shape of an evidence bundle and says whether it is fresh. It stores nothing and looks at no sandbox.

**Cuerpo** (obligatorio): [VerifyAttestationRequest](#schema-verifyattestationrequest)

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The verdict. `valid` false carries the reason in `error`; it is still a 200. | [VerifyAttestationResult](#schema-verifyattestationresult) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 413 | The body is larger than the limit (1 MiB; 64 KiB for the calls of nodes and keys). | [Error](#schema-error) |
| 503 | The control plane has not been set up for this (attestation, OIDC, enrollment CA). | [Error](#schema-error) |

### Nodes

The inventory of nodes and their administration, for operators.

| Operación | Qué hace | Acceso |
|---|---|---|
| [`GET /v1/nodes`](#listnodes) | List the nodes | `operator` |
| [`GET /v1/nodes/{id}/doctor`](#nodedoctor) | Run the self-checks of a node | `operator` |
| [`POST /v1/nodes/{id}/cordon`](#cordonnode) | Stop placing sandboxes on a node | `admin` |
| [`POST /v1/nodes/{id}/uncordon`](#uncordonnode) | Place sandboxes on a node again | `admin` |
| [`POST /v1/nodes/{id}/revoke`](#revokenode) | Revoke a node and its certificate | `admin` |
| [`POST /v1/nodes/{id}/rotate-cert`](#rotatenodecert) | Issue a node a new certificate | `admin` |
| [`PUT /v1/nodes/{id}/fence`](#setnodefence) | Say how to power a node off | `admin` |
| [`DELETE /v1/nodes/{id}/fence`](#clearnodefence) | Remove a node's power-off target | `admin` |
| [`POST /v1/nodes/enroll-tokens`](#createenrolltoken) | Issue a single-use enrollment token | `admin` |

#### `GET /v1/nodes` <a id="listnodes"></a>

**List the nodes**. Acceso: `operator`. Autenticación: clave de API o token del IdP.

Every registered node, by id, with what is placed on it (`allocated`), what it offers after CPU
overcommit (`allocatable`, 0 = not limited) and whether the scheduler can place a new sandbox there
(`schedulable`, and `unschedulable_reason` when not). Fence credentials are never included. The inventory is
operations data: it needs the IdP role `admin` or `operator`, or a platform key (a tenant key is 403).

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The nodes. | [NodeList](#schema-nodelist) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

#### `GET /v1/nodes/{id}/doctor` <a id="nodedoctor"></a>

**Run the self-checks of a node**. Acceso: `operator`. Autenticación: clave de API o token del IdP.

The node-agent of the node runs its checks (KVM, hypervisor, guest images, disk, nftables, clock,
this control plane) and the control plane returns its report as the agent made it. It reaches the node the way an
exec does. The checks take a while: the call is bounded at 90 seconds. Same callers as listing the nodes.
`asp node doctor <id>` calls it.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the node. |

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The report. | [DoctorReport](#schema-doctorreport) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 409 | The node is revoked. | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |
| 501 | The node-agent predates the doctor. Upgrade it. | [Error](#schema-error) |
| 502 | The node agent did not answer, or refused the control plane. The message says which. | [Error](#schema-error) |

#### `POST /v1/nodes/{id}/cordon` <a id="cordonnode"></a>

**Stop placing sandboxes on a node**. Acceso: `admin`. Autenticación: clave de API o token del IdP.

The scheduler places no new sandbox on the node; the ones running there stay. Needs the IdP role `admin` or a platform key.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the node. |

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The node, `cordoned`. | [NodeView](#schema-nodeview) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

#### `POST /v1/nodes/{id}/uncordon` <a id="uncordonnode"></a>

**Place sandboxes on a node again**. Acceso: `admin`. Autenticación: clave de API o token del IdP.

Undoes `cordon`. Needs the IdP role `admin` or a platform key.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the node. |

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The node. | [NodeView](#schema-nodeview) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

#### `POST /v1/nodes/{id}/revoke` <a id="revokenode"></a>

**Revoke a node and its certificate**. Acceso: `admin`. Autenticación: clave de API o token del IdP.

Marks the node and the fingerprint of its current client certificate revoked: its mTLS calls are refused
and the scheduler never picks it. The bootstrap token every node holds is not enough: needs the IdP role
`admin` or a platform key.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the node. |

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The node, with `revoked_at`. | [Node](#schema-node) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

#### `POST /v1/nodes/{id}/rotate-cert` <a id="rotatenodecert"></a>

**Issue a node a new certificate**. Acceso: `admin`. Autenticación: clave de API o token del IdP.

Issues a replacement client certificate for the node and revokes the fingerprint of the old one. Callable
with the node's own, still valid certificate over mTLS (how node agents renew before expiry), an IdP `admin`,
or a platform key. The bootstrap token does not work: every node holds it, so it would let one take over
another's identity. It always needs a credential, even in the open lab, because it hands out a private key.
The response carries the new private key: it is shown only here.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the node. |

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The new certificate and key. | [EnrollResponse](#schema-enrollresponse) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 409 | The node is revoked. | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |
| 503 | The control plane has not been set up for this (attestation, OIDC, enrollment CA). | [Error](#schema-error) |

#### `PUT /v1/nodes/{id}/fence` <a id="setnodefence"></a>

**Say how to power a node off**. Acceso: `admin`. Autenticación: clave de API o token del IdP.

Sets where the control plane asks for a power-off when it declares the node lost (`ASP_FENCE_PROVIDER`:
a webhook URL, a Redfish base URL or an IPMI host), with the credential for it: the secret itself, or a
reference the control plane resolves when it fences (`env:NAME`, `file:/abs/path`). Needs the IdP role `admin` or
a platform key. Neither the endpoint nor the token is ever returned by any call.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the node. |

**Cuerpo** (obligatorio): [SetFenceRequest](#schema-setfencerequest)

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 204 | The target is set. | — |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 413 | The body is larger than the limit (1 MiB; 64 KiB for the calls of nodes and keys). | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

#### `DELETE /v1/nodes/{id}/fence` <a id="clearnodefence"></a>

**Remove a node's power-off target**. Acceso: `admin`. Autenticación: clave de API o token del IdP.

Removes the fence target. Needs the IdP role `admin` or a platform key.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the node. |

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 204 | The target is removed. | — |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

#### `POST /v1/nodes/enroll-tokens` <a id="createenrolltoken"></a>

**Issue a single-use enrollment token**. Acceso: `admin`. Autenticación: clave de API o token del IdP.

Issues a token with which one node enrolls (`POST /v1/nodes/enroll`), instead of the bootstrap token every node would
share. With `node_id` it is pinned: only that node may use it, and it may re-enroll a node that already holds a certificate
(re-keying it). It lasts `ttl_seconds` (one hour by default, 7 days at most). The body is optional.
Needs the IdP role `admin` or a platform key, and always a credential, even in the open lab.

**Cuerpo** (opcional): [EnrollTokenRequest](#schema-enrolltokenrequest)

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 201 | The token. It is shown only here. | [EnrollToken](#schema-enrolltoken) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 413 | The body is larger than the limit (1 MiB; 64 KiB for the calls of nodes and keys). | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

### Node agent

The protocol between the control plane and the node agents. Not for people; listed because the API is whole.

| Operación | Qué hace | Acceso |
|---|---|---|
| [`POST /v1/internal/oidc/token`](#mintoidctoken) | Mint a token for a sandbox | `node` |
| [`POST /v1/sandboxes/{id}/local-net/node-public`](#registerlocalnetnode) | The node publishes its tunnel key | `node` |
| [`POST /v1/sandboxes/{id}/attest`](#storeattestation) | A node submits the boot evidence of a sandbox | `node` |
| [`POST /v1/sandboxes/{id}/claim`](#claimsandbox) | A node claims a sandbox placed on it | `node` |
| [`POST /v1/sandboxes/{id}/renew-lease`](#renewsandboxlease) | Renew a lease (removed) | `node` |
| [`POST /v1/sandboxes/{id}/status`](#updatesandboxstatus) | A node reports what a sandbox is doing | `node` |
| [`POST /v1/nodes/enroll`](#enrollnode) | Enroll a node | `public` |
| [`POST /v1/nodes/register`](#registernode) | A node registers | `node` |
| [`POST /v1/nodes/{id}/heartbeat`](#heartbeatnode) | A node says it is alive | `node` |
| [`GET /v1/nodes/{id}/work`](#listnodework) | What a node has to do | `node` |

#### `POST /v1/internal/oidc/token` <a id="mintoidctoken"></a>

**Mint a token for a sandbox**. Acceso: `node`. Autenticación: certificado de nodo (mTLS) o clave de API de plataforma.

A node mints a short-lived JWT for one of its sandboxes, for the services the sandbox calls.
The tenant, subject and the human owner (`user_sub`, and `act` for the delegation) come from the
store: what the node or the guest sends for them is ignored. When the sandbox has fresh boot
evidence the token carries it as `x_asp_attestation`.

**Cuerpo** (obligatorio): [OIDCTokenRequest](#schema-oidctokenrequest)

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The token and its claims. | [OIDCToken](#schema-oidctoken) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 413 | The body is larger than the limit (1 MiB; 64 KiB for the calls of nodes and keys). | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |
| 503 | The control plane has not been set up for this (attestation, OIDC, enrollment CA). | [Error](#schema-error) |

#### `POST /v1/sandboxes/{id}/local-net/node-public` <a id="registerlocalnetnode"></a>

**The node publishes its tunnel key**. Acceso: `node`. Autenticación: certificado de nodo (mTLS) o clave de API de plataforma.

The node-agent publishes the public key of the WireGuard device it made for the sandbox, and the UDP port
and the two ends of the /30 it allocated. It does not turn `local_net` on or move the tunnel state.
The node must be the one the sandbox is placed on; a guest may not call it.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the sandbox. |

**Cuerpo** (obligatorio): [LocalNetNodePublicRequest](#schema-localnetnodepublicrequest)

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The sandbox. | [Sandbox](#schema-sandbox) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 409 | The request conflicts with the state of the resource. The message says how. | [Error](#schema-error) |
| 413 | The body is larger than the limit (1 MiB; 64 KiB for the calls of nodes and keys). | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

#### `POST /v1/sandboxes/{id}/attest` <a id="storeattestation"></a>

**A node submits the boot evidence of a sandbox**. Acceso: `node`. Autenticación: certificado de nodo (mTLS) o clave de API de plataforma.

The node-agent sends the statement it signed when it booted a sandbox. The control plane verifies the
signature against a key it trusts (`ASP_ATTEST_KEY`, `ASP_ATTEST_PUB`, `ASP_ATTEST_TRUSTED_PUBS`) or, over
mTLS, the key of the node's certificate, which binds the evidence to the node's enrolled identity. A key that
only the statement carries is not trusted. It then checks the image against the allowlist if there is one,
stores the evidence and records a `sandbox.attested` event; a refused image is recorded as
`sandbox.attestation_refused`.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the sandbox. |

**Cuerpo** (obligatorio): [AttestRequest](#schema-attestrequest)

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The stored evidence. | [AttestationRecord](#schema-attestationrecord) |
| 400 | The statement is for another sandbox, its signature does not verify, or its image is not allowed. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 413 | The body is larger than the limit (1 MiB; 64 KiB for the calls of nodes and keys). | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |
| 503 | The control plane has not been set up for this (attestation, OIDC, enrollment CA). | [Error](#schema-error) |

#### `POST /v1/sandboxes/{id}/claim` <a id="claimsandbox"></a>

**A node claims a sandbox placed on it**. Acceso: `node`. Autenticación: certificado de nodo (mTLS) o clave de API de plataforma.

The node the scheduler placed a `requested` sandbox on claims it, which moves it to `starting`.
Only that node can: claiming a sandbox placed elsewhere, or one not in `requested`, is 409.
With mTLS `node_id` may be left out, and when given must be the certificate's node.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the sandbox. |

**Cuerpo** (obligatorio): [ClaimRequest](#schema-claimrequest)

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The sandbox, `starting`. | [Sandbox](#schema-sandbox) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 409 | The request conflicts with the state of the resource. The message says how. | [Error](#schema-error) |
| 413 | The body is larger than the limit (1 MiB; 64 KiB for the calls of nodes and keys). | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

#### `POST /v1/sandboxes/{id}/renew-lease` <a id="renewsandboxlease"></a>

**Renew a lease (removed)**. Acceso: `node`. Autenticación: certificado de nodo (mTLS) o clave de API de plataforma. **Obsoleta.**

Leases were replaced by the `assigned` list of `GET /v1/nodes/{id}/work`: a node stops the VMs the control
plane no longer assigns to it. The route stays for one release so that an old node-agent logs why it
fails. It always answers 410.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the sandbox. |

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 410 | Removed. Update the node agent. | [Error](#schema-error) |

#### `POST /v1/sandboxes/{id}/status` <a id="updatesandboxstatus"></a>

**A node reports what a sandbox is doing**. Acceso: `node`. Autenticación: certificado de nodo (mTLS) o clave de API de plataforma.

The node reports the state it observed (`starting`, `running`, `stopped`, `failed`, `deleted`…) and,
for a failure, why (`detail`). A report that arrives late does not bring a sandbox back: `stopped` is
final, `failed` only goes to `stopped`, `stopping` only finishes, and `deleted` is accepted only for a
sandbox being deleted. Those are 409. A guest may not call it.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the sandbox. |

**Cuerpo** (obligatorio): [StatusRequest](#schema-statusrequest)

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The sandbox as updated. | [Sandbox](#schema-sandbox) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 409 | The request conflicts with the state of the resource. The message says how. | [Error](#schema-error) |
| 413 | The body is larger than the limit (1 MiB; 64 KiB for the calls of nodes and keys). | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

#### `POST /v1/nodes/enroll` <a id="enrollnode"></a>

**Enroll a node**. Acceso: `public`. Autenticación: token de bootstrap o de enroll.

A node-agent presents the bootstrap token (`ASP_NODE_BOOTSTRAP_TOKEN`) or an enrollment token, as
`Authorization: Bearer` or `X-ASP-Bootstrap-Token`, and gets a client certificate for mutual TLS, with its private key
and the CA that signed it. The bootstrap token enrolls only an id that has no certificate yet, or a revoked node;
re-enrolling a live node needs a token pinned to it (409 otherwise). The node is registered in the same call; it takes
no sandboxes until it registers with its capacity. With `ASP_MTLS_STRICT=1` the main listener demands
client certificates, so this route (and `/healthz`) is also served on the plaintext listener of `ASP_ENROLL_LISTEN`.

**Cuerpo** (obligatorio): [EnrollNodeRequest](#schema-enrollnoderequest)

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 201 | The certificate, its key and the CA. | [EnrollResponse](#schema-enrollresponse) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The enrollment token is pinned to another node. | [Error](#schema-error) |
| 409 | The node is enrolled and not revoked, and the token is not pinned to it. | [Error](#schema-error) |
| 413 | The body is larger than the limit (1 MiB; 64 KiB for the calls of nodes and keys). | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |
| 503 | The control plane has not been set up for this (attestation, OIDC, enrollment CA). | [Error](#schema-error) |

#### `POST /v1/nodes/register` <a id="registernode"></a>

**A node registers**. Acceso: `node`. Autenticación: certificado de nodo (mTLS) o clave de API de plataforma.

The node-agent announces itself and what it offers: its endpoints, hypervisor profiles, capacity, whether it takes work and
enforces egress, and what it runs (version, guest kernel and image digests). It repeats this on every start. It makes
the node `ready`; it does not undo a cordon and keeps the node's fence target. A revoked node is 409. A new
`agent_instance_id` means the agent restarted: the sandboxes it had running and does not list in `adopted_sandboxes`
are failed as orphaned. With mTLS a node registers only itself. `agent_endpoint` must be `https://`, or `http://` on
loopback (on another host only with `ASP_INSECURE_AGENT_HTTP=1`, for a lab).

**Cuerpo** (obligatorio): [RegisterNodeRequest](#schema-registernoderequest)

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The node as stored. | [Node](#schema-node) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 409 | The node is revoked. | [Error](#schema-error) |
| 413 | The body is larger than the limit (1 MiB; 64 KiB for the calls of nodes and keys). | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

#### `POST /v1/nodes/{id}/heartbeat` <a id="heartbeatnode"></a>

**A node says it is alive**. Acceso: `node`. Autenticación: certificado de nodo (mTLS) o clave de API de plataforma.

Refreshes `last_seen_at`; every 30 seconds or so. The body is optional and may carry the free space of the node's disk
directory. A node that has not been heard from for `ASP_NODE_STALE_AFTER` is not schedulable, and after
`ASP_NODE_FAILOVER_AFTER` its sandboxes are failed (ADR-0011). A revoked node is 409; an unknown one is 404, which is
the agent's cue to register again.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the node. |

**Cuerpo** (opcional): [HeartbeatRequest](#schema-heartbeatrequest)

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The node. | [Node](#schema-node) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 409 | The node is revoked. | [Error](#schema-error) |
| 413 | The body is larger than the limit (1 MiB; 64 KiB for the calls of nodes and keys). | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

#### `GET /v1/nodes/{id}/work` <a id="listnodework"></a>

**What a node has to do**. Acceso: `node`. Autenticación: certificado de nodo (mTLS) o clave de API de plataforma.

The poll of a node-agent (every 2 seconds or so; it also counts as a sign of life). `sandboxes` are the ones placed
on the node that need its action: claim, start, stop, delete, local-net. `assigned` lists every sandbox the node must keep
running: the node stops any VM it runs that is not listed (it was failed over, or destroyed). `retained` lists the
stopped sandboxes whose disks the node keeps. `egress` carries the egress policy of each tenant that has a sandbox
on the node, with a `version` that changes when the decision can: a node reapplies a policy only when it moves.
It is absent when the rules could not be read: the node keeps the ones it has. An unknown node is 404.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the node. |

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The work. | [NodeWork](#schema-nodework) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

### API keys

Create, list, revoke and rotate API keys.

| Operación | Qué hace | Acceso |
|---|---|---|
| [`POST /v1/api-keys`](#createapikey) | Create an API key | `admin` |
| [`GET /v1/api-keys`](#listapikeys) | List API keys | `admin` |
| [`DELETE /v1/api-keys/{id}`](#revokeapikey) | Revoke an API key | `admin` |
| [`POST /v1/api-keys/{id}/rotate`](#rotateapikey) | Give an API key a new secret | `admin` |

#### `POST /v1/api-keys` <a id="createapikey"></a>

**Create an API key**. Acceso: `admin`. Autenticación: clave de API o token del IdP.

Makes a key and returns its `secret` **once**: it is stored only as a hash. A platform key may make a key for any
tenant and either scope; an IdP `admin` may make `tenant` keys for their own tenant only (403 otherwise). A tenant
key may not manage keys.
`tenant_id` defaults to the caller's tenant, or `default`. `name` is unique within the tenant (409).
`ttl` is a Go duration (`720h`) or days (`30d`), at most 5 years; empty means the key does not expire.

**Cuerpo** (obligatorio): [CreateApiKeyRequest](#schema-createapikeyrequest)

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 201 | The key and its secret. Cabecera `Cache-Control`: Always `no-store`. | [ApiKeyWithSecret](#schema-apikeywithsecret) |
| 400 | The body is not valid JSON, a field is missing or out of range, or a parameter is invalid. The message says which. | [Error](#schema-error) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 409 | The tenant already has a key with that name. | [Error](#schema-error) |
| 413 | The body is larger than the limit (1 MiB; 64 KiB for the calls of nodes and keys). | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

#### `GET /v1/api-keys` <a id="listapikeys"></a>

**List API keys**. Acceso: `admin`. Autenticación: clave de API o token del IdP.

The keys, without secrets, revoked ones included. An IdP `admin` confined to a tenant sees their tenant's; a platform key sees all, or those of `tenant_id`.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `tenant_id` | consulta | string | Only this tenant's. A caller confined to a tenant gets its own whatever it asks, and naming another tenant is 403. |

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The keys. | [ApiKeyList](#schema-apikeylist) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

#### `DELETE /v1/api-keys/{id}` <a id="revokeapikey"></a>

**Revoke an API key**. Acceso: `admin`. Autenticación: clave de API o token del IdP.

The key stops authenticating at once. The row stays, marked revoked. Another tenant's key is 404 to a confined admin.

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the API key (not its secret or its prefix). |

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The key, with `revoked_at`. | [ApiKey](#schema-apikey) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

#### `POST /v1/api-keys/{id}/rotate` <a id="rotateapikey"></a>

**Give an API key a new secret**. Acceso: `admin`. Autenticación: clave de API o token del IdP.

The key keeps its id and settings and gets a new `secret`, returned once. The old secret stops working at once. A revoked key cannot be rotated (409).

**Parámetros**

| Nombre | En | Tipo | Descripción |
|---|---|---|---|
| `id` | ruta (obligatorio) | string | The id of the API key (not its secret or its prefix). |

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The key and its new secret. Cabecera `Cache-Control`: Always `no-store`. | [ApiKeyWithSecret](#schema-apikeywithsecret) |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |
| 404 | It does not exist, or it belongs to another tenant than the caller's. | [Error](#schema-error) |
| 409 | The key is revoked, or no free key prefix was found (retry). | [Error](#schema-error) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |

### Identity

The OIDC issuer of the control plane, whose tokens a sandbox presents to the services it calls.

| Operación | Qué hace | Acceso |
|---|---|---|
| [`GET /.well-known/openid-configuration`](#getopenidconfiguration) | OIDC discovery document | `public` |
| [`GET /oidc/jwks.json`](#getjwks) | Public keys of the issuer | `public` |

#### `GET /.well-known/openid-configuration` <a id="getopenidconfiguration"></a>

**OIDC discovery document**. Acceso: `public`. Autenticación: ninguna.

The discovery document of the control plane as an OIDC issuer.

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The discovery document. | [OpenIDConfiguration](#schema-openidconfiguration) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |
| 503 | The control plane has not been set up for this (attestation, OIDC, enrollment CA). | [Error](#schema-error) |

#### `GET /oidc/jwks.json` <a id="getjwks"></a>

**Public keys of the issuer**. Acceso: `public`. Autenticación: ninguna.

The keys that verify the tokens `POST /v1/internal/oidc/token` mints, the current one and the one before a rotation.

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The key set. | [JWKS](#schema-jwks) |
| 500 | The control plane failed to do it, usually its store. The message is the error. | [Error](#schema-error) |
| 503 | The control plane has not been set up for this (attestation, OIDC, enrollment CA). | [Error](#schema-error) |

### Operations

Health, metrics and this document.

| Operación | Qué hace | Acceso |
|---|---|---|
| [`GET /healthz`](#healthz) | Liveness | `public` |
| [`GET /openapi.json`](#getopenapi) | This document | `public` |
| [`GET /metrics`](#getmetrics) | Prometheus metrics | `operator` |

#### `GET /healthz` <a id="healthz"></a>

**Liveness**. Acceso: `public`. Autenticación: ninguna.

Answers 200 while the process serves requests. It does not look at the store or the nodes.

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The control plane is up. | [Health](#schema-health) |

#### `GET /openapi.json` <a id="getopenapi"></a>

**This document**. Acceso: `public`. Autenticación: ninguna.

The OpenAPI document of the API, as JSON. `info.version` is the version of the control plane that serves it.

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The OpenAPI 3.0 document. | objeto libre |

#### `GET /metrics` <a id="getmetrics"></a>

**Prometheus metrics**. Acceso: `operator`. Autenticación: clave de API o token del IdP.

The metrics of the control plane in the Prometheus text format: requests, sandbox creates and
their results, sandboxes by state, the nodes, the idle reaper and the node monitor
(`docs/how-to/monitoring.md` lists them). Needs an IdP `admin` or `operator`, or a platform
key: they span every tenant. The same metrics are served without credentials on the separate
listener of `ASP_METRICS_LISTEN` (loopback unless told otherwise).

**Respuestas**

| Estado | Significado | Cuerpo |
|---|---|---|
| 200 | The metrics. | `text/plain`: string |
| 401 | No credential, an unknown, revoked or expired one, or an IdP token that is not valid or names no tenant. | [Error](#schema-error) |
| 403 | The credential is valid but its role, its tenant or its node identity does not allow this. | [Error](#schema-error) |

## Esquemas

Los cuerpos JSON que las operaciones reciben y devuelven. «Obligatorio» en una respuesta quiere decir que el plano de control siempre lo escribe; en una petición, que lo exige.

### Error <a id="schema-error"></a>

The body of every error.

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `error` | string | sí | What went wrong, for people. It may change between releases. |

### PlacementError <a id="schema-placementerror"></a>

The 503 of a placement that found no room.

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `error` | string | sí | A sentence with the reasons, for example `no node can take the sandbox (2 nodes: insufficient_memory 1, cordoned 1)`. |
| `reasons` | objeto (nombre → integer) | no | For each reason a node was skipped, how many nodes. The reasons are `unknown_node`, `no_agent_endpoint`, `revoked`, `offline`, `stale`, `not_accepting_work`, `cordoned`, `vmm_profile`, `insufficient_cpu`, `insufficient_memory` and `max_sandboxes`. |

### Health <a id="schema-health"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `status` | string: `ok` | sí | Always `ok`. |

### OpenIDConfiguration <a id="schema-openidconfiguration"></a>

The OIDC discovery document of the control plane as an issuer.

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `issuer` | string | sí | The issuer: the address of this control plane. |
| `jwks_uri` | string | sí | Where the public keys are. |
| `token_endpoint` | string | sí | Where nodes mint tokens. It needs a node credential, so it is not for people. |
| `id_token_signing_alg_values_supported` | array de string | sí | `RS256`. |
| `response_types_supported` | array de string | sí | `id_token`. |
| `subject_types_supported` | array de string | sí | `public`. |

### JWKS <a id="schema-jwks"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `keys` | array de [JWK](#schema-jwk) | sí | The current key, and the one before the last rotation for as long as tokens signed with it can still be around. |

### JWK <a id="schema-jwk"></a>

An RSA public key, RFC 7517.

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `kty` | string: `RSA` | sí | The key type, `RSA`. |
| `use` | string: `sig` | sí | `sig`: the key verifies signatures. |
| `alg` | string: `RS256` | sí | `RS256`. |
| `kid` | string | sí | The key id, as in the `kid` header of the tokens it verifies. |
| `n` | string | sí | The modulus, base64url. |
| `e` | string | sí | The exponent, base64url. |

### OIDCTokenRequest <a id="schema-oidctokenrequest"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `sandbox_id` | string | sí | The sandbox to mint the token for. It must be placed on the calling node. |
| `aud` | string | sí | The audience of the token: the service that will verify it. |
| `nonce` | string | no | Put into the token as its `nonce` claim. |
| `user_sub` | string | no | Ignored. The human owner comes from the sandbox. |
| `act` | cualquiera | no | Ignored. The delegation comes from the sandbox. |

### OIDCToken <a id="schema-oidctoken"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `access_token` | string | sí | The JWT, signed RS256 with a key in the JWKS. |
| `token_type` | string: `Bearer` | sí | Always `Bearer`. |
| `expires_in` | integer (int64) | sí | Seconds until it expires. |
| `claims` | objeto libre | sí | The claims of the token, for convenience: `sub`, `iss`, `aud`, `tenant_id`, `sandbox_id`, and when they apply `user_sub` (the human owner), `act` (the delegation) and `x_asp_attestation` (what the node measured at boot). |

### SandboxState <a id="schema-sandboxstate"></a>

Where a sandbox is in its life. See the states in the description of this API.

Valores: `requested` \| `scheduled` \| `starting` \| `running` \| `paused` \| `stopping` \| `stopped` \| `failed` \| `deleting` \| `deleted`.

### Sandbox <a id="schema-sandbox"></a>

A sandbox, as the control plane and its node last knew it.

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `id` | string | sí | The id of the sandbox. |
| `tenant_id` | string | sí | The tenant that owns it. |
| `node_id` | string, puede ser null | sí | The node the sandbox is placed on; null if it has none. |
| `state` | [SandboxState](#schema-sandboxstate) | sí | Where a sandbox is in its life. See the states in the description of this API. |
| `vmm_profile` | string | sí | The hypervisor profile; `cloud-hypervisor` unless asked otherwise. |
| `image_ref` | string | sí | The image the caller asked for, recorded with the sandbox. |
| `cpu_millis` | integer | sí | CPU in thousandths of a core. |
| `memory_mib` | integer | sí | Memory in MiB. |
| `state_version` | integer (int64) | sí | Goes up with every change of state. |
| `owner_sub` | string | no | The IdP subject of the creator. Empty for a sandbox made with an API key. |
| `owner_email` | string | no | The creator's e-mail, from the IdP token. For showing; it decides nothing. |
| `last_activity_at` | string (date-time) | sí | The last command, or the last time the sandbox started. The idle reaper counts from here. |
| `stop_reason` | string | no | Why the sandbox was stopped or failed, when the user did not ask: `idle_timeout`, `node_lost`, `node_agent_restarted`, `vmm_exited`, `unscheduled`, `retention_expired` or `tenant_cap`. |
| `status_detail` | string | no | Why the node last reported a failure, or why a resume went back to stopped. |
| `boot_count` | integer | sí | How many times the sandbox has started: 1 at the first boot, one more per resume. |
| `booted_at` | string (date-time) | no | When the node first reported it running. Until then there is no disk worth keeping. Never cleared. |
| `stopped_at` | string (date-time) | no | When the node reported it stopped; absent while it is not. The retention period counts from it. |
| `workspace_host_path` | string | no | The host directory shared into the guest at `/workspace`, if any. |
| `local_net` | boolean | sí | The sandbox routes its traffic through the caller's local-net tunnel and never through the node's public proxy. |
| `local_net_state` | string: `off` \| `pending` \| `up` \| `withdrawn` | sí | Where the tunnel is: `off` (none; egress goes through the node's proxy), `pending` (asked for, not up), `up`, or `withdrawn` (taken down; egress stays closed). |
| `local_net_attached_at` | string (date-time) | no | When the tunnel last came up. |
| `local_net_grant_expires_at` | string (date-time) | no | When the current grant expires. The grant itself is not stored. |
| `local_net_client_public` | string | no | The WireGuard public key of the caller's end. |
| `local_net_node_public` | string | no | The WireGuard public key of the node's end. |
| `local_net_listen_port` | integer | no | The node's UDP port for the tunnel; 0 until the node publishes it. |
| `local_net_node_addr` | string | no | The node's end of the tunnel's /30. |
| `local_net_client_addr` | string | no | The caller's end of the tunnel's /30. |
| `created_at` | string (date-time) | sí | When the sandbox was created. |
| `updated_at` | string (date-time) | sí | When it last changed. |

### CreateSandboxRequest <a id="schema-createsandboxrequest"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `tenant_id` | string | no | The tenant. Defaults to the caller's, or to `ASP_DEFAULT_TENANT` (`default`). |
| `image_ref` | string | sí | The image to run, recorded with the sandbox (`debian:bookworm-slim`). A node boots the base image it is configured with; the reference does not select one today. |
| `cpu_millis` | integer, mínimo 1 | sí | CPU in thousandths of a core (`1000` is one core). |
| `memory_mib` | integer, mínimo 64 | sí | Memory in MiB, at least 64. |
| `vmm_profile` | string | no | The hypervisor profile. Defaults to `cloud-hypervisor`. Only a node that lists it is a candidate. |
| `node_id` | string | no | Place the sandbox on this node. A node that cannot take it is 409, not another node. |
| `owner_sub` | string | no | Only in the open lab. With an IdP token the owner is its subject; naming another is 403. |
| `owner_email` | string | no | Only in the open lab. With an IdP token it comes from the token. |
| `actor_sub` | string | no | Only in the open lab. Who the audit trail says created it. |
| `workspace_host_path` | string | no | An absolute host directory to share into the guest at `/workspace`. It must be inside `<root>/<tenant>` of an `ASP_WORKSPACE_ROOTS` root. |
| `local_net` | boolean | no | Opt into the local-net tunnel. Off by default. |

### SandboxList <a id="schema-sandboxlist"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `sandboxes` | array de [Sandbox](#schema-sandbox) | sí | The sandboxes, most recent first. |

### SandboxEvent <a id="schema-sandboxevent"></a>

One row of a sandbox's append-only audit trail.

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `id` | integer (int64) | sí | Goes up with every event. |
| `sandbox_id` | string | sí | The sandbox it is about. |
| `tenant_id` | string | sí | Its tenant. |
| `event_type` | string | sí | What happened: `sandbox.created`, `sandbox.placed`, `sandbox.claimed`, `sandbox.state_changed`, `sandbox.resumed`, `sandbox.exec`, `sandbox.idle_reaped`, `sandbox.node_lost`, `sandbox.agent_restarted`, `sandbox.unscheduled`, `sandbox.deleted`, `sandbox.attested`, `sandbox.attestation_refused`, `sandbox.local_net_requested`, `sandbox.local_net_up` or `sandbox.local_net_withdrawn`. New types may appear. |
| `from_state` | string | no | The state before, for a change of state. |
| `to_state` | string | no | The state after. |
| `actor` | string | sí | The component that did it (`api`, `node-agent`, `local-agent`, `idle-reaper`…). |
| `actor_sub` | string | no | The person or service on whose behalf: the IdP subject, `apikey:<prefix>`, or `node:<id>`. |
| `request_id` | string | no | The request that caused it, when there is one. |
| `payload` | objeto libre | sí | Details of the event; they depend on its type. |
| `created_at` | string (date-time) | sí | When it happened. |

### SandboxEventList <a id="schema-sandboxeventlist"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `events` | array de [SandboxEvent](#schema-sandboxevent) | sí | The events, oldest first. |

### ExecRequest <a id="schema-execrequest"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `cmd` | array de string | sí | The command and its arguments. Not run through a shell: use `["sh", "-c", "…"]` for that. |
| `env` | objeto (nombre → string) | no | Variables to add to the command's environment. |
| `cwd` | string | no | The working directory in the guest. |
| `actor_sub` | string | no | Only in the open lab. See the `X-ASP-Actor-Sub` header. |
| `pty` | boolean | no | Run the command under a pseudoterminal. Use it with a stream: the first event is `ready`. |
| `rows` | integer | no | The initial height of the terminal. 0 lets the guest pick 24. |
| `cols` | integer | no | The initial width of the terminal. 0 lets the guest pick 80. |
| `stdin` | string | no | Input for a buffered command, given up front. |
| `stdin_stream` | boolean | no | Keep a pipe to the command open, to feed with `…/exec/stdin`. A stream answers `ready` first. |
| `as_root` | boolean | no | Run as root. By default the command runs as the owner of the workspace, or as the guest's default user. |
| `timeout_seconds` | integer, mínimo 0 | no | How long a buffered command may run, at most `ASP_BUFFERED_EXEC_TIMEOUT`; 0 uses that limit. A stream has none. |

### ExecResponse <a id="schema-execresponse"></a>

The result of a buffered command.

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `stdout` | string | sí | What the command wrote to its standard output. Invalid UTF-8 is replaced. |
| `stderr` | string | sí | What it wrote to its standard error. |
| `exit_code` | integer | sí | The command's exit status. 124 when the guest cut it off for time. |

### ExecStreamEvent <a id="schema-execstreamevent"></a>

One line of a streamed command. `ready` comes first when the command was started with `pty` or `stdin_stream`, and
carries `exec_id`. `stdout` and `stderr` carry `data`, a chunk of the output (invalid UTF-8 is replaced). `exit`
comes last and carries `exit_code`.

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `type` | string: `ready` \| `stdout` \| `stderr` \| `exit` | sí | What the line is. |
| `exec_id` | string | no | With `ready`. Pass it to `…/exec/stdin`. |
| `data` | string | no | With `stdout` and `stderr`. |
| `exit_code` | integer | no | With `exit`. |

### ExecStdinRequest <a id="schema-execstdinrequest"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `exec_id` | string | sí | The `exec_id` of the `ready` event. |
| `data` | string | no | Bytes to write to the command's input. |
| `close` | boolean | no | Close the command's input (end of file). |
| `rows` | integer | no | A new terminal height, for a command with `pty`. |
| `cols` | integer | no | A new terminal width, for a command with `pty`. |

### ExecStdinAck <a id="schema-execstdinack"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `ok` | boolean | sí | Always true. |

### LocalNetGrant <a id="schema-localnetgrant"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `grant` | string | sí | The one-time secret that brings the tunnel up. Shown only here; the control plane keeps a hash. |
| `dial` | string | sí | `host[:port]` where the caller reaches the node, from the node's `local_net_dial` or `ASP_LOCAL_NET_DIAL`. |
| `expires_at` | string (date-time) | sí | When the grant stops being valid: 10 minutes after it is issued. |
| `tunnel_iface` | string | sí | The name of the WireGuard interface. |
| `transport` | string: `wireguard` | sí | Always `wireguard`. |
| `node_public_key` | string | sí | Empty until the node has published its key. |
| `listen_port` | integer | sí | The node's UDP port for this tunnel; 0 until the node publishes it. |
| `node_tunnel_addr` | string | sí | The node's end of the tunnel's /30; empty until the node publishes it. |
| `client_tunnel_addr` | string | sí | The caller's end of the /30. |

### LocalNetHeartbeatRequest <a id="schema-localnetheartbeatrequest"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `grant` | string | sí | The grant from `…/local-net/grant`. |
| `client_public_key` | string | sí | The caller's WireGuard public key: 32 bytes, standard base64. |

### LocalNetNodePublicRequest <a id="schema-localnetnodepublicrequest"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `public_key` | string | sí | The public key of the WireGuard device the node made for the sandbox. |
| `listen_port` | integer, mínimo 1, máximo 65535 | no | The UDP port the node's device listens on. |
| `node_tunnel_addr` | string | no | The node's end of the /30, inside 10.188.0.0/16. |
| `client_tunnel_addr` | string | no | The other host of that /30. |

### EgressRule <a id="schema-egressrule"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `id` | string | no | The id of the rule. |
| `tenant_id` | string | no | The tenant it belongs to. |
| `host_pattern` | string | sí | A host name (`api.example.com`) or `*.example.com`, which matches its subdomains and the domain itself. Lower-cased when stored. |
| `port` | integer, mínimo 1, máximo 65535 | no | The port it allows. Without one, 80 and 443. |
| `enabled` | boolean | sí | A disabled rule is kept and does not count. |

### EgressRuleInput <a id="schema-egressruleinput"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `host_pattern` | string | sí | A host name, or `*.` and a domain. |
| `port` | integer, mínimo 1, máximo 65535 | no | The port it allows. Without one, 80 and 443. |
| `enabled` | boolean | no | Defaults to true. |

### PutEgressRequest <a id="schema-putegressrequest"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `rules` | array de [EgressRuleInput](#schema-egressruleinput) | sí | The whole set of rules. An empty list removes them all. |

### EgressPolicy <a id="schema-egresspolicy"></a>

What a tenant's sandboxes may reach.

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `tenant_id` | string | sí | The tenant. |
| `mode` | string: `allow-all` \| `deny-default` | sí | `deny-default` lets through only what a rule allows. |
| `rules` | array de [EgressRule](#schema-egressrule) | sí | The enabled rules. |

### EgressRules <a id="schema-egressrules"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `tenant_id` | string | sí | The tenant. |
| `rules` | array de [EgressRule](#schema-egressrule) | sí | The stored rules, enabled or not. |
| `policy` | [EgressPolicy](#schema-egresspolicy) | sí | What a tenant's sandboxes may reach. |

### EgressCheckRequest <a id="schema-egresscheckrequest"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `host` | string | sí | A host name. A URL is accepted and reduced to its host. |
| `port` | integer | no | The port. 0 or absent means the caller does not know it, as for a DNS lookup. |

### EgressCheck <a id="schema-egresscheck"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `tenant_id` | string | sí | The tenant. |
| `host` | string | sí | The host as it was asked. |
| `port` | integer | sí | The port as it was asked; 0 if none. |
| `allowed` | boolean | sí | Whether the policy lets it through. |
| `policy` | [EgressPolicy](#schema-egresspolicy) | sí | What a tenant's sandboxes may reach. |

### BootStatement <a id="schema-bootstatement"></a>

What a node says it booted a sandbox from.

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `sandbox_id` | string | sí | The sandbox the VM was booted for. |
| `image_digest` | string | sí | `sha256:<hex>` of the base image the sandbox's disk was copied from, as the node hashed it. A node that does not measure sends the image reference here. |
| `vmm_profile` | string | sí | The hypervisor profile it ran under. |
| `cid` | integer, mínimo 0 | sí | The vsock context id of the guest. |
| `node_id` | string | sí | The node that booted it. |
| `ts` | string (date-time) | sí | When the node made the statement. Old statements are not fresh. |
| `kernel_digest` | string | no | `sha256:<hex>` of the kernel the VM loaded. |
| `vmm_version` | string | no | The version the hypervisor reported. |
| `boot` | string: `new` \| `resume` | no | A `resume` boots a retained disk that has diverged from the base image `image_digest` names. |

### Evidence <a id="schema-evidence"></a>

A signed boot statement.

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `statement` | [BootStatement](#schema-bootstatement) | sí | What a node says it booted a sandbox from. |
| `signature` | string | sí | base64url of the DER ECDSA signature. |
| `alg` | string: `ES256` | sí | The signature algorithm. |
| `key_id` | string | no | The id of the key that signed, when the node has one. |
| `public_key_pem` | string | no | The public key, when the node sends it along. |
| `received_at` | string (date-time) | no | When the control plane stored it. |

### AttestRequest <a id="schema-attestrequest"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `statement` | [BootStatement](#schema-bootstatement) | sí | What a node says it booted a sandbox from. |
| `signature` | string | sí | base64url of the DER ECDSA signature of the statement. |
| `alg` | string: `ES256` | no | Defaults to `ES256`. |
| `key_id` | string | no | The id of the signing key. |
| `public_key_pem` | string | no | The signing key, PEM. |

### AttestationRecord <a id="schema-attestationrecord"></a>

The latest verified evidence kept for a sandbox.

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `sandbox_id` | string | sí | The sandbox. |
| `node_id` | string | sí | The node that booted it. |
| `image_digest` | string | sí | `sha256:<hex>` of the base image, or the image reference for a node that does not measure. |
| `vmm_profile` | string | sí | The hypervisor profile. |
| `cid` | integer, mínimo 0 | sí | The vsock context id of the guest. |
| `statement_ts` | string (date-time) | sí | When the node made the statement. |
| `alg` | string | sí | The signature algorithm. |
| `key_id` | string | no | The id of the signing key. |
| `signature` | string | sí | base64url of the DER ECDSA signature. |
| `bundle` | [Evidence](#schema-evidence) | sí | A signed boot statement. |
| `received_at` | string (date-time) | sí | When the control plane stored it. |

### AttestationResult <a id="schema-attestationresult"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `attestation` | [AttestationRecord](#schema-attestationrecord) | sí | The latest verified evidence kept for a sandbox. |
| `fresh` | boolean | sí | The statement is recent enough to be trusted for a token claim. |
| `measured` | boolean | sí | The statement carries real digests of the kernel and the base image. |
| `allowlisted` | boolean | no | Only when the control plane has an image allowlist: the digests are on it. |
| `image_name` | string | no | The allowlist's name for the image, when `allowlisted`. |

### VerifyAttestationRequest <a id="schema-verifyattestationrequest"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `evidence` | [Evidence](#schema-evidence) | sí | A signed boot statement. |

### VerifyAttestationResult <a id="schema-verifyattestationresult"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `valid` | boolean | sí | Whether the signature and the statement hold. |
| `error` | string | sí | Why it is not valid; empty when it is. |
| `fresh` | boolean | sí | Whether the statement is recent enough. |
| `kid` | string | sí | The key id of the evidence. |
| `alg` | string | sí | Its algorithm. |

### ClaimRequest <a id="schema-claimrequest"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `node_id` | string | no | The claiming node. With mTLS it may be left out and is the certificate's node. |

### StatusRequest <a id="schema-statusrequest"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `state` | [SandboxState](#schema-sandboxstate) | sí | Where a sandbox is in its life. See the states in the description of this API. |
| `detail` | string | no | Why, for a failure. |

### NodeUsage <a id="schema-nodeusage"></a>

What is placed on a node, or what it offers.

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `cpu_millis` | integer (int64) | sí | CPU in thousandths of a core. |
| `memory_mib` | integer (int64) | sí | Memory in MiB. |
| `sandboxes` | integer (int64) | sí | How many sandboxes. |

### Node <a id="schema-node"></a>

A node as the control plane stores it. The fence target is never part of it.

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `id` | string | sí | The node id: the CN of its certificate. |
| `name` | string | sí | A name for people; the id if none was given. |
| `endpoint` | string | sí | The node's own address, as it registered. |
| `agent_endpoint` | string | no | Where the control plane calls the node agent. `https://` over mutual TLS, or `http://` on loopback. |
| `state` | string | sí | `ready` once registered or enrolled; `offline` when it has been revoked or the control plane declared it lost (a heartbeat brings it back). |
| `vmm_profiles` | array de string | sí | The hypervisor profiles the node can run. |
| `capacity_cpu` | integer | sí | Cores the node offers; 0 is not limited. |
| `capacity_mem_mib` | integer | sí | Memory the node offers; 0 is not limited. |
| `max_sandboxes` | integer | sí | At most this many sandboxes; 0 is not limited. |
| `cordoned` | boolean | sí | An admin stopped new placements. Running sandboxes stay. |
| `accepts_work` | boolean | sí | False for an agent that runs without `--reconcile`, and for a node that enrolled but has not registered. |
| `local_net_dial` | string | no | `host[:port]` a laptop dials for this node's local-net tunnels. |
| `egress_enforced` | boolean | sí | The node forces its guests through its egress proxy, so a tenant's policy binds them. |
| `agent_instance_id` | string | no | Changes when the node agent process restarts. |
| `agent_version` | string | no | The build of the node-agent, as it said when it registered. |
| `guest_kernel_digest` | string | no | `sha256:<hex>` of the guest kernel the node boots, as it hashed it. |
| `guest_image_digest` | string | no | `sha256:<hex>` of the base image the node boots. |
| `cert_fingerprint` | string | no | SHA-256 of the node's current client certificate. |
| `cert_serial` | string | no | The serial number of the current certificate. |
| `cert_not_after` | string (date-time) | no | When the node certificate expires. |
| `disk_free_mib` | integer (int64) | no | Free space of the node's disk directory at its last heartbeat. |
| `enrolled_at` | string (date-time) | no | When the node enrolled. |
| `revoked_at` | string (date-time) | no | When it was revoked; absent otherwise. |
| `last_seen_at` | string (date-time), puede ser null | sí | Its last heartbeat or work poll; null if it has had none. |
| `created_at` | string (date-time) | sí | When the node first appeared. |
| `updated_at` | string (date-time) | sí | When it last changed. |

### NodeView <a id="schema-nodeview"></a>

A node as operators see it, with what is placed on it and whether the scheduler can use it.

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `id` | string | sí | The node id: the CN of its certificate. |
| `name` | string | sí | A name for people; the id if none was given. |
| `endpoint` | string | sí | The node's own address, as it registered. |
| `agent_endpoint` | string | no | Where the control plane calls the node agent. `https://` over mutual TLS, or `http://` on loopback. |
| `state` | string | sí | `ready` once registered or enrolled; `offline` when it has been revoked or the control plane declared it lost (a heartbeat brings it back). |
| `vmm_profiles` | array de string | sí | The hypervisor profiles the node can run. |
| `capacity_cpu` | integer | sí | Cores the node offers; 0 is not limited. |
| `capacity_mem_mib` | integer | sí | Memory the node offers; 0 is not limited. |
| `max_sandboxes` | integer | sí | At most this many sandboxes; 0 is not limited. |
| `cordoned` | boolean | sí | An admin stopped new placements. Running sandboxes stay. |
| `accepts_work` | boolean | sí | False for an agent that runs without `--reconcile`, and for a node that enrolled but has not registered. |
| `local_net_dial` | string | no | `host[:port]` a laptop dials for this node's local-net tunnels. |
| `egress_enforced` | boolean | sí | The node forces its guests through its egress proxy, so a tenant's policy binds them. |
| `agent_instance_id` | string | no | Changes when the node agent process restarts. |
| `agent_version` | string | no | The build of the node-agent, as it said when it registered. |
| `guest_kernel_digest` | string | no | `sha256:<hex>` of the guest kernel the node boots, as it hashed it. |
| `guest_image_digest` | string | no | `sha256:<hex>` of the base image the node boots. |
| `cert_fingerprint` | string | no | SHA-256 of the node's current client certificate. |
| `cert_serial` | string | no | The serial number of the current certificate. |
| `cert_not_after` | string (date-time) | no | When the node certificate expires. |
| `disk_free_mib` | integer (int64) | no | Free space of the node's disk directory at its last heartbeat. |
| `enrolled_at` | string (date-time) | no | When the node enrolled. |
| `revoked_at` | string (date-time) | no | When it was revoked; absent otherwise. |
| `last_seen_at` | string (date-time), puede ser null | sí | Its last heartbeat or work poll; null if it has had none. |
| `created_at` | string (date-time) | sí | When the node first appeared. |
| `updated_at` | string (date-time) | sí | When it last changed. |
| `allocated` | [NodeUsage](#schema-nodeusage) | sí | What is placed on a node, or what it offers. |
| `allocatable` | [NodeUsage](#schema-nodeusage) | sí | What is placed on a node, or what it offers. |
| `vm_overhead_mib` | integer (int64) | sí | Memory counted for each sandbox on top of its own, for the hypervisor. |
| `stopped_sandboxes` | integer (int64) | sí | Stopped sandboxes on the node. Each keeps a disk there and holds no CPU or memory. |
| `fence_configured` | boolean | sí | The control plane can power the node off if it declares it lost. |
| `schedulable` | boolean | sí | The scheduler can place a new sandbox on the node. |
| `unschedulable_reason` | string: `unknown_node` \| `no_agent_endpoint` \| `revoked` \| `offline` \| `stale` \| `not_accepting_work` \| `cordoned` \| `vmm_profile` \| `insufficient_cpu` \| `insufficient_memory` \| `max_sandboxes` | no | Why the scheduler skips the node; absent when `schedulable`. |

### NodeList <a id="schema-nodelist"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `nodes` | array de [NodeView](#schema-nodeview) | sí | The nodes, by id. |

### RegisterNodeRequest <a id="schema-registernoderequest"></a>

What a node-agent announces on every start. `id` or `name` is required.

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `id` | string | no | The node id. With mTLS it is the certificate's. |
| `name` | string | no | A name for people; defaults to the id. |
| `endpoint` | string | no | The node's own address. |
| `agent_endpoint` | string | no | Where the control plane calls the agent; defaults to `endpoint`. `https://`, or `http://` on loopback. |
| `vmm_profiles` | array de string | no | Defaults to `["cloud-hypervisor"]`. |
| `capacity_cpu` | integer, mínimo 0 | no | Cores offered; 0 is not limited. |
| `capacity_mem_mib` | integer, mínimo 0 | no | Memory offered, in MiB; 0 is not limited. |
| `max_sandboxes` | integer, mínimo 0 | no | At most this many sandboxes; 0 is not limited. |
| `accepts_work` | boolean | no | Defaults to true (agents that predate the field). |
| `local_net_dial` | string | no | `host[:port]` a laptop dials for this node's local-net tunnels. |
| `egress_enforced` | boolean | no | The node forces its guests through its egress proxy. |
| `adopted_sandboxes` | array de string | no | Sandboxes whose VMs the previous process of this agent left running and this one took over. |
| `agent_instance_id` | string | no | Random per process. A new one means the agent restarted. |
| `agent_version` | string | no | The build of the node-agent. |
| `guest_kernel_digest` | string | no | `sha256:<hex>` of the guest kernel. |
| `guest_image_digest` | string | no | `sha256:<hex>` of the base image. |

### EnrollNodeRequest <a id="schema-enrollnoderequest"></a>

`id` or `name` is required.

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `id` | string | no | The node id, which becomes the CN of its certificate and the name the control plane checks when it calls the agent. A DNS-style name: letters, digits, `-` and `_` in labels separated by dots, at most 253 characters, not an IP address. |
| `name` | string | no | A name for people; defaults to the id. |
| `endpoint` | string | no | The node's own address. |
| `agent_endpoint` | string | no | Where the control plane calls the agent; defaults to `endpoint`. |
| `vmm_profiles` | array de string | no | The hypervisor profiles; defaults to `cloud-hypervisor`. |
| `capacity_cpu` | integer | no | Cores offered; 0 is not limited. |
| `capacity_mem_mib` | integer | no | Memory offered, in MiB; 0 is not limited. |

### EnrollResponse <a id="schema-enrollresponse"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `node_id` | string | sí | The node id, as in the certificate. |
| `client_cert_pem` | string | sí | The node's client certificate (also valid as a server certificate for the agent's mTLS listener). |
| `client_key_pem` | string | sí | Its private key. Shown only here: the control plane does not keep it. |
| `ca_cert_pem` | string | sí | The CA that signed it. |
| `cert_fingerprint` | string | sí | SHA-256 of the certificate. |
| `cert_serial` | string | no | Its serial number. |
| `node` | [Node](#schema-node) | sí | A node as the control plane stores it. The fence target is never part of it. |

### EnrollTokenRequest <a id="schema-enrolltokenrequest"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `node_id` | string | no | Pin the token to this node. |
| `ttl_seconds` | integer, mínimo 0, máximo 604800 | no | How long it lasts. 0 or absent is one hour; 7 days at most. |

### EnrollToken <a id="schema-enrolltoken"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `token` | string | sí | `asp_enroll_` and 64 hexadecimal digits. Shown only here; the control plane keeps a hash. |
| `node_id` | string | no | The node it is pinned to; absent if it works for any. |
| `expires_at` | string (date-time) | sí | When it stops working. |
| `note` | string | sí | How to use it. |

### HeartbeatRequest <a id="schema-heartbeatrequest"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `disk_free_mib` | integer (int64), puede ser null | no | Free space of the node's disk directory, in MiB. Absent or null if the node does not report it. |

### NodeWork <a id="schema-nodework"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `sandboxes` | array de [Sandbox](#schema-sandbox) | sí | The sandboxes placed on the node that need its action. |
| `assigned` | array de string | sí | The ids of every sandbox the node must keep running. The node stops any VM it runs that is not listed. |
| `retained` | array de string | sí | The ids of the stopped sandboxes whose disks the node keeps. Always present, even empty; a node that does not see it is talking to an older control plane. |
| `egress` | [WorkEgress](#schema-workegress) | no | The egress policies a node needs, one for each tenant that has a sandbox on it. |

### WorkEgress <a id="schema-workegress"></a>

The egress policies a node needs, one for each tenant that has a sandbox on it.

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `tenants` | objeto (nombre → string) | sí | The tenant of each sandbox assigned to the node, by sandbox id. |
| `policies` | objeto (nombre → [WorkEgressPolicy](#schema-workegresspolicy)) | sí | The effective egress policy of each of those tenants, by tenant id. |

### WorkEgressPolicy <a id="schema-workegresspolicy"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `tenant_id` | string | sí | The tenant. |
| `mode` | string: `allow-all` \| `deny-default` | sí | `deny-default` lets through only what a rule allows. |
| `rules` | array de [EgressRule](#schema-egressrule) | sí | The enabled rules. |
| `version` | string | sí | Changes when what the policy allows can change, and not when the order or the ids of its rules do. |

### SetFenceRequest <a id="schema-setfencerequest"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `endpoint` | string | sí | Where the control plane asks for the power-off. A webhook URL, a Redfish base URL or an IPMI host, according to `ASP_FENCE_PROVIDER`. |
| `token` | string | no | The credential for it, or a reference the control plane resolves when it fences (`env:NAME`, `file:/abs/path`). |

### DoctorResult <a id="schema-doctorresult"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `name` | string | sí | The check. |
| `status` | string: `ok` \| `warn` \| `fail` \| `skip` | sí | `fail` means sandboxes will not start or will not be confined; `warn`, that it works but not as it should; `skip`, that it does not apply to this node. |
| `detail` | string | no | What it found. |
| `fix` | string | no | What to do about a warn or a fail. |

### DoctorReport <a id="schema-doctorreport"></a>

The report of a node-agent's self-checks, as the agent made it.

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `node_id` | string | no | The node. |
| `at` | string (date-time) | sí | When the checks ran. |
| `results` | array de [DoctorResult](#schema-doctorresult) | sí | One entry per check, in the order they ran. |

### CreateApiKeyRequest <a id="schema-createapikeyrequest"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `tenant_id` | string | no | The tenant of the key. Defaults to the caller's tenant, or `default`. |
| `name` | string | sí | Unique within the tenant. |
| `scope` | string: `tenant` \| `platform` | no | Defaults to `tenant`. Only a platform key can make a `platform` key. |
| `ttl` | string | no | How long it lasts, a Go duration (`720h`) or days (`30d`), at most 5 years. Empty: it does not expire. |

### ApiKey <a id="schema-apikey"></a>

An API key, without its secret.

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `id` | string | sí | The id of the key: what the other calls take. Not the secret. |
| `tenant_id` | string | sí | The tenant it belongs to. |
| `name` | string | sí | Its name, unique within the tenant. |
| `scope` | string: `tenant` \| `platform` | sí | `tenant` acts within its tenant; `platform` sees every tenant and may call the node routes. |
| `key_prefix` | string | sí | The first 8 characters of the secret, to tell keys apart. Not a secret. |
| `last_used_at` | string (date-time) | no | When it last authenticated a request; written at most once a minute. |
| `expires_at` | string (date-time) | no | When it stops working; absent if it does not expire. |
| `revoked_at` | string (date-time) | no | When it was revoked; absent otherwise. |
| `created_at` | string (date-time) | sí | When it was created. |

### ApiKeyWithSecret <a id="schema-apikeywithsecret"></a>

An API key and its secret, as create and rotate return them.

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `id` | string | sí | The id of the key: what the other calls take. Not the secret. |
| `tenant_id` | string | sí | The tenant it belongs to. |
| `name` | string | sí | Its name, unique within the tenant. |
| `scope` | string: `tenant` \| `platform` | sí | `tenant` acts within its tenant; `platform` sees every tenant and may call the node routes. |
| `key_prefix` | string | sí | The first 8 characters of the secret, to tell keys apart. Not a secret. |
| `last_used_at` | string (date-time) | no | When it last authenticated a request; written at most once a minute. |
| `expires_at` | string (date-time) | no | When it stops working; absent if it does not expire. |
| `revoked_at` | string (date-time) | no | When it was revoked; absent otherwise. |
| `created_at` | string (date-time) | sí | When it was created. |
| `secret` | string | sí | The key itself, to use as a Bearer token. Shown only here: the control plane keeps a hash. |

### ApiKeyList <a id="schema-apikeylist"></a>

| Campo | Tipo | Obligatorio | Descripción |
|---|---|---|---|
| `keys` | array de [ApiKey](#schema-apikey) | sí | The keys. |

