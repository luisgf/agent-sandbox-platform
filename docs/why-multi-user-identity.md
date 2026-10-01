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
- El bridge SSH **global** de hoy sigue siendo inseguro para multi-usuario hasta la fase de sesión/scoped (ADR-0007 fase 4).
- No hay IdP embebido ni magia SoftFail: sin JWKS/config de Entra/Okta no hay login humano.
- Diseño aceptado; **fase 1 (schema + audit) hecha**; authz IdP / JWT / RBAC / SSH scoped / `user_sub` en mint **aún no** (fases 2–5).
- En lab, `owner_sub` vacío y sin header de actor siguen siendo válidos — no rompe smokes existentes.

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
