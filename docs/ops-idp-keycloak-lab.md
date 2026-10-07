# Ops — Keycloak lab IdP (realm `asp`) cableado al control-plane

Guía operativa del **lab** en ncc1701d: cómo el CP valida Bearer JWT de Keycloak **sin** meter secretos en git.  
Diseño: [ADR-0007](adr/0007-multi-user-identity.md) · narrativa [why-multi-user-identity.md](why-multi-user-identity.md).

## Por qué

Las fases 3u.1–3u.5 del código ya saben validar JWT, mapear roles y exigir token. En lab hacía falta un IdP **real** (no mocks en test) para:

1. Probar el camino `Authorization: Bearer <access_token>` de punta a punta.
2. Fijar un contrato de claims (`iss` / `aud` / `groups`) que luego se traduzca a Entra/Okta.
3. Separar **secretos del host** del repo (client secret, password de usuario de prueba).

Sin este cableado, “IdP listo en código” sigue siendo teórico: el binary no ve JWKS ni `ASP_IDP_REQUIRED=1`.

## Qué ganamos

- Realm dedicado `asp` en `https://auth.luisgf.es/realms/asp` (issuer público, discovery/JWKS).
- Cliente confidencial `asp-api` (`aud` = `asp-api`) alineado con `ASP_IDP_AUDIENCE`.
- Grupos de lab: `asp-admin`, `asp-operator`, `asp-user`, `asp-viewer`, más `sandbox:destroy-any` y `sandbox:exec-any` para que un operator haga destroy o exec en sandboxes ajenas. `asp-user` es el grupo de las personas y agentes que solo usan sandboxes: crean las suyas y no ven las de otros.
- CP en loopback `127.0.0.1:18112` con IdP **required**; `/healthz` público; rutas user-facing → **401** sin Bearer.
- Secretos solo en el host (`~/.secrets/…`); plantilla systemd y scripts en git **sin** passwords.
- Usuario de prueba `asp-lab` para password-grant / smokes manuales (no es cuenta de producción).

## Qué no ganamos (límites honestos)

| Límite | Realidad |
|---|---|
| No es Entra/Okta | Keycloak lab; el mapeo de grupos corporativos (`ASP_IDP_ROLE_MAP`) aún no está cableado a AD. |
| Sin `tenant_memberships` | El rol sale del JWT; no hay tabla SQL de membership por tenant. |
| Store del CP lab | Postgres desde 2026-10-07 (contenedor `asp-postgres`; ver abajo): audit, dueños y sandboxes **sí** persisten entre reinicios. Con el store en memoria no lo harían. |
| Puerto 18112 solo loopback | No hay TLS ni reverse-proxy delante del CP lab; acceso remoto = SSH tunnel / bastion. |
| Admin Keycloak en edge | Históricamente ADR infra pedía tunnel-only; **hoy** nginx de `auth.luisgf.es` hace `proxy_pass` de `location /` (incluye `/admin`) a `127.0.0.1:8081`. Postura segura recomendada: administrar por tunnel y **re-bloquear** `/admin` en edge cuando se pueda. |
| Password grant | Útil para lab/CI scripts; OAuth corporativo real suele ser auth code / device / client credentials — no depender de ROPC en prod. |
| Client secret en host | Quien lea `~/.secrets/asp-keycloak-lab.txt` puede impersonar el cliente; modo 600 + usuario `ubuntu` only. |
| SSH multi-user | Este doc no materializa socks por `owner_sub`; ver fase 3u.4. |

---

## Piezas del lab

```text
Internet / operadores
        │
        ▼
 https://auth.luisgf.es  ──nginx──►  Keycloak :8081 (docker infra-keycloak-1)
        │                              realm asp, client asp-api
        │ discovery + JWKS (público)
        ▼
 ~/.secrets/asp-idp.env  ──EnvironmentFile──►  asp-control-plane.service
                                               binary ~/src/bots/build/api
                                               LISTEN 127.0.0.1:18112
        ▲
        │ (password grant / tests)
 usuario lab asp-lab + client secret  ←  ~/.secrets/asp-keycloak-lab.txt
```

### Realm / cliente / grupos

| Recurso | Valor lab |
|---|---|
| Issuer | `https://auth.luisgf.es/realms/asp` |
| JWKS | `https://auth.luisgf.es/realms/asp/protocol/openid-connect/certs` |
| Cliente | `asp-api` (audiencia del access token) |
| Grupos / roles claim | claim `groups`; prefijo `asp-` → `asp-admin` / `asp-operator` / `asp-user` / `asp-viewer` |
| Destroy-any | valor de grupo `sandbox:destroy-any` (`ASP_IDP_DESTROY_ANY_GROUP`) |
| Exec-any | valor de grupo `sandbox:exec-any` (`ASP_IDP_EXEC_ANY_GROUP`). Sin él un operator solo hace exec en sus sandboxes |
| Usuario de prueba | `asp-lab` (password **solo** en el fichero de secretos del host) |

**Migrar al rol `asp-user`:** antes, para poder crear sandboxes había que ser `asp-operator`, y operator podía hacer exec en las sandboxes de cualquiera del tenant. Ahora un operator solo hace exec en las suyas, salvo que también tenga `sandbox:exec-any`. En Keycloak:

1. Crear los grupos `asp-user` y `sandbox:exec-any`.
2. Pasar a `asp-user` a las personas y agentes que solo usan sandboxes.
3. Dejar `asp-operator`, y en su caso `sandbox:exec-any`, a quien de verdad opera sobre las sandboxes de otros.

Un operator sin `sandbox:exec-any` recibe 403 al hacer exec en una sandbox ajena.

### Variables `ASP_IDP_*` (no secretas; viven en el env file del host)

| Variable | Valor lab típico | Efecto en CP |
|---|---|---|
| `ASP_IDP_ISSUER` | `https://auth.luisgf.es/realms/asp` | Enciende validador JWT |
| `ASP_IDP_AUDIENCE` | `asp-api` | Exige `aud`. Con `ASP_IDP_REQUIRED=1` es obligatoria: el CP no arranca sin ella (o sin `ASP_IDP_ALLOW_ANY_AUDIENCE=1`). El realm tiene que emitir ese `aud` (mapper de audiencia en el client) |
| `ASP_IDP_JWKS_URL` | `…/protocol/openid-connect/certs` | Evita depender solo de discovery |
| `ASP_IDP_REQUIRED` | `1` | User-facing sin JWT → **401** |
| `ASP_IDP_ROLE_CLAIM` | `groups` | Lee grupos del token |
| `ASP_IDP_ROLE_PREFIX` | `asp-` | `asp-operator` → rol operator |
| `ASP_IDP_DESTROY_ANY_GROUP` | `sandbox:destroy-any` | Operator puede destroy no-propios |
| `ASP_IDP_EXEC_ANY_GROUP` | `sandbox:exec-any` | Operator puede hacer exec en no-propios |
| `ASP_IDP_DEFAULT_TENANT` | `default` | Tenant de los tokens del realm, que no traen claim `tenant_id`. **Obligatorio** desde el aislamiento entre tenants: sin tenant, 401 |

Código: `control-plane/internal/authn/idp` + middleware en `internal/api/auth.go`.

### Secretos en el host (mencionar rutas, **nunca** passwords en git)

| Ruta | Contenido | Uso |
|---|---|---|
| `/home/ubuntu/.secrets/asp-idp.env` | Solo `ASP_IDP_*` (issuer/aud/jwks/flags) | `EnvironmentFile=` del unit / `source` del runner ad-hoc |
| `/home/ubuntu/.secrets/asp-keycloak-lab.txt` | `CLIENT_ID`, `CLIENT_SECRET`, `USER`, `PASSWORD`, issuer/JWKS | Password-grant y notas de ops; **no** lo carga el CP |

Ambos: modo `600`, dueño `ubuntu`. No copiar al repo ni a issues/PRs.

Plantilla conceptual de `asp-idp.env` (valores públicos OK; el fichero real ya existe en el host):

```bash
# ~/.secrets/asp-idp.env — mode 600; do not commit
ASP_IDP_ISSUER=https://auth.luisgf.es/realms/asp
ASP_IDP_AUDIENCE=asp-api
ASP_IDP_JWKS_URL=https://auth.luisgf.es/realms/asp/protocol/openid-connect/certs
ASP_IDP_REQUIRED=1
ASP_IDP_ROLE_CLAIM=groups
ASP_IDP_ROLE_PREFIX=asp-
ASP_IDP_DESTROY_ANY_GROUP=sandbox:destroy-any
ASP_IDP_DEFAULT_TENANT=default   # the realm's tokens carry no tenant_id claim
```

---

## Cómo el CP valida Bearer

1. Cliente envía `Authorization: Bearer <JWT>`.
2. Si el bearer “parece JWT” y hay `ASP_IDP_ISSUER`, el middleware llama a `idp.Validator.Validate`:
   - firma RS256 contra JWKS (cache ~5 min),
   - `iss` / `aud` / `exp`,
   - roles desde `ASP_IDP_ROLE_CLAIM` (+ prefix o `ASP_IDP_ROLE_MAP`).
3. Principal → `owner_sub` / `actor_sub` / RBAC (ADR-0007 fases 2–3).
4. Rutas **node** (enroll/heartbeat/work/…) **no** exigen JWT humano (mTLS / bootstrap).
5. `/healthz`, discovery OIDC del **ASP** y JWKS de mint ASP siguen públicos.

### Modo lab cuando IdP está off

Si `ASP_IDP_ISSUER` vacío → validador nil:

- No hay RBAC IdP.
- Create/exec pueden usar body `owner_sub` / header `X-ASP-Actor-Sub` (smokes dry-run), pero solo si tampoco hay API keys: con una key, el actor es la key y el dueño no se puede nombrar.
- `ASP_IDP_REQUIRED=1` **sin** issuer configurado es configuración inválida/ops error (el unit lab siempre lleva issuer).

Con IdP on + required (este lab): sin token en `GET /v1/sandboxes` → **401**  
`{"error":"missing or invalid idp bearer token"}`.

### Autenticación siempre activa y credencial del nodo (desde 2026-10, #95)

Ninguna ruta que haga algo responde a una petición sin credencial, y el CP no arranca si no hay forma de autenticarse (ninguna API key en el store, sin IdP; `ASP_INSECURE_OPEN_API=1` es la salida explícita de los labs). Los nodos usan su certificado (mTLS) o, por HTTP plano como este lab, una API key de ámbito `platform`. Para este lab, una vez y antes de reiniciar CP y agente:

```bash
# 1. La clave: la crea el CP al arrancar a partir de ASP_BOOTSTRAP_API_KEY (en el env file, modo 600)
head -c 24 /dev/urandom | base64 | sudo install -m 0600 -o ubuntu -g ubuntu /dev/stdin ~/.secrets/asp-node.key
echo "ASP_BOOTSTRAP_API_KEY=$(cat ~/.secrets/asp-node.key)" >> ~/.secrets/asp-idp.env
# 2. El agente la manda: ASP_NODE_API_KEY_FILE=/home/ubuntu/.secrets/asp-node.key (legible por quien corra el agente)
```

Con `ASP_IDP_REQUIRED=1` las rutas de usuario siguen pidiendo token del IdP; la API key solo vale para las de nodo y de plataforma.

---

## Admin UI Keycloak

```bash
# Desde el portátil (recomendado)
ssh -L 8081:127.0.0.1:8081 ubuntu@ns31186228.ip-51-91-118.eu
# Abrir http://127.0.0.1:8081/admin/ (realm asp)
```

Keycloak escucha en el host como `127.0.0.1:8081` (docker publish).  
Nginx edge (`auth.luisgf.es`) hoy reenvía **todo** `/` a ese puerto — incluido admin (override ops 2026-07).  
**Consecuencia:** no asumir “admin cerrado en edge” hasta que se reinstaure un `location ^~ /admin` deny/return. Mientras tanto, endurecer password de admin KC y preferir tunnel.

---

## systemd — `asp-control-plane.service`

Plantilla en repo: [`scripts/systemd/asp-control-plane.service`](../scripts/systemd/asp-control-plane.service).

| Campo | Valor lab ncc1701d |
|---|---|
| Unit | `asp-control-plane.service` |
| Binary | `/home/ubuntu/src/bots/build/api` |
| Listen | `127.0.0.1:18112` (`ASP_LISTEN_ADDR`; la unit instalada hoy dice `LISTEN_ADDR`, el nombre anterior, que sigue valiendo con un aviso) |
| Env file | `/home/ubuntu/.secrets/asp-idp.env` (con `DATABASE_URL`, hoy `ASP_DATABASE_URL`: el nombre anterior sigue valiendo con un aviso) |
| Estado | Postgres: contenedor `asp-postgres` en `127.0.0.1:5433` ([bare-metal §4.1](bare-metal-ch.md#41-postgres-compose)) |
| Drop-ins | `/etc/systemd/system/asp-control-plane.service.d/`: `keys.conf` (claves fuera de `/tmp`), `local-net-dial.conf`, `postgres.conf` (espera a Docker y reintenta sin límite) e `idle.conf` (`ASP_SANDBOX_IDLE_TIMEOUT=2h`) |
| Node-agent | `asp-node-agent.service` (ver abajo), `/usr/local/bin/node-agent` |

**Plantilla y unit instalada.** La plantilla del repo lleva sus ajustes en `/etc/asp/server.yaml` ([`scripts/systemd/lab/server.yaml`](../scripts/systemd/lab/server.yaml): escucha, issuer, reaper de inactividad y rutas de las claves) y deja en el `EnvironmentFile` solo los secretos, que mandan sobre el fichero; su `ExecStart` es `build/api --config /etc/asp/server.yaml`. La unit que corre hoy en ncc1701d es la anterior (líneas `Environment=` y los drop-ins de la tabla) y sigue valiendo: el fichero es una capa más, por debajo del entorno ([el fichero de configuración](how-to/config-file.md)). Pasar a la plantilla es un despliegue que se hace con el OK del operador; `build/api --config /etc/asp/server.yaml --print-config` dice qué valdría cada ajuste antes de hacerlo.

### Instalar / reemplazar el runner ad-hoc

```bash
# 1) Parar CP lab ad-hoc si ocupa 18112
pkill -f '/tmp/asp-idp-lab/api' || true
# o: kill $(cat /tmp/asp-idp-lab/cp.pid)

# 2) Instalar unit (desde clone)
sudo cp ~/src/bots/scripts/systemd/asp-control-plane.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now asp-control-plane.service

# 3) Verificar
systemctl is-active asp-control-plane.service   # active
curl -fsS http://127.0.0.1:18112/healthz        # ok
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18112/v1/sandboxes
# → 401 sin token
```

Runner ad-hoc (sin systemd): [`scripts/run-cp-lab-idp.sh`](../scripts/run-cp-lab-idp.sh) — útil para debug; **no** conviene junto al unit (mismo puerto).

### Actualizar desde claves en `/tmp`

Con `ASP_IDP_REQUIRED=1` el control plane está en modo producción. Si la CA, la clave OIDC o la de atestación están en `/tmp`, no arranca (código 2). Las units instaladas antes de esta comprobación usaban `/tmp` y unos `ASP_OIDC_KEY_PATH`/`ASP_ATTEST_KEY_PATH` que el código ignora. Antes de arrancar un binario nuevo:

```bash
sudo install -d -o ubuntu -g ubuntu -m 0700 /var/lib/asp-control-plane
cp -p /tmp/asp-dev-ca/ca.crt /tmp/asp-dev-ca/ca.key /var/lib/asp-control-plane/
cp -p /tmp/asp-oidc-key.pem /var/lib/asp-control-plane/oidc-key.pem
cp -p /tmp/asp-attest-key.pem /var/lib/asp-control-plane/attest-key.pem
sudo cp ~/src/bots/scripts/systemd/asp-control-plane.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl restart asp-control-plane
```

- **Claves:** copiarlas en vez de dejar que se creen mantiene válidos los certificados de nodo y el `kid` de los tokens.
- **Tenant:** añade también `ASP_IDP_DEFAULT_TENANT=default` al env file (tabla de arriba). Sin esa línea, cada `POST /v1/sandboxes` con un token del realm responde 401 `idp token names no tenant`.
- **Atestación:** el node-agent de este host firma con `ASP_ATTEST_KEY=/var/lib/asp-control-plane/attest-key.pem`.
- **Estado:** desde 2026-10-07 el store es Postgres (`DATABASE_URL` en el env file; con el binario nuevo se llama `ASP_DATABASE_URL`): reiniciar el servicio **conserva** sandboxes, eventos y API keys, y el node-agent sigue registrado. Con el store en memoria (sin `ASP_DATABASE_URL`) reiniciar lo borraba todo y el nodo se registraba de nuevo.
- **Reaper de inactividad:** `idle.conf` lo activa a 2 h. Para la sandbox, no la borra: su disco se conserva y `asp session resume` la trae de vuelta ([ADR-0012](adr/0012-retained-disks.md)).

### systemd — `asp-node-agent.service`

El node-agent de este host corre como unidad persistente (antes, una unidad transitoria de `systemd-run` que no sobrevivía a un reinicio). Plantilla en el repo: [`scripts/systemd/asp-node-agent-lab.service`](../scripts/systemd/asp-node-agent-lab.service): el plano de control está en el mismo host, por HTTP en loopback, sin mTLS ni enroll (la de [`asp-node-agent.service`](../scripts/systemd/asp-node-agent.service) es para nodos de un despliegue de varios servidores).

```bash
sudo install -o root -g root -m 0755 build/node-agent /usr/local/bin/node-agent   # de root: la unidad corre como root
sudo install -d -m 0700 /etc/asp && sudo install -m 0600 scripts/systemd/lab/agent.yaml /etc/asp/agent.yaml   # lo que eran las banderas de ExecStart
sudo cp scripts/systemd/asp-node-agent-lab.service /etc/systemd/system/asp-node-agent.service
sudo systemctl daemon-reload && sudo systemctl enable --now asp-node-agent.service
journalctl -u asp-node-agent -f
```

- **Orden y reintentos.** `After=asp-control-plane.service`, sin `Wants=` a propósito: arrancar o reiniciar el agente no levanta un plano de control que alguien paró, y reiniciar el plano de control no toca al agente ni a sus VMs. Si el plano de control aún no responde (al arrancar el servidor espera a su contenedor de Postgres), el agente sale al no poder registrarse y systemd lo reintenta cada 5 s sin tope (`StartLimitIntervalSec=0`). `RequiresMountsFor=/sandbox` evita crear `/sandbox/disks` en el disco raíz si `/sandbox` no está montado.
- **Actualizar el binario no detiene las VMs** de un agente que ya las deja vivas ([ADR-0014](adr/0014-vms-outlive-the-agent.md)): `sudo install -o root -g root -m 0755 <nuevo> /usr/local/bin/node-agent && sudo systemctl restart asp-node-agent`, y el proceso nuevo adopta las VMs que corren (`journalctl -u asp-node-agent | grep adopted`). **La primera vez** que se pasa a una versión con esto sí las detiene: las VMs que arrancó el agente anterior están atadas a su servicio (`BindsTo=`) y no tienen registro que adoptar; sus sandboxes pasan a `stopped` (`node_agent_restarted`) con su disco, igual que las paradas (`asp session resume` las arranca de nuevo y lo que instalaron fuera de `/workspace` sigue ahí). Esa vez haz `cordon`, drena y entonces reinicia. Con el plano de control ya actualizado ([`ops-multi-node.md`](ops-multi-node.md), orden de actualización).
- **Tras reiniciar el servidor** vuelven solos Docker, `asp-postgres`, el plano de control y el agente; las VMs mueren con el servidor, así que las sesiones que corrían quedan `stopped` (`node_agent_restarted`) y, como las paradas, se pueden reanudar con su disco.
- **Si el agente muere** (`kill -9`), systemd lo reinicia y la limpieza (`--reap-only`) corre antes y respeta las VMs vivas; el plano de control ve un `agent_instance_id` nuevo y el agente le dice qué VMs adoptó. Parar todas las VMs del nodo a mano: `sudo systemctl stop asp-vms.slice`.

### Alternativas y consecuencias

| Alternativa | Pros | Contras |
|---|---|---|
| **systemd + EnvironmentFile** (elegido) | Reinicio, logs journal, un solo puerto documentado | Paths absolutos del host en la plantilla |
| Solo `run-cp-lab-idp.sh` | Rápido, copia binario a `/tmp` | Muere al logout/reboot; fácil olvidar IdP env |
| CP en `:8080` compartido | Un solo listener | Choca con otros servicios lab en 8080; peor aislamiento IdP-required |
| Meter secretos en unit `Environment=` | Simple | Filtra en `systemctl show` / backups; **rechazado** |

---

## Flujo de prueba (password grant) — sin pegar secretos

```bash
# Cargar CLIENT_ID / CLIENT_SECRET / USER / PASSWORD desde el fichero del host
# (ops: set -a; source de un wrapper; no echo de secretos)
TOKEN_URL='https://auth.luisgf.es/realms/asp/protocol/openid-connect/token'

# Obtener access_token (ROPC lab). No registrar la respuesta en tickets.
access=$(curl -fsS -X POST "$TOKEN_URL" \
  -d "grant_type=password" \
  -d "client_id=$CLIENT_ID" \
  -d "client_secret=$CLIENT_SECRET" \
  -d "username=$USER" \
  -d "password=$PASSWORD" | jq -r .access_token)

curl -fsS http://127.0.0.1:18112/v1/sandboxes?tenant_id=default \
  -H "Authorization: Bearer $access"
```

Esperado: **200** (lista, posiblemente vacía) con token válido; **401** sin header.

---

## Checklist ops al tocar el IdP

1. ¿Cambió issuer/aud/JWKS? Actualizar `~/.secrets/asp-idp.env` y `systemctl restart asp-control-plane`.
2. ¿Rotó client secret? Solo `asp-keycloak-lab.txt` + scripts de grant; el CP **no** usa el secret (solo JWKS).
3. ¿Nuevos grupos? Alinear nombres con `ASP_IDP_ROLE_PREFIX` o definir `ASP_IDP_ROLE_MAP`.
4. ¿Promoción a Entra/Okta? Nuevo env file / unit drop-in; no reutilizar password grant ni el usuario `asp-lab`.

## CLI agente (Bearer automático)

El binario `asp` puede obtener/refrescar el token y adjuntar `Authorization`
sin que el agente gestione `curl` al token endpoint:

- Doc: [`ops-asp-agent-runner.md`](ops-asp-agent-runner.md)
- Comandos: `asp auth login|logout|status`; `asp sandbox *` auto-Bearer
- Smoke: [`../scripts/smoke-asp-auth-lab.sh`](../scripts/smoke-asp-auth-lab.sh)

## Referencias

- ADR: [0007-multi-user-identity.md](adr/0007-multi-user-identity.md)
- Por qué / qué ganamos: [why-multi-user-identity.md](why-multi-user-identity.md)
- Roadmap readiness: [roadmap.md](roadmap.md)
- Bare-metal general: [bare-metal-ch.md](bare-metal-ch.md)
- Vars CP: [../control-plane/README.md](../control-plane/README.md)
