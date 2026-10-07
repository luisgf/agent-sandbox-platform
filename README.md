# Agent Sandbox Platform (ASP)

[![CI](https://github.com/luisgf/agent-sandbox-platform/actions/workflows/ci.yml/badge.svg)](https://github.com/luisgf/agent-sandbox-platform/actions/workflows/ci.yml)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

**Run the code your AI agent writes inside a microVM — never on your host.**

ASP is a self-hosted, FOSS sandbox platform for coding agents. An agent harness gets a long-lived **session**: one Cloud Hypervisor microVM with its own disk, its own deny-by-default egress and an owner taken from your IdP. Every shell command the agent runs goes through `asp session exec` and executes inside that guest. Your SSH keys, your tokens and the hypervisor socket stay on the host.

> Independent project. Not affiliated with, sponsored or endorsed by Cursor, Anysphere, anyrun or related products. The design is inspired by that kind of sandbox; the code and threat model are our own.

---

## Contents

- [Why](#why)
- [Architecture](#architecture)
- [How a session works](#how-a-session-works)
- [Sandbox lifecycle](#sandbox-lifecycle)
- [Network and identity](#network-and-identity)
- [Quickstart (dry-run, no KVM)](#quickstart-dry-run-no-kvm)
- [Using ASP with OpenCode](#using-asp-with-opencode)
- [Configuration cheat sheet](#configuration-cheat-sheet)
- [Status and known limits](#status-and-known-limits)
- [Repository layout](#repository-layout)
- [Documentation](#documentation)
- [Development](#development)
- [License](#license)

---

## Why

| Problem | What ASP does |
|---|---|
| Agent-generated code runs with your user's privileges on your machine. | Every command runs in a **microVM** (KVM + Cloud Hypervisor). The VM is the primary security boundary. |
| Booting a VM per tool call is too slow, so harnesses fall back to the host. | A **session** keeps one sandbox alive for hours; dozens of `exec` calls share the same guest. |
| Agents need Git/SSH and cloud tokens, but secrets must not leak into the guest. | The **SSH agent stays on the host** (bridged over vsock) and the guest only gets **short-lived OIDC tokens** minted by the control plane. |
| You need to know *who* owns a sandbox. | `owner_sub` comes from the **IdP JWT**, never from the client body or the guest. |
| An agent should not reach arbitrary hosts. | **Deny-by-default egress**: forward proxy + DNS sink + nftables, with a per-tenant allowlist. |

ASP is **not** a Kubernetes replacement (sandboxes are not Pods), does **not** do TPM/SEV attestation and is **not** a multi-language SDK. The `asp` CLI is the integration contract.

---

## Architecture

Four processes, three trust boundaries. Only the node agent ever talks to the hypervisor; the client never sees it. Capacity grows by adding servers: one node agent per server, and the control plane places each sandbox on a node with room ([ADR-0011](docs/adr/0011-multi-node.md)).

```mermaid
flowchart LR
  subgraph USER["User machine"]
    H["Agent harness<br/>(OpenCode, etc.)"]
    CLI["asp CLI"]
    IDP[("IdP<br/>(Keycloak, …)")]
  end

  subgraph CP["Control plane · Go"]
    API["HTTP API<br/>sandboxes · nodes · exec proxy<br/>scheduler · node monitor"]
    DB[("Postgres<br/>or in-memory")]
    OIDC["OIDC issuer · JWKS"]
    API --- DB
    API --- OIDC
  end

  subgraph NODE["Node × N (bare metal) · privileged"]
    NA["node-agent · Go<br/>reconciler · TAP · nft"]
    EG["Egress<br/>proxy :8888 · DNS sink"]
    VMM["Cloud Hypervisor<br/>(FakeVMM in dry-run)"]
    subgraph VM["microVM · untrusted"]
      PD["pod-daemon · Rust"]
      WL["agent workload"]
    end
  end

  NET(("Internet<br/>allowlist only"))

  H -->|"shell tool"| CLI
  IDP -.->|"JWT"| CLI
  CLI -->|"HTTPS + Bearer"| API
  API <-->|"mTLS · desired state"| NA
  NA -->|"start / stop"| VMM
  VMM --> VM
  NA <-->|"vsock 26500 exec"| PD
  WL -.->|"vsock 26501 SSH agent<br/>vsock 26502 OIDC"| NA
  WL -->|"TAP + nft redirect"| EG
  EG --> NET
```

| Component | Path | Language | Responsibility |
|---|---|---|---|
| **Control plane** | [`control-plane/`](control-plane/) | Go | Multi-tenant API, desired state, event journal, node enrollment (PKI + mTLS both ways), capacity scheduler, node monitor (lost-node failover and fencing), exec proxy, egress policies, OIDC mint/JWKS, software attestation, idle reaper. Node administration (list, cordon, revoke, rotate-cert) takes an IdP admin or a platform-scoped API key, never a tenant key. |
| **Node agent** | [`node-agent/`](node-agent/) | Go | Reports the host's capacity, polls the control plane for the sandboxes placed on it, spawns one Cloud Hypervisor per sandbox, creates TAP devices, applies nftables, runs the egress proxy and DNS sink, bridges SSH agent / OIDC over vsock, starts `virtiofsd`, sets up per-session WireGuard, and removes a previous agent's leftovers on start. |
| **pod-daemon** | [`pod-daemon/`](pod-daemon/) | Rust | Runs inside the guest. Executes commands received over vsock (buffered JSON or streamed NDJSON, optional PTY). |
| **Guest image** | [`images/guest/`](images/guest/) | Dockerfile / shell | Minimal Debian rootfs with systemd/OpenRC units for pod-daemon, the SSH-agent vsock proxy and the virtiofs workspace mount. |
| **CLI `asp`** | [`cli/`](cli/) | Go | `sandbox`, `session`, `auth` and `node` commands (`node` needs an IdP admin, or operator to list, or a platform-scoped API key); resolves IdP tokens transparently. |

**vsock ports** (local to the node, never exposed on the network):

| Port | Direction | Purpose |
|---|---|---|
| `26500` | host → guest | Exec (pod-daemon) |
| `26501` | guest → host | SSH agent (keys never enter the guest) |
| `26502` | guest → host | OIDC token requests |

---

## How a session works

A session is a named, long-lived sandbox. The local session file stores only the sandbox id and control-plane URL — **never a token**.

```mermaid
sequenceDiagram
  autonumber
  participant H as Harness
  participant C as asp CLI
  participant CP as Control plane
  participant NA as node-agent
  participant VM as microVM (pod-daemon)

  H->>C: asp session start --name agent
  C->>CP: POST /v1/sandboxes (Bearer JWT)
  Note over CP: places it on a node with room<br/>(503 if none fits)
  CP-->>C: sandbox id, node, state=requested
  NA->>CP: GET /v1/nodes/{id}/work
  NA->>CP: POST /v1/sandboxes/{id}/claim (only the assigned node)
  NA->>VM: TAP + nft + boot Cloud Hypervisor
  NA->>CP: POST /v1/sandboxes/{id}/status running
  C->>CP: poll until running
  C-->>H: id (writes ~/.cache/asp/sessions/agent.json)

  loop every tool call
    H->>C: asp session exec --name agent --cmd '…'
    C->>CP: POST /v1/sandboxes/{id}/exec?stream=1
    CP->>NA: /v1/internal/exec (mTLS when the node is another host)
    NA->>VM: vsock 26500
    VM-->>C: NDJSON stdout / stderr / exit
    C-->>H: same output, same exit code
  end

  H->>C: asp session stop --name agent
  C->>CP: POST /v1/sandboxes/{id}/stop
  NA->>VM: guest powers off; TAP and sockets cleaned, disk kept
  Note over H,VM: later: asp session resume boots it again on the same disk;<br/>asp session rm deletes the sandbox and its disk
```

For one-off jobs (CI, ops) there is still the single-shot primitive `asp sandbox run --cmd '…'`, which does create → exec → destroy in one call.

---

## Sandbox lifecycle

The control plane stores *desired* state; the node agent reconciles it against what is actually running. Updates are guarded by `state_version`. Each work poll also tells the node which sandboxes are still assigned to it, and the node stops any VM that is not: a sandbox failed over or destroyed never keeps running on a node that comes back.

```mermaid
stateDiagram-v2
  [*] --> requested: POST /v1/sandboxes (placed on a node with room)
  requested --> stopped: stop before any node claims it
  requested --> starting: the assigned node claims it
  starting --> running: VM booted
  starting --> failed: boot / TAP / virtiofsd error
  running --> failed: node lost (node_lost) · agent restarted
  running --> paused
  paused --> running
  running --> stopping: stop · idle timeout
  paused --> stopping
  stopping --> stopped: guest powered off, VM + TAP cleaned up, disk kept
  stopped --> requested: resume (same node, same disk)
  starting --> stopped: a resume that could not start
  running --> deleting: DELETE
  stopping --> deleting: DELETE
  stopped --> deleting: DELETE
  deleting --> deleted: VM and disk removed
  requested --> deleted: DELETE before any node claims it
  failed --> deleted: DELETE
  deleted --> [*]
```

- **Placement:** the control plane picks the node at create time from the nodes that can take sandboxes and their free CPU (overcommitted 4x by default), memory and slots; `ASP_SCHED_POLICY=spread|binpack`. When nothing fits, create fails at once with 503 and the reasons; only the chosen node can claim the sandbox. See [`docs/ops-multi-node.md`](docs/ops-multi-node.md).
- **Stop is not delete** ([ADR-0012](docs/adr/0012-retained-disks.md)): a stop powers the guest off and keeps its disk on the node, so what the agent installed outside `/workspace` is still there after `asp session resume`. Resume is pinned to the node that holds the disk: 409 if that node is cordoned or down, 503 if it is full. `DELETE` (`asp session rm`) removes the VM and the disk. A deleted sandbox stays in the database as `deleted`, hidden from the list unless `?include_deleted=1`, so its events survive.
- **Idle reaper:** with `ASP_SANDBOX_IDLE_TIMEOUT=2h`, a forgotten sandbox is *stopped*, not deleted: `asp session resume` brings it back. Create, reaching `running` and a successful exec count as activity; status calls and heartbeats do not. Off by default.
- **Node failure:** a node silent for `ASP_NODE_STALE_AFTER` (90 s) gets no new sandboxes and goes `offline`; after `ASP_NODE_FAILOVER_AFTER` (5 min) it is fenced (if a `FenceProvider` is set) and its sandboxes fail with `node_lost`. They are not moved: the guest disk lives on that server. A node that comes back stops the VMs it no longer owns. See [ADR-0011](docs/adr/0011-multi-node.md).

---

## Network and identity

By default a guest can only reach hosts in its tenant's allowlist. With `--local-net` (opt-in, per session), the session's default route goes through a WireGuard tunnel back to the user's machine instead, so the agent can reach the user's LAN.

```mermaid
flowchart TB
  G["guest"] --> TAP["TAP asp-{id8}"]
  TAP --> Q{"session started<br/>with --local-net?"}

  Q -->|"no (default)"| NFT["nftables redirect<br/>HTTP(S) + DNS"]
  NFT --> PX["forward proxy :8888<br/>DNS sink (NXDOMAIN)"]
  PX -->|"tenant allowlist"| INET(("Internet"))

  Q -->|"yes"| RT["per-session policy routing table<br/>(never the host's main table)"]
  RT --> S{"tunnel state"}
  S -->|"pending / withdrawn"| BH["blackhole<br/>(no silent fallback to proxy)"]
  S -->|"up"| WG["wg-asp-{id8} on the node"]
  WG <-->|"WireGuard"| LW["wg-asp-{id8} on the user machine<br/>(Linux or macOS utun)"]
  LW --> LAN(("User's LAN"))
```

**Secrets stay on the host:**

- **SSH:** inside the guest, `SSH_AUTH_SOCK` points to a vsock proxy (port 26501). Sign requests are forwarded to an agent on the host, optionally gated by an explicit approval. The private key is never copied.
- **OIDC:** the guest asks for a token for an audience (port 26502). The node agent takes the sandbox from the guest's vsock connection, so a guest cannot ask for another sandbox's token, and the control plane mints a short-lived JWT with the tenant from its store; any user claims sent by the guest are ignored.

---

## Quickstart (dry-run, no KVM)

Dry-run uses `FakeVMM`: it exercises the whole control path on a laptop or in CI. **It provides no isolation.** For real microVMs see [`docs/bare-metal-ch.md`](docs/bare-metal-ch.md).

**Requirements:** Go 1.22+, Rust/Cargo. Docker only for Postgres or for building the guest rootfs.

### 1. Build and test

```bash
make test        # Go + Rust unit tests
make smoke       # enroll / identity / reconcile / two-node smoke scripts
make asp         # builds ./build/asp
make smoke-asp   # CLI end-to-end in dry-run
```

### 2. Run the three processes

```bash
export ASP_NODE_BOOTSTRAP_TOKEN=dev-node-bootstrap

# terminal 1 — control plane (in-memory store, 127.0.0.1:8080). Authentication is always
# on; this dry-run has no keys, so it opts out explicitly (labs and laptops only).
(cd control-plane && ASP_INSECURE_OPEN_API=1 go run ./cmd/api)

# terminal 2 — pod-daemon on a unix socket (stands in for the guest)
(cd pod-daemon && cargo run -- --listen unix --unix-socket /tmp/pod-daemon.sock)

# terminal 3 — node agent in dry-run
(cd node-agent && go run ./cmd/node-agent \
  --control-plane-url=http://127.0.0.1:8080 --node-id=dev-node \
  --dry-run --enroll --bootstrap-token=dev-node-bootstrap \
  --cert-dir=/tmp/asp-node-certs --agent-listen=127.0.0.1:9100 \
  --pod-daemon-sock=/tmp/pod-daemon.sock --reconcile \
  --host-vsock --host-vsock-dir=/tmp/asp-hv)
```

### 3. Open a session and run commands

```bash
./build/asp session start --name demo
./build/asp session exec  --name demo --cmd 'uname -a'
./build/asp session status --name demo
./build/asp session stop  --name demo     # keeps the disk; `resume` boots it again, `rm` deletes it
```

The control plane places the session on a node with room; with the single dry-run node that is `dev-node`. `--node-id` pins a node instead.

**Persistence (optional):** `docker compose up -d postgres` and start the control plane with `DATABASE_URL=postgres://asp:asp@127.0.0.1:5432/asp?sslmode=disable`. Without it, state is lost when the control plane exits.

Step-by-step walkthrough and troubleshooting: [`docs/mvp-smoke.md`](docs/mvp-smoke.md).

---

## Using ASP with OpenCode

[OpenCode](https://opencode.ai) runs every bash tool call as `<shell> -c "<command>"`, and its `shell` config option picks that binary. [`integrations/opencode/`](integrations/opencode/) points it at ASP, so every command the model runs goes to `asp session exec` and executes inside the microVM:

| File | What it does |
|---|---|
| `asp-opencode-shell` | The `shell`. Runs each command with `/bin/sh -c` in the session and returns the guest command's exit code. |
| `plugins/asp-sandbox.ts` | OpenCode plugin. Refuses to fall back to the host shell when the wrapper is missing, maps `workdir` into the guest, and tells the model the bash tool runs on Linux in the sandbox. |
| `instructions/*.md` | What the model needs to know about the sandbox, one file per setup. |
| `opencode.*.json` | Example project configs, one per setup. |

```mermaid
flowchart LR
  M["model"] -->|"bash tool call"| OC["OpenCode<br/>+ asp-sandbox plugin"]
  OC -->|"asp-opencode-shell -c '…'"| W["wrapper"]
  W -->|"asp session exec -- /bin/sh -c '…'"| CP["control plane"]
  CP --> VM["microVM<br/>/workspace"]
  OC -. "read / edit / write tools<br/>(same-host setup only)" .-> REPO[("repo on the node")]
  REPO <-. "virtiofs" .-> VM
```

### Pick a setup

| | Bash only | Same host |
|---|---|---|
| Where OpenCode runs | Any machine, for example your laptop | The node that runs the session |
| Where the repository lives | In the sandbox: a `--workspace` directory on the node, or the guest's own disk | On the node, shared into the guest with `--workspace` |
| OpenCode's read / edit / write / glob / grep / list tools | Denied. The model reads and edits files with bash, in the sandbox | On the host, on the same files the guest sees through virtiofs |
| Files | `opencode.bash-only.json`, `instructions/bash-only.md` | `opencode.same-host.json`, `instructions/same-host.md` |

The bash-only setup was tested end to end with OpenCode 1.18.34: from a laptop against a node, with the task, the `workdir` mapping, the missing-wrapper guard and file ownership all checked.

### 1. Install the CLI and authenticate

```bash
make asp && install -m 0755 build/asp ~/.local/bin/asp   # any directory on PATH

export ASP_CP_URL=https://asp.example.internal          # your control plane
asp auth login                                          # IdP setups
# or, in a lab without an IdP:  export ASP_API_KEY=…
```

### 2. Start a session

`--workspace` shares a directory of the node into the guest at `/workspace`. In the same-host setup, run this from the repository:

```bash
cd ~/src/my-project
asp session start --name opencode --workspace "$PWD" --timeout=120s
```

Against the dry-run stack from the [Quickstart](#quickstart-dry-run-no-kvm) nothing else is needed: the control plane places the session on a node with room. With several nodes, pin one with `--node-id` if the `--workspace` path only exists there. Add `--local-net` if the agent must reach your LAN.

### 3. Install the wrapper

```bash
install -m 0755 integrations/opencode/asp-opencode-shell ~/.local/bin/asp-opencode-shell
```

The wrapper runs commands as the owner of `/workspace` (`setpriv` in the guest), so files the agent creates keep your uid on the host: virtiofs does not map ids. `ASP_GUEST_AS_ROOT=1` keeps root. Guest images built before [#83](https://github.com/luisgf/agent-sandbox-platform/pull/83) have `/` at mode 0700 and only root can run anything there: rebuild the rootfs, or set `ASP_GUEST_AS_ROOT=1`. The header of the script lists its other variables.

### 4. Add the plugin, the instructions and the config to the project

```bash
cd ~/src/my-project
ASP_KIT=~/src/agent-sandbox-platform/integrations/opencode
mkdir -p .opencode/plugins
cp "$ASP_KIT/plugins/asp-sandbox.ts" .opencode/plugins/
cp "$ASP_KIT/instructions/bash-only.md" .opencode/asp-sandbox.md   # or same-host.md
cp "$ASP_KIT/opencode.bash-only.json" opencode.json                # or opencode.same-host.json
```

Set `"shell"` in `opencode.json` to the wrapper's absolute path. Keep the plugin in the project, not in `~/.config/opencode/plugins`: it treats any configured `shell` as the ASP wrapper.

What the example configs set, and why:

- **`external_directory`.** OpenCode checks the paths in bash commands against the project directory on the host, so `/workspace/...` looks like an outside path. Bash only: `allow`, because every bash path is a guest path and the file tools are off. Same host: `/workspace` and `/tmp` are allowed and everything else asks, because the file tools still work on the host.
- **`webfetch: ask`.** `webfetch` runs on the host, outside the session's egress policy.
- **`lsp` and `formatter` off.** They run host binaries on the repository.

### 5. Launch OpenCode

```bash
export ASP_SESSION_NAME=opencode ASP_WORKSPACE_HOST="$PWD"
opencode
```

**Check it:** ask the agent to run `hostname && id && pwd`. You should see the guest's hostname, your uid and `/workspace`, not your machine.

In scripts, give `opencode run` an empty stdin: `opencode run "…" < /dev/null`. Without a terminal it waits for EOF on stdin.

**Control plane only reachable from the node** (for example a lab with the API on loopback): set `ASP_SSH`. The wrapper then sends each command over ssh and runs itself on the node with `--remote-exec`. `ASP_REMOTE_EXEC` is that remote command, with the environment `asp` needs there:

```bash
export ASP_SSH=ubuntu@node1
export ASP_REMOTE_EXEC='env ASP_CP_URL=http://127.0.0.1:18112 /home/ubuntu/.local/bin/asp-opencode-shell --remote-exec'
```

Over ssh, session names and guest directories may only use `A-Z a-z 0-9 _ . / -`, and there is no interactive terminal.

### 6. Stop the session

```bash
asp session stop   --name opencode    # powers the sandbox off, keeps its disk
asp session resume --name opencode    # boots it again on that disk
asp session rm     --name opencode    # deletes the sandbox, its disk and the session file
```

If you forget, the control plane stops it after `ASP_SANDBOX_IDLE_TIMEOUT` (when enabled), which is a stop like any other. A later `exec` then fails with `idle timeout` and says its disk is kept: run `asp session resume --name opencode`. `stop` used to delete; to delete now, use `rm`.

### What is and is not sandboxed

| | Where it runs |
|---|---|
| Bash tool calls and `!` commands (`npm test`, `git`, `curl`, …) | **In the microVM**, as the owner of `/workspace`, with the session's egress policy and host-held SSH agent. |
| OpenCode's read / edit / write / glob / grep / list tools | Bash only: denied. Same host: **on the host**, on the shared repository. |
| `webfetch` | On the host, after you approve it. |
| OpenCode itself, the LLM API calls and your credentials | On the host. They never enter the guest. |

**Practical notes**

- One session per agent. Two OpenCode instances with different `ASP_SESSION_NAME` values get two independent sandboxes.
- If no node has room, `session start` fails with `no capacity` and the reason (for example `2 max_sandboxes`). `asp node list` shows what each node has in use.
- If the session's node is lost, the sandbox fails with `node_lost` and `session status` says so; its disk lived on that server, so run `asp session start --force`. A resume needs the node that holds the disk: it answers 503 if the node is full and 409 if it is cordoned or down.
- A buffered exec (`--buffered`, `--json`, `asp sandbox run`) is killed after the pod-daemon's `--exec-timeout-secs` (30 s by default in the guest image). A streamed exec, the default of `asp session exec`, has no time limit: it ends when the command exits or when the client goes away, and then the guest kills the command. `--stream-idle-timeout-secs` in the guest image adds an inactivity limit.
- OpenCode merges stdout and stderr. An `asp` error (`no active session`, 401, network) exits 1 like a failing command; the model tells them apart by the message. Usage errors of the wrapper exit 125 and ssh errors 255.
- The plugin edits the system prompt through OpenCode's experimental `experimental.chat.system.transform` hook. Pin OpenCode, and after an upgrade check that the model still sees `/workspace` and `Platform: linux`.
- The plugin moves `workdir` from the tool call to the wrapper, so the call shown in the session no longer has it.
- Guest images built before `workspace-virtiofs.service` don't auto-mount `/workspace`. Run `mkdir -p /workspace && mount -t virtiofs workspace /workspace` once through the wrapper with `ASP_GUEST_AS_ROOT=1`, or rebuild the rootfs.
- No `--workspace`? The bash-only setup still works on the guest's own disk. In the same-host setup, the host-side file tools and the shell would see different files.

Full session contract, flags and failure table: [`docs/ops-asp-session.md`](docs/ops-asp-session.md). Any other harness that lets you replace its shell works the same way.

### Optional session features

| Flag / setting | What it does | Guide |
|---|---|---|
| `--workspace /abs/path` | Shares a host directory into the guest via virtiofs, mounted at `/workspace`. | [`why-virtiofs-pty.md`](docs/why-virtiofs-pty.md) |
| `--local-net` + `asp session local-net up` | Routes the session's traffic through a WireGuard tunnel to your machine. | [`ops-local-net.md`](docs/ops-local-net.md) |
| `asp auth login` | Fetches and caches an IdP token; the CLI then sends it as Bearer automatically. | [`ops-idp-keycloak-lab.md`](docs/ops-idp-keycloak-lab.md) |
| `ASP_SANDBOX_IDLE_TIMEOUT` | Stops idle sandboxes on the control plane (their disks are kept). | [`control-plane/README.md`](control-plane/README.md) |
| `asp session stop` / `resume` / `rm` | Stop keeps the disk, resume boots it again on the same node, rm deletes it. `ASP_STOPPED_SANDBOX_TTL` (7 days) bounds how long a stopped one is kept. | [ADR-0012](docs/adr/0012-retained-disks.md) |

---

## Configuration cheat sheet

Only the most common settings. Full lists live in each component's README.

| Variable | Used by | Purpose |
|---|---|---|
| `ASP_CP_URL` | CLI | Control-plane URL (default `http://127.0.0.1:8080`). |
| `ASP_ID_TOKEN` / `ASP_API_KEY` | CLI | Bearer credential (IdP token preferred; API key for labs without an IdP). |
| `ASP_TENANT` | CLI | Tenant for create/list/run; empty uses the caller's tenant (token claim or key). |
| `ASP_IDP_TENANT_CLAIM` / `ASP_IDP_DEFAULT_TENANT` | CP | Where a user's tenant comes from (claim `tenant_id` by default); every request is confined to it. |
| `ASP_IDP_REQUIRED` | CLI, CP | Require an IdP token. |
| `ASP_SESSION_DIR` | CLI | Where session files live (default `~/.cache/asp/sessions`, mode `0700`). |
| `LISTEN_ADDR` | CP | API listen address (default `127.0.0.1:8080`). |
| `ASP_BOOTSTRAP_API_KEY` | CP | Creates the first API key (platform scope). Without a key or an IdP the control plane refuses to start. |
| `ASP_INSECURE_OPEN_API` | CP | `1` accepts requests with no credential (labs and dry-run only; logged at start). |
| `ASP_NODE_API_KEY` / `ASP_NODE_API_KEY_FILE` | node-agent | The platform key a node sends to a control plane reached over plain HTTP. |
| `DATABASE_URL` | CP | Use Postgres instead of the in-memory store. |
| `ASP_SANDBOX_IDLE_TIMEOUT` | CP | Idle stop (e.g. `2h`); off by default. A stop keeps the disk. |
| `ASP_STOPPED_SANDBOX_TTL` | CP | Delete a sandbox (and its disk) stopped for this long. Default `7d`; `0`/`off` keeps it until deleted. Needs Postgres to mean anything. |
| `ASP_MAX_STOPPED_PER_TENANT` | CP | Keep at most this many stopped sandboxes per tenant, deleting the oldest. Default: no cap. |
| `ASP_NODE_BOOTSTRAP_TOKEN` | CP, node | One-time token for node enrollment. |
| `ASP_CLIENT_CA` | CP | Require node client certificates and bind each node route to the certificate's node. |
| `--agent-tls-listen` | node | mTLS exec listener for a control plane on another host ([ADR-0011](docs/adr/0011-multi-node.md)). |
| `ASP_SCHED_POLICY` | CP | `spread` (default) or `binpack`. |
| `ASP_SCHED_CPU_OVERCOMMIT` | CP | vCPUs per physical core (default `4`); memory is never overcommitted. |
| `ASP_SCHED_VM_OVERHEAD_MIB` | CP | Memory each microVM costs beyond its `memory_mib` (default `64`), counted in placement. Sandboxes ask for at least 64 MiB. |
| `--capacity-cpu` / `--capacity-mem-mib` / `--max-sandboxes` | node | What the node offers; detected from the host by default. |
| `ASP_NODE_STALE_AFTER` / `ASP_NODE_FAILOVER_AFTER` | CP | A silent node leaves placement after `90s`; its sandboxes fail after `5m`. |

Reference: [`cli/README.md`](cli/README.md) · [`control-plane/README.md`](control-plane/README.md) · [`node-agent/README.md`](node-agent/README.md) · [`pod-daemon/README.md`](pod-daemon/README.md).

---

## Status and known limits

ASP is an MVP that has been hardened in phases (see the [roadmap](docs/roadmap.md)). What the code does **not** do yet:

| Area | Reality today |
|---|---|
| Dry-run | `FakeVMM` exercises the control plane only. No KVM, no isolation. |
| nftables | `soft` mode tolerates missing root; `enforce` needs privileges. CI does not prove bypass resistance. |
| Attestation | Software signature, not TPM/SEV. The control plane only trusts keys it is configured with (`ASP_ATTEST_KEY`, `ASP_ATTEST_TRUSTED_PUBS`) or, over mTLS, the key of the node certificate the request came with. It proves which node signed, not what actually booted. |
| Fencing | Lost-node failover calls a `FenceProvider` (the webhook works; Redfish and IPMI are stubs). Not real BMC STONITH. |
| Exec timeouts | Only buffered execs have a time limit (30 s in the control plane and the guest's `--exec-timeout-secs`). Streams and PTY sessions last as long as the command. See [timeouts](control-plane/README.md#timeouts-hacia-el-node-agent). |
| Idle timeout | Off by default. Does not delete the local session file. |
| `--local-net` | Real `ip`/`wg` commands on node and client; needs `wireguard-tools` and `CAP_NET_ADMIN`. End-to-end packet flow not yet lab-tested. |
| Workspace (virtiofs) | Auto-mount ships in newly built guest images; older images need `mount -t virtiofs workspace /workspace`. No KVM test in CI. |
| Harness integration | A shell wrapper is the integration point; for OpenCode, `integrations/opencode/` adds a plugin, instructions and example configs. |
| Flow attribution | Mapping network flows to `owner_sub` is designed ([ADR-0008](docs/adr/0008-network-flow-attribution.md)) but not implemented. |
| Kubernetes | Optional, only to deploy the API. Sandboxes are not Pods. |
| Multiple nodes | Capacity placement, cordon, lost-node failover and mTLS between control plane and nodes. No migration: a lost server takes its sessions with it. Agents do not adopt running VMs after a restart: the systemd unit stops them with the agent, and a starting agent removes whatever a previous one left (VMs, TAPs, tunnels, disks). One node-agent per server. Placement does not know about `--workspace` paths. Not tested on a multi-server KVM lab yet. |

---

## Repository layout

```text
.
├── cli/              asp CLI (sandbox · session · auth · node)
├── control-plane/    API, scheduler, node monitor, store + SQL migrations, PKI, OIDC, attest, local-net
├── node-agent/       reconciler, VMM drivers, capacity, host cleanup, TAP, nft, egress proxy, vsock, virtiofs, WireGuard
├── pod-daemon/       in-guest exec daemon (Rust)
├── images/guest/     Debian rootfs, systemd/OpenRC units, vsock SSH-agent proxy
├── integrations/opencode/  OpenCode shell wrapper, plugin, model instructions, example configs
├── scripts/          smoke tests, rootfs build, release pack, nft helpers, systemd units, diagram generation
├── docs/             architecture, ADRs, ops guides, design notes
├── docker-compose.yml  Postgres for local development
└── Makefile
```

---

## Documentation

> The documents under `docs/` are written in Spanish.

**Start here**

| Topic | Document |
|---|---|
| Architecture, trust boundaries, threat model | [`docs/architecture.md`](docs/architecture.md) |
| Phases delivered and open gaps | [`docs/roadmap.md`](docs/roadmap.md) |
| Dry-run smoke test | [`docs/mvp-smoke.md`](docs/mvp-smoke.md) |
| Real Cloud Hypervisor on bare metal | [`docs/bare-metal-ch.md`](docs/bare-metal-ch.md) |

**Operations guides**

| Topic | Document |
|---|---|
| Agent sessions and harness wrapper | [`docs/ops-asp-session.md`](docs/ops-asp-session.md) |
| One-shot `asp sandbox run` | [`docs/ops-asp-agent-runner.md`](docs/ops-asp-agent-runner.md) |
| Keycloak IdP lab | [`docs/ops-idp-keycloak-lab.md`](docs/ops-idp-keycloak-lab.md) |
| On-demand local network | [`docs/ops-local-net.md`](docs/ops-local-net.md) |
| Multiple servers: capacity, placement, cordon, adding a node | [`docs/ops-multi-node.md`](docs/ops-multi-node.md) |
| Guest vsock notes | [`scripts/guest-vsock-notes.md`](scripts/guest-vsock-notes.md) |

**Architecture decision records**

| ADR | Decision |
|---|---|
| [0001](docs/adr/0001-vmm-choice.md) | Cloud Hypervisor as the VMM |
| [0002](docs/adr/0002-networking.md) | Node networking and egress |
| [0003](docs/adr/0003-identity.md) | SSH agent and OIDC kept outside the guest |
| [0004](docs/adr/0004-k8s-scope.md) | Kubernetes only to deploy the control plane |
| [0005](docs/adr/0005-fase-2d-hardening.md) | Phase 2d hardening |
| [0006](docs/adr/0006-fase-2e-nft-ssh-guest.md) | nftables and SSH in the guest |
| [0007](docs/adr/0007-multi-user-identity.md) | Multi-user identity via IdP |
| [0008](docs/adr/0008-network-flow-attribution.md) | Network flow → `owner_sub` attribution (evaluation, not implemented) |
| [0009](docs/adr/0009-agent-sessions.md) | Sessions as the primary use of isolation |
| [0010](docs/adr/0010-on-demand-local-net.md) | On-demand local network: full tunnel, opt-in |
| [0011](docs/adr/0011-multi-node.md) | Multiple nodes: node identity bound to its certificate, mutual TLS between control plane and nodes, capacity placement |
| [0012](docs/adr/0012-retained-disks.md) | Stop is not delete: a sandbox's disk survives a stop, bounded by a TTL |

Design rationale notes (`why-*.md`) are in [`docs/`](docs/).

---

## Development

```bash
make test             # all unit tests (Go modules + Rust + guest helper; control plane and node agent with -race)
make lint             # gofmt, go vet and staticcheck for every Go module, as CI runs them
make smoke            # control-plane / node-agent smoke scripts
make smoke-multi-node # two dry-run nodes: placement, cordon, node loss, agent restart
make smoke-asp        # CLI end-to-end (dry-run)
make smoke-asp-auth   # CLI against an IdP lab
make pack             # release tarball
./scripts/gen-diagram.sh   # regenerate docs/diagram.svg from docs/diagram.mmd
```

Each Go component is its own module (`control-plane/`, `node-agent/`, `cli/`); `pod-daemon/` is a Cargo crate.

---

## License

[Apache License 2.0](LICENSE). To report a vulnerability, see [SECURITY.md](SECURITY.md).
