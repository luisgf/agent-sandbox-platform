# MVP smoke — control-plane + node-agent + exec (+ Postgres / mTLS opcional)

Guía rápida para el slice actual: memoria por defecto, Postgres cuando `DATABASE_URL` está set, enrollment/mTLS y exec dataplane en dry-run.

Para **Cloud Hypervisor real en bare-metal/KVM** (sin FakeVMM), ver [`bare-metal-ch.md`](bare-metal-ch.md).

## 0. (Opcional) Postgres local

Desde la raíz del repo:

```bash
docker compose up -d postgres
export DATABASE_URL='postgres://asp:asp@127.0.0.1:5432/asp?sslmode=disable'
# opcional: API key de bootstrap
export ASP_BOOTSTRAP_API_KEY='dev-bootstrap-key'
# opcional: forzar auth
# export ASP_REQUIRE_API_KEY=1
```

Sin Docker/`DATABASE_URL`, el API usa `MemoryStore` (los tests offline también).

## 1. Arrancar el control plane

```bash
cd control-plane
# Enrollment CA: ASP_CA_CERT/ASP_CA_KEY o auto-create en /tmp/asp-dev-ca
export ASP_NODE_BOOTSTRAP_TOKEN='dev-node-bootstrap'
LISTEN_ADDR=:8080 go run ./cmd/api
```

Health (siempre público):

```bash
curl -s http://127.0.0.1:8080/healthz
# {"status":"ok"}
```

Si arrancaste con `ASP_BOOTSTRAP_API_KEY`, añade el header en rutas de tenant:

```bash
AUTH=(-H "Authorization: Bearer ${ASP_BOOTSTRAP_API_KEY}")
```

### TLS opcional (lab / corp)

```bash
# Certificado servidor (ej. autofirmado) + CA de cliente = CA de enrollment
export ASP_TLS_CERT=/path/to/server.crt
export ASP_TLS_KEY=/path/to/server.key
export ASP_CLIENT_CA=/tmp/asp-dev-ca/ca.crt   # VerifyClientCertIfGiven
# Enroll sigue sin exigir client cert (solo bootstrap token + server TLS).
# register/heartbeat sí exigen peer cert verificado por middleware.
```

## 2. Crear / listar / obtener sandboxes + events

```bash
# Create (provisioner stub → state=running, node_id=local-dev por defecto)
curl -s "${AUTH[@]}" -X POST http://127.0.0.1:8080/v1/sandboxes \
  -H 'Content-Type: application/json' \
  -d '{
    "tenant_id": "tenant-demo",
    "image_ref": "debian:bookworm-slim",
    "cpu_millis": 1000,
    "memory_mib": 512
  }'
# → JSON con id, state="running", node_id="local-dev"

# Pin a un nodo concreto (dry-run exec):
# "node_id": "dev-node"

# List / Get / Events
curl -s "${AUTH[@]}" 'http://127.0.0.1:8080/v1/sandboxes?tenant_id=tenant-demo'
curl -s "${AUTH[@]}" http://127.0.0.1:8080/v1/sandboxes/SANDBOX_ID
curl -s "${AUTH[@]}" http://127.0.0.1:8080/v1/sandboxes/SANDBOX_ID/events
```

## 3. Enrollment de nodo + register / heartbeat

```bash
# Manual enroll (devuelve PEMs + fingerprint)
curl -s -X POST http://127.0.0.1:8080/v1/nodes/enroll \
  -H "Authorization: Bearer ${ASP_NODE_BOOTSTRAP_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{
    "id": "dev-node",
    "name": "dev-node",
    "agent_endpoint": "http://127.0.0.1:9100",
    "capacity_cpu": 4,
    "capacity_mem_mib": 8192
  }'

# Register / heartbeat (sin mTLS en lab HTTP)
curl -s "${AUTH[@]}" -X POST http://127.0.0.1:8080/v1/nodes/register \
  -H 'Content-Type: application/json' \
  -d '{
    "id": "dev-node",
    "name": "dev-node",
    "endpoint": "http://127.0.0.1:9100",
    "agent_endpoint": "http://127.0.0.1:9100",
    "capacity_cpu": 4,
    "capacity_mem_mib": 8192
  }'
curl -s "${AUTH[@]}" -X POST http://127.0.0.1:8080/v1/nodes/dev-node/heartbeat
curl -s "${AUTH[@]}" http://127.0.0.1:8080/v1/nodes
```

## 4. Exec dataplane (dry-run)

Script automatizado (recomendado):

```bash
./scripts/smoke-enroll-exec.sh
```

Manual:

```bash
# Terminal A — pod-daemon
(cd pod-daemon && cargo run -- --listen unix --unix-socket /tmp/pod-daemon.sock)

# Terminal B — control plane (ver §1)

# Terminal C — node-agent
(cd node-agent && go run ./cmd/node-agent \
  --control-plane-url=http://127.0.0.1:8080 \
  --node-id=dev-node \
  --dry-run \
  --enroll \
  --bootstrap-token="$ASP_NODE_BOOTSTRAP_TOKEN" \
  --cert-dir=/tmp/asp-node-certs \
  --agent-listen=127.0.0.1:9100 \
  --pod-daemon-sock=/tmp/pod-daemon.sock)

# Exec
SID=$(curl -s -X POST http://127.0.0.1:8080/v1/sandboxes \
  -H 'Content-Type: application/json' \
  -d '{"tenant_id":"t","image_ref":"img","cpu_millis":1,"memory_mib":1,"node_id":"dev-node"}' \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')

curl -s -X POST "http://127.0.0.1:8080/v1/sandboxes/${SID}/exec" \
  -H 'Content-Type: application/json' \
  -d '{"cmd":["echo","hello"]}'
# → {"stdout":"hello\n","stderr":"","exit_code":0}
```

Flujo: cliente → control-plane `POST /v1/sandboxes/{id}/exec` → node-agent `POST /v1/internal/exec` (localhost) → pod-daemon unix `POST /v1/exec`.

## 5. Tests

```bash
# Offline (MemoryStore + PKI) — debe pasar sin Postgres ni TLS hardware:
(cd control-plane && go test ./...)
(cd node-agent && go test ./...)
(cd pod-daemon && cargo test && cargo check)

# Integración Postgres (skip si no hay DATABASE_URL):
(cd control-plane && DATABASE_URL="$DATABASE_URL" go test ./... -count=1)

# Smoke e2e dry-run:
./scripts/smoke-enroll-exec.sh
./scripts/smoke-identity-egress.sh
./scripts/smoke-reconcile.sh
```

## Notas

- Sin `DATABASE_URL`: persistencia solo en memoria; reiniciar el API borra sandboxes/nodos.
- Con `DATABASE_URL`: migraciones `001_init.sql` + `002_node_enrollment.sql` + `003_tenant_egress.sql` al arrancar.
- Auth opcional API key en rutas de tenant; `/healthz` y `/v1/nodes/enroll` son públicos para API keys (enroll usa `ASP_NODE_BOOTSTRAP_TOKEN`).
- TLS: `ASP_TLS_CERT`/`ASP_TLS_KEY`; client CA con `ASP_CLIENT_CA` (VerifyClientCertIfGiven + middleware en register/heartbeat).
- CA de enrollment: `ASP_CA_CERT`/`ASP_CA_KEY` o `/tmp/asp-dev-ca`.

## 6. Egress allowlist + OIDC + SSH agent bridge

Script automatizado:

```bash
./scripts/smoke-identity-egress.sh
```

### Egress

```bash
# Harden empty-rules deny (memory-dev otherwise sets ASP_EGRESS_DEFAULT_ALLOW=1):
export ASP_EGRESS_DENY_DEFAULT=1

# Replace tenant allowlist
curl -s -X PUT http://127.0.0.1:8080/v1/tenants/tenant-demo/egress \
  -H 'Content-Type: application/json' \
  -d '{"rules":[
    {"host_pattern":"*.github.com","enabled":true},
    {"host_pattern":"registry.npmjs.org","port":443}
  ]}'

curl -s http://127.0.0.1:8080/v1/tenants/tenant-demo/egress

# CP test helper
curl -s -X POST http://127.0.0.1:8080/v1/tenants/tenant-demo/egress/check \
  -H 'Content-Type: application/json' \
  -d '{"host":"api.github.com"}'
# → {"allowed":true,...}

# Node-agent check (with --egress-enforce → HTTP 403 when denied)
curl -s -X POST http://127.0.0.1:9100/v1/internal/egress-check \
  -H 'Content-Type: application/json' \
  -d '{"host":"evil.example","egress_allowlist":{"mode":"deny-default","rules":[{"host_pattern":"*.github.com","enabled":true}]}}'
```

`POST /v1/sandboxes/{id}/exec` adjunta el `egress_allowlist` efectivo del tenant en el payload hacia el node-agent.

Defaults:

| Condición | Mode |
|---|---|
| ≥1 regla enabled | `deny-default` |
| 0 reglas + `ASP_EGRESS_DEFAULT_ALLOW=1` | `allow-all` (memory-dev auto) |
| 0 reglas + `ASP_EGRESS_DENY_DEFAULT=1` / sin allow | `deny-default` |

### OIDC

```bash
# Discovery / JWKS (públicos)
curl -s http://127.0.0.1:8080/.well-known/openid-configuration
curl -s http://127.0.0.1:8080/oidc/jwks.json

# Mint (nodo / lab): tenant y sub salen del sandbox en store — no del body
curl -s -X POST http://127.0.0.1:8080/v1/internal/oidc/token \
  -H 'Content-Type: application/json' \
  -d '{"sandbox_id":"SANDBOX_ID","aud":"https://api.example.com","nonce":"n1"}'

# Identity proxy en node-agent (--identity-listen=/tmp/identity.sock):
curl -s --unix-socket /tmp/identity.sock \
  -X POST http://localhost/v1/tokens/oidc \
  -H 'Content-Type: application/json' \
  -H "X-ASP-Sandbox-ID: SANDBOX_ID" \
  -d '{"aud":"https://api.example.com"}'
```

Clave de firma: `ASP_OIDC_KEY` (PEM path; auto-create) e issuer `ASP_OIDC_ISSUER`.

### SSH agent bridge

```bash
# Sin SSH_AUTH_SOCK → FakeAgent (0 keys). Con sock de host → byte-pump.
(cd node-agent && go run ./cmd/node-agent --dry-run \
  --ssh-agent-bridge=/tmp/asp-ssh-agent.sock ...)

# En el guest (futuro / docs): symlink tip
# ln -sf /run/host-services/ssh-auth.sock /run/agent-sandbox/ssh-agent.sock
# El bridge MVP vive en el node-agent; pod-daemon solo documenta el path.
```

Migraciones con Postgres: `001` + `002` + `003_tenant_egress.sql`.

## 7. Reconciler (fase 1e)

Por defecto `ASP_AUTO_PROVISION` está **off**: Create deja el sandbox en `requested`. El node-agent con `--reconcile` hace el ciclo:

1. `GET /v1/nodes/{id}/work` — requested/starting/stopping asignados o requested sin asignar
2. `POST /v1/sandboxes/{id}/claim` `{node_id}` — assign atómico → `starting`
3. FakeVMM / CH `Start` → `POST .../status` `{state:running}`
4. `DELETE /v1/sandboxes/{id}` → `stopping` → Stop+Delete → `stopped`

```bash
export ASP_AUTO_PROVISION=0
./scripts/smoke-reconcile.sh
```

| Variable / flag | Efecto |
|---|---|
| `ASP_AUTO_PROVISION=1` | Stub sync Create→running (smoke antiguo) |
| `--reconcile` / `ASP_RECONCILE=1` | Loop de reconciliación |
| `--reconcile-interval` | Default 2s |

> CH real: multi-socket spawn + hybrid vsock exec (`--ch-socket-dir`; ver [`bare-metal-ch.md`](bare-metal-ch.md) §5–§6). Dry-run: FakeVMM + unix sock. Post-MVP: host-vsock identity/SSH, TAP auto, fase 2c/2d (ver roadmap). Gaps: virtiofs auto, TPM/SEV hardware.


## 8. CLI `asp` (demo lifecycle)

Con el stack dry-run del §4 (CP + node-agent `--reconcile` + pod-daemon):

```bash
make asp
./build/asp sandbox run --cp-url=http://127.0.0.1:8080 --node-id=dev-node --cmd 'echo hello'
# o: ./build/asp sandbox run --node-id=dev-node -- echo hello
```

Building blocks: `create`, `get`, `list`, `exec`, `delete`. Por defecto `run` destruye el sandbox; `--keep` lo deja.

Smoke dedicado (arranca CP en `:18081`):

```bash
./scripts/smoke-asp-cli.sh
# o: make smoke-asp
```

Ver [`why-cli-asp.md`](why-cli-asp.md).
