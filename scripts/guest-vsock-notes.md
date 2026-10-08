# Guest ↔ host vsock notes (MVP)

## Port map

| Port | Direction | Protocol | Role |
|---|---|---|---|
| **26500** | host → guest | HTTP JSON | pod-daemon (`GET /healthz`, `POST /v1/exec`) |
| **26501** | guest → host | SSH agent | proxy (identities + sign only) to host `SSH_AUTH_SOCK` / FakeAgent |
| **26502** | guest → host | HTTP JSON | identity proxy `POST /v1/tokens/oidc` |

## Host → guest (exec) — Cloud Hypervisor hybrid

CH binds a host Unix muxer (`/run/asp/vsock-{sandboxID}.sock`). Node-agent:

```text
connect(UDS) → WRITE "CONNECT 26500\n" → READ "OK …\n" → HTTP
```

Guest: `pod-daemon --listen vsock --vsock-port 26500` (AF_VSOCK `CID_ANY`).

## Guest → host (SSH + identity) — CID 2 + CH hybrid

With virtio-vsock, the **host/hypervisor CID is 2**. Guest apps dial:

```text
AF_VSOCK connect(cid=2, port=26501)  → SSH agent
AF_VSOCK connect(cid=2, port=26502)  → identity HTTP
```

### Cloud Hypervisor / Firecracker hybrid (production)

CH does **not** deliver those guest dials to host `AF_VSOCK Listen`. Instead the
VMM connects to a host Unix socket named after the muxer path:

```text
muxer:   /run/asp/vsock-{sandboxID}.sock          (CH --vsock socket=…)
guest→host SSH:       …/vsock-{sandboxID}.sock_26501
guest→host identity:  …/vsock-{sandboxID}.sock_26502
```

With `--host-vsock --reconcile`, node-agent `AttachSandbox` listens on those
paths per sandbox (before VMM start) and runs the same SSH/identity handlers.
See [`docs/why-ch-hybrid-guest-host.md`](../docs/why-ch-hybrid-guest-host.md).

### Optional global listeners (lab / non-hybrid VMM)

```bash
node-agent --host-vsock --reconcile ...
# Lab without /dev/vsock (and dry-run):
node-agent --host-vsock --host-vsock-dir=/run/asp ...
# → unix sockets /run/asp/host-vsock-26501.sock and host-vsock-26502.sock
```

The global listeners cannot tell guests apart, so identity there refuses tokens
(403) unless `--insecure-identity-sandbox-header` (lab) trusts the
`X-ASP-Sandbox-ID` header.

Env in guest: `ASP_HOST_CID=2` (default in image).

### Identity from guest (example with socat + curl)

```bash
# In guest (needs socat):
socat TCP-LISTEN:18080,reuseaddr,fork VSOCK-CONNECT:${ASP_HOST_CID:-2}:26502 &
curl -sS -X POST http://127.0.0.1:18080/v1/tokens/oidc \
  -H 'Content-Type: application/json' \
  -d '{"aud":"https://api.example.com"}'
```

Or a tiny Go/Rust client dialing `vsock.Dial(2, 26502)`.

The token is always for the sandbox of the connection: under CH the VMM
connects to that sandbox's `…/vsock-{sandboxID}.sock_26502`. `X-ASP-Sandbox-ID`
is optional; naming another sandbox gets 403 (ADR-0003 § 2).

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

Host: `--host-vsock` (and `--ssh-agent-bridge` for a shared agent).

## Dry-run / no KVM

Use unix: `--pod-daemon-sock`, `--ssh-agent-bridge`, `--identity-listen`, and
optionally `--host-vsock --host-vsock-dir=/tmp/asp-hv`.

## Bare-metal demo: guest SSH agent via hybrid (26501)

On the node, after deploying a node-agent build with hybrid attach:

```bash
# Create sandbox, wait running, then exec into guest:
# (adjust IDs / asp CLI as used on the host)

# 1) Confirm hybrid listeners exist for the sandbox muxer:
ls -l /run/asp/vsock-*.sock_26501 /run/asp/vsock-*.sock_26502

# 2) In guest (via exec): start proxy if unit not already up
vsock-ssh-agent-proxy -listen /run/agent-sandbox/ssh-agent.sock -cid 2 -port 26501 &

# 3) REQUEST_IDENTITIES (type 11) → expect SSH_AGENT_IDENTITIES_ANSWER (type 12)
perl -e '
  use strict; use warnings;
  my $sock = "/run/agent-sandbox/ssh-agent.sock";
  use IO::Socket::UNIX;
  my $s = IO::Socket::UNIX->new(Type => SOCK_STREAM, Peer => $sock)
    or die "dial: $!";
  # length-prefixed SSH agent: REQUEST_IDENTITIES = 11
  my $payload = pack("C", 11);
  print $s pack("N", length($payload)), $payload;
  my $hdr; read($s, $hdr, 4) == 4 or die "hdr";
  my $len = unpack("N", $hdr);
  my $body; read($s, $body, $len) == $len or die "body";
  my $type = unpack("C", substr($body, 0, 1));
  print "GUEST_SSH_AGENT_OK type=$type\n";  # want 12
  exit($type == 12 ? 0 : 1);
'
```

Expected: `GUEST_SSH_AGENT_OK type=12` (empty identities from FakeAgent is fine
when host has no `SSH_AUTH_SOCK`).
