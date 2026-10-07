# MVP smoke — control-plane + node-agent + exec (+ Postgres / mTLS opcional)

Guía rápida del camino **dry-run** (FakeVMM, sin KVM): memoria por defecto, Postgres cuando `ASP_DATABASE_URL` está set, enrollment/mTLS y exec dataplane.

Para **Cloud Hypervisor real en bare-metal/KVM** (sin FakeVMM), ver [`bare-metal-ch.md`](bare-metal-ch.md). Arquitectura: [`architecture.md`](architecture.md). Diagrama: [`diagram.svg`](diagram.svg).

## Precondiciones / resultados / fallos (léelo antes)

| | Detalle |
|---|---|
| **Precondiciones** | Go 1.25+; Rust/Cargo para pod-daemon; puertos libres `8080` (CP) y `9100` (agent); opcional Docker para Postgres. **No** hace falta `/dev/kvm` ni root. |
| **Qué demuestra** | Plano de control + enroll + exec unix + (scripts) identity/egress/reconcile/CLI y reparto entre dos nodos (`smoke-multi-node`). Contrato de APIs y flags. |
| **Qué NO demuestra** | Aislamiento de hipervisor, bypass-proof nft, AF_VSOCK real, TAP/NAT. Eso es bare-metal. |
| **Resultado OK** | `curl /healthz` → `{"status":"ok"}`; exec → `exit_code:0`; smokes exit 0; `asp sandbox run` imprime stdout del guest. |
| **Fallos típicos** | Puerto ocupado; crear sin ningún node-agent con `--reconcile` registrado (503 `no schedulable nodes registered`); un node-agent sin `--reconcile` no recibe sandboxes (409 si lo fijas con `node_id`); pod-daemon caído (exec 5xx/timeout); API key requerida pero no enviada; SoftFail nft/TAP solo avisa en logs. |

Scripts automatizados (preferibles a copiar curls a mano):

```bash
./scripts/smoke-enroll-exec.sh
./scripts/smoke-identity-egress.sh
./scripts/smoke-reconcile.sh
./scripts/smoke-multi-node.sh   # dos nodos dry-run: reparto, cordon, nodo caído, reinicio del agente
./scripts/smoke-asp-cli.sh   # o: make smoke-asp
# make smoke  # enroll-exec, identity-egress, reconcile y multi-node
```

## 0. (Opcional) Postgres local

Desde la raíz del repo:

```bash
docker compose up -d postgres
export ASP_DATABASE_URL='postgres://asp:asp@127.0.0.1:5432/asp?sslmode=disable'
# opcional: API key de bootstrap
export ASP_BOOTSTRAP_API_KEY='dev-bootstrap-key'
# La autenticación está siempre activa. Sin API key ni IdP el CP no arranca; para estos
# smokes sin autenticación (solo laboratorio, solo loopback):
export ASP_INSECURE_OPEN_API=1
```

Sin Docker/`ASP_DATABASE_URL`, el API usa `MemoryStore` (los tests offline también).

## 1. Arrancar el control plane

```bash
cd control-plane
# Enrollment CA: ASP_CA_CERT/ASP_CA_KEY o auto-create en /tmp/asp-dev-ca
export ASP_NODE_BOOTSTRAP_TOKEN='dev-node-bootstrap'
ASP_LISTEN_ADDR=127.0.0.1:8080 go run ./cmd/api
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

Por defecto (`ASP_AUTO_PROVISION=0`) crear necesita un nodo planificable: arranca antes el node-agent con `--reconcile` (§4). Sin ninguno, el create responde **503** `no schedulable nodes registered`. Para probar solo la API sin nodos, arranca el CP con `ASP_AUTO_PROVISION=1`: un stub la pasa a `running` en el nodo ficticio `local-dev`.

```bash
# Create: el planificador elige un nodo con hueco (ver ops-multi-node.md)
curl -s "${AUTH[@]}" -X POST http://127.0.0.1:8080/v1/sandboxes \
  -H 'Content-Type: application/json' \
  -d '{
    "tenant_id": "tenant-demo",
    "image_ref": "debian:bookworm-slim",
    "cpu_millis": 1000,
    "memory_mib": 512
  }'
# → JSON con id, state="requested", node_id="dev-node" (y el reconciler la lleva a running)
# Con ASP_AUTO_PROVISION=1 y sin nodos: state="running", node_id="local-dev"

> **ADR-0007 fases 1–3 (opcional):** en el lab abierto (sin IdP **ni API keys**) puedes enviar `"owner_sub"` / `"owner_email"` en el body y/o `X-ASP-Actor-Sub` en create/exec/destroy. Vacío = lab OK; **smokes existentes no cambian** (`ASP_IDP_REQUIRED` off). Con una API key el actor es la key (`apikey:<prefijo>`), la cabecera se ignora y un `owner_sub` en el body da 403. Con IdP: ver § «Lab JWT IdP (fase 2)» más abajo.


# Fijar un nodo concreto (opcional; 409 si no existe o no admite sandboxes):
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
# El node-agent real envía la capacidad del host (--capacity-cpu, --capacity-mem-mib,
# --max-sandboxes) y accepts_work según --reconcile. 0 = esa dimensión no se limita.

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
  --pod-daemon-sock=/tmp/pod-daemon.sock \
  --reconcile)

# Exec (el planificador coloca la sandbox en dev-node; el reconciler la arranca)
SID=$(curl -s -X POST http://127.0.0.1:8080/v1/sandboxes \
  -H 'Content-Type: application/json' \
  -d '{"tenant_id":"t","image_ref":"img","cpu_millis":1,"memory_mib":1}' \
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

# Integración Postgres (los tests la saltan si no hay DATABASE_URL; el API usa ASP_DATABASE_URL):
(cd control-plane && DATABASE_URL="$ASP_DATABASE_URL" go test ./... -count=1)

# Smoke e2e dry-run:
./scripts/smoke-enroll-exec.sh
./scripts/smoke-identity-egress.sh
./scripts/smoke-reconcile.sh
./scripts/smoke-multi-node.sh
```

## Notas

- Sin `ASP_DATABASE_URL`: persistencia solo en memoria; reiniciar el API borra sandboxes/nodos.
- Con `ASP_DATABASE_URL`: migraciones embebidas `001`–`017` al arrancar (init, enrollment, egress, leases, attestation/fence, cert rotation, multi-user, idle, workspace, local-net, atributos de planificación del nodo, `agent_instance_id`, scope de API keys, tokens de enroll, caducidad del cert de nodo, túnel local-net asignado por el nodo).
- Auth opcional API key en rutas de tenant; `/healthz` y `/v1/nodes/enroll` son públicos respecto a API keys (enroll usa `ASP_NODE_BOOTSTRAP_TOKEN`). Con `ASP_MTLS_STRICT=1` el enroll vive en `ASP_ENROLL_LISTEN` (ver ADR-0005).
- TLS: `ASP_TLS_CERT`/`ASP_TLS_KEY`; client CA con `ASP_CLIENT_CA` (lab: `VerifyClientCertIfGiven` + middleware; prod: `ASP_MTLS_STRICT=1`).
- CA de enrollment: `ASP_CA_CERT`/`ASP_CA_KEY` o auto-create en `/tmp/asp-dev-ca`.
- **Modo dry-run:** node-agent `--dry-run` → FakeVMM; `--pod-daemon-sock` unix. No confundir con bare-metal (omitir `--dry-run`, hybrid vsock, `--tap-auto`, nft `enforce`).

## 6. Egress allowlist + OIDC + SSH agent bridge

Script automatizado:

```bash
./scripts/smoke-identity-egress.sh
```

### Egress

```bash
# Una sandbox sin reglas: el control plane en memoria permite todo, el que usa Postgres lo deniega.
# ASP_EGRESS_DEFAULT_ALLOW=0 lo deniega también en memoria (antes: ASP_EGRESS_DENY_DEFAULT=1).
export ASP_EGRESS_DEFAULT_ALLOW=0

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
  -H "Authorization: Bearer $(cat "$ASP_AGENT_TOKEN_FILE")" \
  -H 'Content-Type: application/json' \
  -d '{"host":"evil.example","egress_allowlist":{"mode":"deny-default","rules":[{"host_pattern":"*.github.com","enabled":true}]}}'
```

`POST /v1/sandboxes/{id}/exec` adjunta el `egress_allowlist` efectivo del tenant en el payload hacia el node-agent.

Defaults:

| Condición | Mode |
|---|---|
| ≥1 regla enabled | `deny-default` |
| 0 reglas + `ASP_EGRESS_DEFAULT_ALLOW=1` (por defecto con el store en memoria) | `allow-all` |
| 0 reglas + `ASP_EGRESS_DEFAULT_ALLOW=0` (por defecto con Postgres) | `deny-default` |

### OIDC

```bash
# Discovery / JWKS (públicos)
curl -s http://127.0.0.1:8080/.well-known/openid-configuration
curl -s http://127.0.0.1:8080/oidc/jwks.json

# Mint (nodo / lab): tenant y sub salen del sandbox en store — no del body
curl -s -X POST http://127.0.0.1:8080/v1/internal/oidc/token \
  -H 'Content-Type: application/json' \
  -d '{"sandbox_id":"SANDBOX_ID","aud":"https://api.example.com","nonce":"n1"}'

# Identity proxy en node-agent (--identity-listen=/tmp/identity.sock --insecure-identity-sandbox-header).
# Este socket no está ligado a una sandbox: la cabecera solo vale con el flag de lab
# (ADR-0003 § 2). El guest real usa su vsock 26502 y no puede nombrar otra sandbox.
curl -s --unix-socket /tmp/identity.sock \
  -X POST http://localhost/v1/tokens/oidc \
  -H 'Content-Type: application/json' \
  -H "X-ASP-Sandbox-ID: SANDBOX_ID" \
  -d '{"aud":"https://api.example.com"}'
```

Clave de firma: `ASP_OIDC_KEY` (PEM path; auto-create) e issuer `ASP_OIDC_ISSUER`.

### SSH agent bridge

```bash
# Sin SSH_AUTH_SOCK → FakeAgent (0 keys). Con sock de host → proxy (solo listar claves y firmar).
(cd node-agent && go run ./cmd/node-agent --dry-run \
  --ssh-agent-bridge=/tmp/asp-ssh-agent.sock ...)

# Guest productivo (Fase 2e): ssh-agent-vsock.service →
#   SSH_AUTH_SOCK=/run/agent-sandbox/ssh-agent.sock  (vsock CID 2:26501)
# Lab sin KVM: ASP_SSH_AGENT_UPSTREAM=unix:/tmp/asp-hv/host-vsock-26501.sock
# Confirm gate opcional: --ssh-agent-confirm + POST /v1/internal/ssh-agent/approve (con el bearer token del agente)
```

Con Postgres, todas las migraciones (`001`–`017`) se aplican al arrancar.

## 7. Reconciler (fase 1e)

Por defecto `ASP_AUTO_PROVISION` está **off**: Create deja el sandbox en `requested`. El node-agent con `--reconcile` hace el ciclo:

1. `GET /v1/nodes/{id}/work` — requested/starting/stopping asignados a ese nodo (el plano de control los coloca al crear)
2. `POST /v1/sandboxes/{id}/claim` `{node_id}` — assign atómico → `starting`
3. FakeVMM / CH `Start` → `POST .../status` `{state:running}`
4. `POST /v1/sandboxes/{id}/stop` → `stopping` → `stopped`; `POST …/start` → `running` (arranque 2); `DELETE` → `deleting` → `deleted`

```bash
export ASP_AUTO_PROVISION=0
./scripts/smoke-reconcile.sh
```

| Variable / flag | Efecto |
|---|---|
| `ASP_AUTO_PROVISION=1` | Stub sync Create→running (smoke antiguo) |
| `--reconcile` / `ASP_RECONCILE=1` | Loop de reconciliación |
| `--reconcile-interval` | Default 2s |

> **Dry-run vs bare-metal:** aquí FakeVMM + unix sock bastan. CH real = multi-socket spawn + hybrid vsock (`--ch-socket-dir`; [`bare-metal-ch.md`](bare-metal-ch.md) §5–§6). Si Create deja el sandbox en `requested` y nunca pasa a `running`, casi seguro falta `--reconcile` en el node-agent (o tienes `ASP_AUTO_PROVISION=0` sin reconciler). Gaps conocidos: SoftFail nft/TAP, attestation software ≠ TPM/SEV (roadmap).


## 8. CLI `asp` (demo lifecycle)

Con el stack dry-run del §4 (CP + node-agent `--reconcile` + pod-daemon):

```bash
make asp
./build/asp sandbox run --control-plane-url=http://127.0.0.1:8080 --cmd 'echo hello'
# o: ./build/asp sandbox run -- echo hello   (--node-id fija un nodo si hace falta)
```

Building blocks: `create`, `get`, `list`, `exec`, `delete`. Por defecto `run` destruye el sandbox; `--keep` lo deja.

Smoke dedicado (arranca CP en `:18081`):

```bash
./scripts/smoke-asp-cli.sh
# o: make smoke-asp
```

Ver [`why-cli-asp.md`](why-cli-asp.md).


## Lab JWT IdP (ADR-0007 fase 2)

Smokes por defecto **no** activan IdP. Para probar validación local:

```bash
# 1) Genera RSA + JWKS estático (ejemplo con openssl + jq; o usa el test helper mental)
#    Arranca un JWKS HTTP local y apunta:
export ASP_IDP_ISSUER="http://127.0.0.1:9999"
export ASP_IDP_AUDIENCE="asp-api"
export ASP_IDP_JWKS_URL="http://127.0.0.1:9999/jwks"
# ASP_IDP_REQUIRED=0  → JWT opcional (si viene Bearer JWT se valida)
# ASP_IDP_REQUIRED=1  → create/list/get/exec/destroy/events exigen JWT
# Fase 3 RBAC (solo con JWT): groups asp-admin|asp-operator|asp-viewer
# export ASP_IDP_ROLE_CLAIM=groups
# export ASP_IDP_ROLE_PREFIX=asp-

# 2) Create con token válido → owner_sub = claim sub; email del claim si existe
curl -s -X POST http://127.0.0.1:8080/v1/sandboxes \
  -H "Authorization: Bearer ${ASP_ID_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{"tenant_id":"tenant-demo","image_ref":"debian:bookworm-slim","cpu_millis":1000,"memory_mib":512}'

# 3) JWT falso / firma mala → 401
# 4) Body owner_sub distinto del sub del token → 403
# 5) Rutas de nodo (enroll / heartbeat / work / claim) **no** piden JWT humano
```

Tests automatizados (RSA + JWKS estático): `go test ./internal/authn/idp/ ./internal/api/ -run IdP` en `control-plane`.
