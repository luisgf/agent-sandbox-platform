# Por qué / Qué ganamos — Identidad multi-usuario (humano ↔ sandbox)

Ver también ADR: [`adr/0007-multi-user-identity.md`](adr/0007-multi-user-identity.md).  
Complementa (no sustituye) la identidad *dentro del guest* de [`adr/0003-identity.md`](adr/0003-identity.md).

## Por qué

El MVP ya sabe quién es el **tenant**, el **sandbox** y el **nodo**. Eso aísla bien cargas y secretos *respecto al guest* (SSH host-held, OIDC corto con claims server-side).

Pero en un entorno corporativo la pregunta del auditor no es solo “¿de qué tenant es?” sino:

- **¿Qué persona** creó este sandbox?
- **¿Quién** lanzó ese `exec` que tocó producción vía el proxy?
- **¿Quién** aprobó la firma SSH / aparece como actor en el token hacia Entra/Okta?

Hoy la respuesta honesta es: *una API key de tenant (o “api” / “node-agent” en el journal)*. Varias personas comparten la misma capacidad. El guest no elige claims (bien), pero el **plano de control tampoco distingue humanos**.

Sin sujeto humano:

1. No hay RBAC fino (admin / operator / viewer) dentro del mismo tenant.
2. El JWT de workload no puede llevar `user_sub` / `act` creíbles.
3. El bridge SSH global del host es aún más peligroso si varios usuarios meten sandboxes en el mismo nodo.

## Qué ganamos

- **`owner_sub` (+ `owner_email` opcional)** en el sandbox: dueño estable = `sub` del IdP (Entra/Okta/OIDC).
- **`actor_sub` en cada acción** y en `sandbox_events`: create, exec, destroy, approve SSH quedan atribuibles.
- **Authn real de usuario:** el CP valida JWT del IdP; grupos/roles → membership de tenant + rol.
- **Authz explícita:** owner o rol; create exige usuario; exec/destroy no son “cualquiera con la key del tenant”.
- **Workload OIDC con cadena humana:** mint sigue siendo server-side, pero añade `user_sub` / `act` desde `owner_sub` (el guest no lo inventa).
- **Camino SSH multi-user:** confirm gate atado a actor como mínimo; sock/sesión por usuario como objetivo antes de “corporate ready”.
- **API keys** no desaparecen: pasan a ser principals de *servicio*, auditables como `apikey:{id}`, no como identidad humana fingida.

## Qué no ganamos (límites honestos)

- UID de Linux en el guest **≠** empleado. No vamos a “mapear UIDs” como authz.
- Sin `ASP_SSH_AGENT_SOCK_TEMPLATE`, el bridge SSH **global** (`SSH_AUTH_SOCK` del operador) sigue siendo inseguro para multi-usuario.
- Con template (fase 4): cada sandbox resuelve un UDS por `owner_sub` (o FakeAgent si falta); **no** spawneamos ssh-agent ni cargamos keys — eso es ops.
- No hay IdP embebido ni magia SoftFail: sin JWKS/config de Entra/Okta no hay login humano.
- Diseño aceptado; **fases 1–4 hechas** (schema/audit + JWT IdP + RBAC + SSH scoped MVP). `user_sub` en mint **aún no** (fase 5).
- En lab (`ASP_IDP_REQUIRED` off / sin `ASP_IDP_ISSUER`), `owner_sub` vacío y sin header de actor siguen siendo válidos — no rompe smokes existentes.
- Con JWT IdP: `owner_sub` y `actor_sub` salen del `sub` del token; un body `owner_sub` distinto → 403.

## Cómo encaja con lo que ya hay

| Pieza actual | Sigue igual | Cambia |
|---|---|---|
| Guest no elige `tenant_id`/`sandbox_id` | sí | tampoco elige `user_sub` |
| SSH claves fuera del guest | sí | scoping por sesión/usuario |
| API keys | sí (servicio/lab) | ya no son la única identidad de cliente |
| mTLS node-agent | sí | humanos no autentican nodos |
| `sandbox_events` | sí | `actor` / `actor_sub` humanos |

## Lectura rápida del modelo

```text
Humano (JWT IdP) ──create──► sandbox{ owner_sub, tenant_id }
         │
         ├──exec/destroy──► authorize(owner | role) + audit(actor_sub)
         │
         └──(indirecto) guest mint OIDC ──► JWT{ sub=sandbox/…, user_sub=owner_sub, act }
```


## Fase 2 — JWT IdP en el control-plane (qué cambia ops)

| Variable | Default | Efecto |
|---|---|---|
| `ASP_IDP_ISSUER` | unset | Si vacío → IdP off (lab). Si set → valida Bearer JWT RS256. |
| `ASP_IDP_AUDIENCE` | unset | Si set → exige `aud` (string o array). |
| `ASP_IDP_JWKS_URL` | unset | JWKS directo; si vacío usa discovery `{issuer}/.well-known/openid-configuration`. |
| `ASP_IDP_REQUIRED` | `0` | `1` → create/list/get/exec/destroy/events exigen JWT válido. Nodos (enroll/heartbeat/work/claim/…) **no**. |

Matriz breve:

| Situación | Create/exec/destroy | Node enroll/heartbeat/work |
|---|---|---|
| IdP off (lab) | Body/`X-ASP-Actor-Sub` OK; API key opcional | Igual que antes (bootstrap / mTLS) |
| IdP on, token presente | `owner_sub`/`actor_sub` = token `sub`; email del claim si viene | Sin cambio (JWT humano no aplica) |
| IdP on, token falso | **401** | Sin cambio |
| `ASP_IDP_REQUIRED=1` sin token | **401** en rutas user-facing sandbox | Sigue sin exigir JWT humano |


## Fase 3 — RBAC (qué cambia ops)

| Variable | Default | Efecto |
|---|---|---|
| `ASP_IDP_ROLE_CLAIM` | `groups` | Claim del JWT con grupos/roles (`groups`, `roles`, u otro nombre). |
| `ASP_IDP_ROLE_MAP` | unset | Mapa `claimValue:role` CSV, p.ej. `Corp.Admin:admin,Corp.Ops:operator`. Si set, tiene prioridad sobre prefijo. |
| `ASP_IDP_ROLE_PREFIX` | `asp-` (si no hay map) | `asp-admin` → admin, `asp-operator` → operator, `asp-viewer` → viewer. También acepta roles bare (`admin`). |
| `ASP_IDP_DESTROY_ANY_GROUP` | `sandbox:destroy-any` | Si el claim incluye este valor, un **operator** puede destroy de cualquier sandbox del tenant. |

Matriz efectiva (IdP on + JWT presente):

| Acción | owner | admin | operator | viewer |
|---|---|---|---|---|
| Create | — (pasa a owner) | sí | sí | no |
| List / Get | sí | sí (todos) | sí (**tenant-wide**) | sí (**tenant-wide**, RO) |
| Exec | sí | sí | sí | no |
| Destroy | sí | sí | sí **solo propios** (o +destroy-any) | no |
| Egress policy | no | sí | no | no |

**Elección list:** tenant-wide para operator/viewer (no filtro a propios). IdP off → sin RBAC (lab/smokes iguales).


## Fase 4 — SSH scoped (qué cambia ops)

| Variable / flag | Default | Efecto |
|---|---|---|
| `ASP_SSH_AGENT_SOCK_TEMPLATE` / `--ssh-agent-sock-template` | unset | Si set → path por sandbox vía `{owner_sub}` / `{sandbox_id}` / `{id}`. Ausente → FakeAgent. Sin template → legacy `SSH_AUTH_SOCK` global. |
| `ASP_MULTI_USER=1` | off | Perfil multi-user: confirm SSH default-on. |
| `ASP_IDP_REQUIRED=1` | (CP) | En node-agent también enciende `--multi-user` heuristics → confirm default-on. |
| `ASP_SSH_AGENT_CONFIRM` | unset | `1` fuerza on; `0` fuerza off; unset → on si multi-user/template. |

Flujo:

```text
reconciler Start(sandbox{owner_sub})
  → Registry.Bind(id, owner_sub)  // expand template
  → AttachSandbox → ServeConnScoped(hostSock)  // no env fallback
  → symlink /run/asp/ssh-agent-{id}.sock → hostSock (si no vacío)
```

Approve (audit):

```bash
curl -s -X POST http://127.0.0.1:9100/v1/internal/ssh-agent/approve \
  -H 'Content-Type: application/json' \
  -H 'X-ASP-Actor-Sub: user:alice' \
  -d '{"ttl_seconds":30,"sandbox_id":"sb-1","actor_sub":"user:alice"}'
```

**Límite honesto:** no es un session-agent spawner (opción C del ADR); las claves reales deben existir en el UDS del template **antes** de que el guest firme.
