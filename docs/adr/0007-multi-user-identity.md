# ADR-0007: Identidad multi-usuario (humano ↔ sandbox)

- **Estado:** Aceptada
- **Implementación:** hecha, fases 1–5 (schema/audit + IdP JWT + RBAC + SSH scoped MVP + workload `user_sub`/`act`)
- **Fecha:** 2026-10
- **Relacionados:** [0003](0003-identity.md) (SSH/OIDC workload), [0005](0005-fase-2d-hardening.md) (SSH confirm), [0008](0008-network-flow-attribution.md) (flujos de red → `owner_sub`, evaluación), [`../architecture.md`](../architecture.md), [`../roadmap.md`](../roadmap.md) (§ readiness corporativa)
- **Extiende:** el modelo de “identidad” de ADR-0003 (tenant + sandbox + nodo) con **sujeto humano** del IdP corporativo

## Contexto

Hoy la plataforma es **multi-tenant** (`tenant_id`) y multi-sandbox, pero la identidad operativa **no es humana**:

| Capa | Qué identifica hoy | Quién autentica |
|---|---|---|
| API cliente | API key → `tenant_id` | Bearer obligatorio (desde 2026-10, #95; antes opcional con `ASP_REQUIRE_API_KEY`) |
| Sandbox | `sandbox_id` + `tenant_id` (+ `node_id`) | Store / reconciler |
| Guest OIDC | `sub = sandbox/{id}`, claims `tenant_id`/`sandbox_id` **inyectados por el host** | CP firma; guest solo elige `aud` |
| SSH | Un bridge host-held por node-agent (`SSH_AUTH_SOCK` o `FakeAgent`) | Confirm gate opcional, **global** al proceso |
| Audit | `sandbox_events.actor` = strings como `"api"`, `"node-agent"`, `"lease"` | Sin `sub` humano |

Eso basta para lab y para un solo operador por tenant. **No basta** cuando varios humanos (o agentes supervisores) del mismo tenant crean, ejecutan y destruyen sandboxes, firman SSH o minten tokens hacia APIs corporativas.

Amenazas / requisitos corporativos que el gap deja abiertos:

1. **Atribución:** “quién pidió este `exec` / destroy / SignRequest” no se puede responder con un humano.
2. **Separación dentro del tenant:** la API key del tenant es capacidad compartida; cualquiera con la key es dueño de todo.
3. **Workload → IdP humano:** el JWT de workload actual no lleva `user_sub` ni cadena de actor (`act`); los consumidores Entra/Okta no pueden ligar la llamada al empleado.
4. **SSH multi-usuario inseguro:** un solo `SSH_AUTH_SOCK` de host bridged a todos los guests del nodo = cualquier sandbox puede pedir firmas con las claves del operador (el confirm gate mitiga silencio, no multi-tenancy humana).
5. El **orden de readiness corporativa** ya lista “OIDC IdP (Entra/Okta)” como gap explícito frente a solo API keys de CP.

Restricciones honestas del código actual (límites reales, no wishful):

- `store.Sandbox` no tiene `owner_sub` / `owner_email` (`models.go`).
- `AuthMiddleware` solo conoce API keys + mTLS de nodo (`api/auth.go`); no valida JWT de IdP.
- `oidc.Signer.Mint` fija `sub = sandbox/{id}`; fase 5 añade `user_sub`/`act` desde `owner_sub` (MintIdentity).
- Identity proxy (`node-agent/internal/identity`) **ignora** overrides del guest (`sandbox_id`, `user_sub`, `act`); el CP inyecta `user_sub` desde store.
- SSH bridge (`sshagent.Bridge`) es proceso-global; FakeAgent en lab; confirm es on/off por flag, no por usuario.
- No hay tablas de membership/roles; `api_keys` es por tenant, sin RBAC fino.

## Decisión

**Vincular el sujeto humano del IdP corporativo al ciclo de vida del sandbox**, sin romper el modelo “secretos fuera del guest” de ADR-0003.

### Modelo de datos (campos)

Sobre `sandboxes` (y el struct `store.Sandbox`):

| Campo | Tipo | Obligatorio | Semántica |
|---|---|---|---|
| `owner_sub` | text | **sí** (tras fase 1+ con IdP) | `sub` estable del JWT del IdP (Entra `oid`/`sub`, Okta `sub`, …) del creador |
| `owner_email` | text | no | Claim `email` / `preferred_username` para ops/UI; **no** es la clave de authz |
| (existente) `tenant_id` | text | sí | Aislamiento organizacional |
| (existente) `id` | text | sí | Sandbox |

Sobre **acciones** (create / exec / destroy / approve SSH / mint OIDC):

| Campo | Dónde | Semántica |
|---|---|---|
| `actor_sub` | request context + `sandbox_events` | Humano (o principal de servicio) que **ejecuta** la acción ahora |
| `actor` (legado) | `sandbox_events.actor` | Se evoluciona: de `"api"` → prefijo estable p.ej. `user:{sub}` / `apikey:{id}` / `node:{id}` / `system:lease` |

Reglas:

- **Create** exige JWT de usuario (o, en transición, API key *bootstrap* de servicio documentada); el CP escribe `owner_sub` desde el token, **nunca** desde el body del cliente.
- **Exec / destroy / list filtrado** autorizan: `actor_sub == owner_sub` **o** rol suficiente en el tenant (ver matriz).
- El guest **sigue sin elegir** `tenant_id`, `sandbox_id`, `owner_sub` ni `user_sub` en tokens de workload.

### Autenticación (CP)

1. El cliente envía `Authorization: Bearer <JWT IdP>` (Entra ID / Okta / OIDC genérico).
2. CP valida: firma (JWKS del IdP), `iss`, `aud` (audiencia de la API ASP), `exp`, y opcionalmente `tid`/tenant claim.
3. Mapeo configurado:
   - `sub` (o `oid` en Entra si se elige) → identidad estable.
   - `groups` / App Roles / claim custom → **membership** de `tenant_id` + **rol** (`admin` | `operator` | `viewer`).
4. Las **API keys** permanecen para automatización de nodo/lab y para principals de servicio; se etiquetan en audit como `apikey:{id}` y se les asigna rol explícito (no “dios silencioso”).
5. Rutas de node-agent siguen en **mTLS** (ADR-0005); no usan JWT humano.

Config ops prevista (nombres ilustrativos): `ASP_IDP_ISSUER`, `ASP_IDP_AUDIENCE`, `ASP_IDP_JWKS_URL`, `ASP_IDP_GROUP_TENANT_MAP` / tabla `tenant_memberships`.

### Autorización — matriz

| Acción | owner (`actor_sub == owner_sub`) | `admin` tenant | `operator` | `user` | `viewer` | API key servicio (rol) | Node mTLS |
|---|---|---|---|---|---|---|---|
| Create sandbox | — (pasa a ser owner) | sí | sí | sí | no | según rol de la key | no |
| List (propios) | sí | sí (todos) | sí (todos o filtro) | solo propios | sí (todos, RO) | según rol | no |
| Get | sí | sí | sí | solo propios | sí | según rol | claim/status sí |
| Exec | sí | sí | solo propios salvo `sandbox:exec-any` | solo propios | no | operator+ | proxy interno |
| Destroy / stop | sí | sí | solo propios salvo `sandbox:destroy-any` | solo propios | no | operator+/admin | status |
| Approve SSH SignRequest | sí (del sandbox) o admin | sí | opcional (flag) | — | no | no por defecto | endpoint local host |
| Mint OIDC workload | vía guest→proxy; sujeto = owner del sandbox | — | — | — | — | — | mint interno |
| Egress / tenant policy | no | sí | no | no | no | admin key | no |

Política por defecto para `operator`: destroy y exec **solo sobre sandboxes propios** salvo los grupos `sandbox:destroy-any` y `sandbox:exec-any`. El exec en una sandbox ajena lee su workspace y pide tokens de workload a nombre de esa sandbox, así que es un permiso explícito igual que el destroy. `user` (2026-10) es el rol de quien solo usa sandboxes: crea las suyas y no ve ni toca las de otros. Con `viewer` y `user` a la vez el rol es `operator`, su unión.

**Aislamiento entre tenants (hecho, 2026-10).** Todo llamante con credencial queda confinado a un tenant y los handlers lo comprueban:

- **JWT del IdP:** el tenant sale del claim `ASP_IDP_TENANT_CLAIM` (por defecto `tenant_id`; un string o un array con un solo valor) o, si no viene, de `ASP_IDP_DEFAULT_TENANT` (despliegues de un solo tenant). Un token sin tenant, o con varios, recibe 401. Los roles (`admin`, `operator`, `viewer`) valen **dentro** de ese tenant.
- **API keys:** columna `scope` (migración 014). Una key `tenant` actúa solo en su `tenant_id`; una key `platform` ve todos los tenants (herramientas de operación). La key de `ASP_BOOTSTRAP_API_KEY` es `platform` en el tenant `default` salvo `ASP_BOOTSTRAP_API_KEY_SCOPE=tenant` / `ASP_BOOTSTRAP_API_KEY_TENANT`.
- **Comprobaciones:** get, events, attestation, exec, exec/stdin, destroy, local-net y el mint OIDC con credencial de usuario responden **404** para un sandbox de otro tenant (no se distingue de uno inexistente); create con otro `tenant_id`, list filtrando por otro tenant y las rutas de egress de otro tenant responden **403**. Un create sin `tenant_id` va al tenant del llamante (o a `ASP_DEFAULT_TENANT`, `default`, si el llamante ve todos). El list de un llamante confinado devuelve solo su tenant.
- **Sin cambios:** el lab abierto (sin keys ni IdP) no tiene tenants; las rutas de nodo (mTLS) se autorizan por identidad de nodo, no por tenant.

**Elección fase 3 (list):** operator y viewer ven **tenant-wide** (todos los sandboxes del `tenant_id` consultado), no filtro a propios. ADR permitía «todos o filtro» para operator; elegimos **todos** para que un operator pueda descubrir sandboxes del tenant sin ser admin. El aislamiento fino queda en **exec/destroy** (owner o rol). Viewer es RO sobre esa misma visibilidad.


### Flujos

#### 1) Create sandbox

```text
Cliente (humano / IDE agente)
  → Authorization: Bearer <JWT IdP>
  → POST /v1/sandboxes { image_ref, resources, … }   # sin owner_sub en body
       CP: ValidateJWT → map sub+groups → (tenant_id, role)
       CP: authorize create (operator|admin)
       CP: INSERT sandbox (tenant_id, owner_sub=sub, owner_email=…)
       CP: EmitEvent(actor_sub=sub, event=created)
       → reconciler / node-agent (mTLS) como hoy
```

#### 2) Exec

```text
Cliente Bearer JWT
  → POST /v1/sandboxes/{id}/exec
       CP: actor_sub from JWT
       CP: load sandbox; require tenant match + (owner | role≥operator)
       CP: EmitEvent(actor_sub, event=exec, payload={cmd hash / argv meta})
       → node-agent exec proxy → pod-daemon (vsock 26500)  # sin cambio de dataplane
```

#### 3) Mint OIDC de workload (extiende ADR-0003)

```text
guest POST /v1/tokens/oidc {"aud":"…"}     # solo aud (+nonce)
  → identity proxy 26502 (inyecta sandbox_id; ignora overrides)
  → CP POST /v1/internal/oidc/token (mTLS nodo)
       CP lee sandbox.owner_sub del store
       Mint claims:
         sub        = sandbox/{sandbox_id}          # estable para workload
         tenant_id, sandbox_id                      # server-side
         user_sub   = owner_sub                     # humano dueño
         act        = { "sub": user_sub }           # RFC 8693-style actor (opcional pero útil)
         + x_asp_attestation si Fase 2c lo exige
```

El guest **no** puede pedir un `user_sub` distinto del owner del sandbox. Qué claim lo decide quién:

| Claim | Origen | ¿Lo puede elegir el guest? |
|---|---|---|
| `sub` | `sandbox/{id}` | no |
| `tenant_id`, `sandbox_id` | el store | no |
| `user_sub` | `owner_sub` de la sandbox | **no** (se ignora) |
| `act.sub` | el mismo `user_sub` (el dueño) | **no** (se ignora) |
| `aud` | el cuerpo del guest | sí: es el único campo que elige |

**Límite honesto:** el mint del guest no lleva el `actor_sub` de la acción en curso (el guest no es autoridad): `act` refleja al **dueño** de la sandbox. Los consumidores de Entra u Okta confían en el JWKS de ASP (`ASP_OIDC_*`), no en el del IdP humano. Sin `owner_sub` persistido (laboratorio, sandboxes antiguas) el token no lleva cadena humana.

#### 4) SSH agent — opciones de scoping (decidir en fase 4 del rollout)

El bridge global actual es **inseguro para multi-usuario real**. Tres opciones, de más fuerte a más pragmática:

| Opción | Mecánica | Pros | Contras |
|---|---|---|---|
| **A. Sock por usuario/sesión** | node-agent mantiene `SSH_AUTH_SOCK` (o agent) por `owner_sub` / session id; AttachSandbox enlaza el puerto 26501 de *ese* sandbox al sock del owner | Aislamiento real de claves | Ops: cómo materializar keys por usuario (sshd agent forwarding, vault, 1Password, …) |
| **B. Confirm gate default-on + approve atado a actor** | `--ssh-agent-confirm` default en multi-user; `POST …/approve` exige JWT y comprueba que el actor es owner/admin del sandbox que originó el SignRequest | Reusa código de ADR-0005; auditable | Sigue siendo *una* keyring host si el sock es compartido; solo añade fricción + audit |
| **C. Session agent** | Al create/attach, el CP/node-agent lanza un `ssh-agent` efímero, carga solo las keys de la sesión (via SSH agent forwarding del cliente o secret broker), lo destruye al stop | Blindaje temporal; no reutiliza el agent del operador del host | Más piezas; UX de carga de keys |

**Decisión de diseño:** adoptar **B como default obligatorio** en cuanto haya >1 usuario humano por nodo, y **A (template/path por owner_sub)** como MVP de fase 4. **C** (spawn de agent efímero) queda fuera de alcance de esta fase. Documentar explícitamente: *sin template, bridge global + un solo `SSH_AUTH_SOCK` de operador = no multi-user-safe*.

#### Fase 4 — implementación MVP (2026-10)

| Pieza | Comportamiento |
|---|---|
| `sshagent.Registry` | `Bind(sandboxID, owner_sub)` → path; `Lookup` / `Scoped` |
| Flag / env | `ASP_SSH_AGENT_SOCK_TEMPLATE=/run/asp/ssh-agents/{owner_sub}.sock` |
| `hostvsock.AttachSandbox` | `sshagent.ServeConn` con HostSock del registry y el id de la sandbox (no env fallback; aprobaciones de esa sandbox) |
| Symlink | `/run/asp/ssh-agent-{id}.sock` → path resuelto (virtiofs docs) |
| Confirm | Default-on si `ASP_MULTI_USER=1` (entonces también `ASP_IDP_REQUIRED=1`) o template set; `ASP_SSH_AGENT_CONFIRM=0` fuerza off (y la bandera `--ssh-agent-confirm` manda sobre la variable) |
| Approve | `POST …/ssh-agent/approve` exige `sandbox_id` (la aprobación solo desbloquea una firma de esa sandbox) y acepta `actor_sub` / `X-ASP-Actor-Sub` para el audit log |
| Fallback | Path ausente o bind vacío → **FakeAgent** (sin claves) |

**Límite honesto:** ASP no crea ni carga claves en el agent del usuario; ops debe materializar el UDS (ssh-agent forwarding, vault, 1Password, …) en la ruta del template.

## Alternativas consideradas

| Alternativa | Pros | Contras | Decisión |
|---|---|---|---|
| **Solo API keys** (status quo) | Simple; ya implementado | Sin humano; keys compartidas; no entra en IdP corporativo | Insuficiente; se conserva como principal de servicio |
| **Solo tenant-level** (sin owner) | RBAC grueso fácil | No atribuye create/exec; dos operators se pisan | Rechazada como modelo final; tenant sigue siendo frontera org |
| **SPIFFE/SPIRE para humanos** | Identidad workload fuerte | No sustituye SSO humano; ops pesada; humanos viven en OIDC IdP | Futuro posible para *workloads*; humanos = OIDC IdP |
| **Mapear UID Linux guest ↔ humano** | Familiar en multi-user UNIX | Guest comprometido forja UID; no hay PAM humano en microVM efímera; no liga a Entra | Rechazada como authz; UID guest **≠** sujeto humano |
| **Impersonation solo en el orquestador externo** | ASP sigue tonto | Pierde audit en CP; cada cliente reimplementa | Rechazada; el CP debe ser fuente de verdad de `owner_sub`/`actor_sub` |

## Consecuencias

### Positivas

- Atribución humana en create/exec/destroy/approve y en tokens de workload (`user_sub` / `act`).
- Mismo tenant, varios usuarios, sin compartir una sola API key como identidad.
- Encaja con readiness corporativa (Entra/Okta) sin abandonar API keys de servicio.
- Preserva ADR-0003: guest no elige claims de tenancy ni de usuario.
- Audit usable en SIEM: `tenant_id` + `sandbox_id` + `actor_sub` en cada evento.

### Negativas / coste

- Nueva dependencia ops: JWKS del IdP, mapeo grupos→tenant/rol, rotación de audiencia.
- Migración de sandboxes existentes sin `owner_sub` (backfill `system:legacy` o exigir recreate).
- CLI `asp` y smokes deben aprender Bearer JWT (además de API key).
- SSH: trabajo no trivial; el confirm gate solo no cierra el threat model multi-user.

### Límites honestos (no-goals de este ADR)

- **No** hay equivalencia guest Linux UID ↔ humano.
- **No** prometemos que el bridge SSH global (sin template) sea seguro con varios usuarios; fase 4 aporta template/registry + confirm default-on, no un spawner de agents.
- **No** implementamos IdP embebido; solo validamos tokens de IdPs externos.
- **No** sustituimos mTLS de nodo por JWT humano.
- Attestation sigue siendo software-signed salvo plug-in futuro (ADR-0003 / 2c).
- Fase 1 solo schema/audit: **no** hay authz IdP ni JWT todavía; el rollout sigue por fases abajo.

## Esquema de implementación (mapeo a paquetes existentes)

| Pieza | Ruta prevista | Cambio |
|---|---|---|
| Modelo sandbox | `control-plane/internal/store/models.go` | `OwnerSub`, `OwnerEmail *string` |
| Migración | `control-plane/migrations/007_multi_user_identity.sql` | columnas + índice `(tenant_id, owner_sub)`; opcional `tenant_memberships(sub, tenant_id, role)` |
| Create/List | `store/memory.go`, `store/postgres.go` | persistir owner; filtrar list |
| Eventos | `SandboxEvent` / `EmitEventInput` | `ActorSub` (campo nuevo o convención en `actor` + payload) |
| Authn IdP | `control-plane/internal/api/auth.go` (+ paquete nuevo `internal/authn/idp`) | validar JWT; context `UserPrincipal{Sub,Email,TenantID,Role}` |
| Authz | `control-plane/internal/api/handlers.go` | guards create/exec/destroy/list |
| OIDC workload | `control-plane/internal/oidc/signer.go` | `Mint` acepta `userSub`; claims `user_sub`, `act` |
| Identity proxy | `node-agent/internal/identity/` | sin confiar en guest; CP aporta user_sub desde store |
| SSH | `node-agent/internal/sshagent/` (`confirm.go`, `bridge.go`) | approve con actor; más adelante sock/sesión por owner |
| Host vsock | `node-agent/internal/hostvsock/` | AttachSandbox sigue; el *backend* del 26501 pasa a ser scoped |
| CLI | `cli/internal/client` | `ASP_ID_TOKEN` / `--id-token` además de API key |
| Docs/smokes | `scripts/smoke-*.sh`, roadmap | fase “3u” / readiness |

## Rollout por fases

| Fase | Qué | Criterio de salida | Código tocado (previsto) |
|---|---|---|---|
| **(1) Schema + audit** | `owner_sub` / `owner_email`; eventos con `actor_sub` (API key → placeholder `apikey:…` o header de lab) | ✅ **Hecho:** migración `007_multi_user_identity.sql`; Create acepta `owner_sub`/`owner_email` + `X-ASP-Actor-Sub` (lab: vacío OK); create/exec/destroy auditan `actor_sub` | `store`, migrations, `api/handlers` |
| **(2) IdP JWT en CP API** | Validar Bearer JWT (`ASP_IDP_*`); create/actor desde token; `ASP_IDP_REQUIRED=1` exige JWT en rutas user-facing | ✅ **Hecho:** JWKS/discovery; iss/aud/exp/sig; lab default off | `internal/authn/idp`, `api/auth.go`, handlers |
| **(3) RBAC** | Roles admin/operator/viewer desde claims IdP; matriz de arriba | ✅ **Hecho:** authz en create/list/get/exec/destroy/egress; list tenant-wide | `idp` role map + `api/authz` + handlers |
| **(4) SSH por sesión / confirm default-on** | Confirm default en perfiles multi-user; approve con `actor_sub`; sock por sandbox vía template (opción A pragmática) | ✅ **Hecho (MVP):** `ASP_SSH_AGENT_SOCK_TEMPLATE`; registry sandbox→sock; proxy por sandbox; confirm default-on con `ASP_MULTI_USER`/`ASP_IDP_REQUIRED`/template; approve audita `actor_sub` | `sshagent.Registry`, `hostvsock.AttachSandbox`, reconciler, execproxy |
| **(5) Workload OIDC + `user_sub`** | Mint incluye `user_sub`/`act` desde `owner_sub` | ✅ **Hecho:** `MintIdentity` / `MintWithAttestation` emiten `user_sub`+`act` desde store; guest/body overrides ignorados; lab sin owner omite claims | `oidc.Signer`, `MintOIDCToken`, identity proxy |

Orden intencional: **no** mintir `user_sub` antes de tener owner real en store (1→2→5); **no** declarar SSH multi-user-ready solo con (2).


## Estado de implementación

| Fase | Estado |
|---|---|
| **1 Schema + audit** | **Hecho** (2026-10): `owner_sub`/`owner_email` en sandbox; `actor_sub` en `sandbox_events`; Create/Get/List exponen owner; lab acepta body/`X-ASP-Actor-Sub` (vacío OK). |
| **2 IdP JWT** | **Hecho** (2026-10): `ASP_IDP_ISSUER` / `ASP_IDP_AUDIENCE` / `ASP_IDP_JWKS_URL` (o discovery) / `ASP_IDP_REQUIRED`; valida RS256 + iss/aud/exp (`exp` obligatorio salvo `ASP_IDP_REQUIRE_EXP=0`; JWKS refrescado cada 5 min y, ante `kid` desconocido, como mucho cada 30 s); `owner_sub`/`actor_sub` desde token; rechazo de `owner_sub` forjado; rutas node mTLS sin JWT humano. |
| **3 RBAC** | **Hecho** (2026-10): roles `admin`/`operator`/`user`/`viewer` desde `ASP_IDP_ROLE_CLAIM` + `ASP_IDP_ROLE_MAP` o prefijo `asp-*`; destroy operator = propios salvo `sandbox:destroy-any`; list tenant-wide para admin/operator/viewer; IdP off = sin RBAC (lab). |
| **4 SSH scoped** | **Hecho (MVP 2026-10):** registry `sandboxID→unix path`; `ASP_SSH_AGENT_SOCK_TEMPLATE` (`{owner_sub}`/`{sandbox_id}`); hybrid con upstream por sandbox (sin fallback a `SSH_AUTH_SOCK` global); symlink `ssh-agent-{id}.sock` → path resuelto; confirm default-on en multi-user; approve registra `actor_sub`. **No** spawnea ssh-agent por usuario (ops provisiona socks/keys). |
| **5 Workload `user_sub`** | **Hecho** (2026-10): `user_sub`/`act` desde `sandbox.owner_sub`; guest `user_sub`/`act` ignorados en proxy y CP; lab sin owner sigue mintando sin claims humanos. |

**Límite honesto fase 1–5:** sin IdP configurado (lab), el cliente *puede* enviar `owner_sub` en el body y `X-ASP-Actor-Sub`; **no hay RBAC**. Con Bearer JWT IdP presente (o `ASP_IDP_REQUIRED=1`), el CP toma `sub` del token, mapea rol desde groups/roles claims, y **aplica la matriz**. Tabla `tenant_memberships` aún no. SSH fase 4 es **path injection / template** — no un gestor de agentes; sin template el bridge legacy (`SSH_AUTH_SOCK` global) sigue existiendo y **no** es multi-user-safe. Fase 5: `user_sub`/`act` solo si hay `owner_sub` en store; el mint **no** lee `actor_sub` de la petición guest (el guest no es autoridad); `act.sub` = `user_sub` (= owner) salvo que un path confiable futuro pase `actorSub` a `MintIdentity`. Un Keycloak real cableado en el laboratorio (ver § ops lab). Gaps residuales: IdP corporativo Entra/Okta, tabla `tenant_memberships`, materializar socks SSH por usuario.

## Ops lab — Keycloak realm `asp`

El cableado del laboratorio de los mantenedores (el realm, el cliente, los grupos, dónde están los secretos) está en [`../lab/idp-keycloak.md`](../lab/idp-keycloak.md); cómo conectar un IdP propio, con la matriz de roles, en [`../how-to/idp.md`](../how-to/idp.md).

**Límite honesto residual ops:** lab ≠ Entra/Okta; sin `tenant_memberships`; el password grant es solo para pruebas.

## Enmiendas

- **2026-10 (#101):** sin token del IdP, el dueño y el actor ya no los elige el cliente. Con una API key el actor es la key (`apikey:<prefijo>`), `owner_sub` / `owner_email` en el cuerpo dan 403 y la sandbox no tiene dueño, así que sus tokens de workload no llevan `user_sub`. La cabecera `X-ASP-Actor-Sub` y los campos del cuerpo solo valen en el lab abierto (sin IdP ni API keys).

## Referencias cruzadas

- Identidad guest SSH/OIDC: [0003](0003-identity.md)
- SSH confirm: [0005](0005-fase-2d-hardening.md)
- Roadmap readiness: [`../roadmap.md`](../roadmap.md)
- Arquitectura § identidad / modelo de datos: [`../architecture.md`](../architecture.md)
- Atribución de egress/TCP a `owner_sub` (evaluación, no implementada): [0008](0008-network-flow-attribution.md)
