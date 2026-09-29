# Guest ↔ host vsock notes (MVP)

## Port map

| Port | Direction | Protocol | Role |
|---|---|---|---|
| **26500** | host → guest | HTTP JSON | pod-daemon (`GET /healthz`, `POST /v1/exec`) |
| **26501** | guest → host | SSH agent | byte-pump to host `SSH_AUTH_SOCK` / FakeAgent |
| **26502** | guest → host | HTTP JSON | identity proxy `POST /v1/tokens/oidc` |

## Host → guest (exec) — Cloud Hypervisor hybrid

CH binds a host Unix muxer (`/run/asp/vsock-{sandboxID}.sock`). Node-agent:

```text
connect(UDS) → WRITE "CONNECT 26500\n" → READ "OK …\n" → HTTP
```

Guest: `pod-daemon --listen vsock --vsock-port 26500` (AF_VSOCK `CID_ANY`).

## Guest → host (SSH + identity) — AF_VSOCK CID 2

With virtio-vsock, the **host/hypervisor CID is 2**. Guest apps dial:

```text
AF_VSOCK connect(cid=2, port=26501)  → SSH agent
AF_VSOCK connect(cid=2, port=26502)  → identity HTTP
```

Enable on the node:

```bash
node-agent --host-vsock --reconcile ...
# Lab without /dev/vsock:
node-agent --host-vsock --host-vsock-dir=/run/asp ...
# → unix sockets /run/asp/host-vsock-26501.sock and host-vsock-26502.sock
```

Env in guest: `ASP_HOST_CID=2` (default in image).

### Identity from guest (example with socat + curl)

```bash
# In guest (needs socat):
socat TCP-LISTEN:18080,reuseaddr,fork VSOCK-CONNECT:${ASP_HOST_CID:-2}:26502 &
curl -sS -X POST http://127.0.0.1:18080/v1/tokens/oidc \
  -H 'Content-Type: application/json' \
  -H "X-ASP-Sandbox-ID: $ASP_SANDBOX_ID" \
  -d '{"aud":"https://api.example.com"}'
```

Or a tiny Go/Rust client dialing `vsock.Dial(2, 26502)`.

### SSH agent from guest (Fase 2e — automated)

**Preferred (automated):** guest unit `ssh-agent-vsock.service` runs
`vsock-ssh-agent-proxy` → listens on `/run/agent-sandbox/ssh-agent.sock` and
dials AF_VSOCK CID 2 port 26501. Set `SSH_AUTH_SOCK` to that path (image default).

```bash
# In guest (systemd enabled at rootfs build):
systemctl status ssh-agent-vsock
echo "$SSH_AUTH_SOCK"   # /run/agent-sandbox/ssh-agent.sock
ssh-add -l              # talks to host agent via vsock
```

**Why vsock (not virtiofs):** matches existing `--host-vsock` architecture; no CH
fs/virtiofs config per sandbox. See [`docs/why-2e-ssh-guest-mount.md`](../docs/why-2e-ssh-guest-mount.md).

```bash
# Option A — manual socat fallback:
socat UNIX-LISTEN:/run/agent-sandbox/ssh-agent.sock,fork VSOCK-CONNECT:2:26501 &
export SSH_AUTH_SOCK=/run/agent-sandbox/ssh-agent.sock

# Option B — virtiofs / shared dir (ops manual): host creates
#   /run/asp/ssh-agent-{sandboxID}.sock → symlink to --ssh-agent-bridge
# Mount that host dir into the guest (CH fs/virtiofs). Not the automated path.

# Option C — pod-daemon in-process:
pod-daemon --listen vsock --ssh-auth-bridge --ssh-auth-socket /run/agent-sandbox/ssh-agent.sock
```

Lab/dry-run without AF_VSOCK:

```bash
export ASP_SSH_AGENT_UPSTREAM=unix:/run/asp/host-vsock-26501.sock
vsock-ssh-agent-proxy --listen /tmp/guest-ssh.sock --upstream "$ASP_SSH_AGENT_UPSTREAM"
```

Host: `--host-vsock` (+ `--guest-ssh-agent-auto`, default on with host-vsock / ssh-agent-bridge).

## Dry-run / no KVM

Use unix: `--pod-daemon-sock`, `--ssh-agent-bridge`, `--identity-listen`, and
optionally `--host-vsock --host-vsock-dir=/tmp/asp-hv`.
