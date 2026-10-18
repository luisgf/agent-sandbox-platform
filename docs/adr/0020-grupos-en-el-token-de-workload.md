# ADR-0020: Un claim de grupos en el token de workload: los grupos del dueño, fijados al crear la sandbox

- **Estado:** Propuesta
- **Fecha:** 2026-10-18
- **Implementación:** hecha en este corte: `groups` en el JWT y en `claims` del mint, `owner_groups` en la sandbox (migración 025), capturado en el create con token del IdP. No está probado en un host con KVM ni contra un infrabroker real.
- **Extiende:** [0007](0007-multi-user-identity.md) (los claims humanos del token de workload), [0019](0019-acceso-a-infraestructura.md) (su Fase 2)
- **Relacionados:** [0003](0003-identity.md) (el guest solo elige `aud`), [conectar un IdP](../how-to/idp.md) (de dónde salen los grupos), [usar ASP con infrabroker](../how-to/infrabroker.md) (quién consume el claim), [`docs/OPERATIONS.md` de infrabroker](https://github.com/luisgf/infrabroker/blob/main/docs/OPERATIONS.md) (`groups_claim`)
- **No es:** RBAC nuevo en ASP (los roles de [0007](0007-multi-user-identity.md) no cambian), ni una consulta al IdP desde el plano de control, ni un claim que el guest pueda pedir.

## Contexto

El token de workload que el plano de control acuña para una sandbox lleva `sub=sandbox/{id}`, `tenant_id`, `sandbox_id` y, cuando la sandbox tiene dueño, `user_sub` y `act` ([0007](0007-multi-user-identity.md)). El guest solo elige `aud`. Con eso, un consumidor OIDC sabe **quién** está detrás de la sandbox, pero no **qué alcance** tiene esa persona.

[0019](0019-acceso-a-infraestructura.md) lo deja como límite honesto de su Fase 1: infrabroker atribuye cada acción al dueño (`user_claim: "user_sub"`), pero su RBAC por usuario necesita un claim de grupos, y el token no lo lleva. Sin él, todo dueño de sandbox alcanza lo que alcance el CN del broker. El verificador de infrabroker tiene tres comportamientos que condicionan el diseño:

- Con `groups_claim` configurado es **fail-closed**: un token **sin** ese claim se rechaza (401).
- Una lista **vacía** se acepta y se reenvía tal cual al signer, que exige `grupos del host ∩ grupos del usuario ≠ ∅`: una lista vacía no alcanza ningún host.
- Sin `groups_claim`, los grupos no se propagan y el RBAC por usuario está apagado.

Lo que ASP tiene a mano es el token del IdP con que una persona crea la sandbox: su claim de roles (`ASP_IDP_ROLE_CLAIM`, por defecto `groups`) es de donde ya sale el rol de esa persona ([conectar un IdP](../how-to/idp.md)). Después de ese momento el plano de control no vuelve a ver un token humano: el mint lo pide el guest por vsock, o el nodo en su nombre, y el plano de control no tiene credencial propia ante el IdP (ni cuenta de servicio ni API de directorio) para preguntar por los grupos de nadie.

## Decisión

### 1. El claim es `groups`, una lista de cadenas

El JWT lleva `"groups": ["…", …]`, y la respuesta del mint lo repite en `claims` como hace con `user_sub` y `act`. Es el nombre que infrabroker (y Keycloak, Entra y Okta) usan por defecto, así que el consumidor se configura con `groups_claim: "groups"` y nada más.

### 2. Los valores son los del claim de roles del IdP del dueño en el momento del create, tal cual, y se persisten con la sandbox

Al crear una sandbox con un token del IdP, el plano de control guarda en la sandbox (`owner_groups`, migración 025) los valores del claim de roles de ese token: **los mismos valores de los que sale el rol**, sin mapearlos a `admin|operator|user|viewer`, sin quitarles el prefijo `asp-`, sin cambiar mayúsculas. Solo se normalizan en lo que no cambia su significado: espacios alrededor fuera, vacíos fuera, duplicados fuera, orden estable. Cada mint de esa sandbox copia la lista en `groups`.

Por qué los grupos y no el rol: el rol de ASP es un vocabulario de cuatro palabras que proyecta los grupos y pierde el resto. La política de infrabroker nombra grupos del IdP (`"groups": ["sre", "db-admins"]` en cada host), y es con esos nombres con los que el operador ya gobierna el acceso en su directorio. Un rol de ASP no se puede volver a convertir en ellos.

Por qué en el create y no en el mint: en el mint no hay token humano (lo pide el guest) y el plano de control no puede preguntar al IdP. La única alternativa sería una cuenta de servicio del plano de control en el IdP, que es una credencial más que custodiar y un acoplamiento a la API de cada directorio; se descarta abajo.

Por qué persistidos y visibles: el campo `owner_groups` de la sandbox dice con qué grupos acuña, igual que `owner_sub` dice en nombre de quién. Lo ve quien pueda ver la sandbox, como `owner_email`.

### 3. El guest no lo elige, como el resto de claims

`groups` se suma a la tabla de [0007](0007-multi-user-identity.md): lo pone el plano de control desde el almacén y **nadie del lado del guest lo toca**.

| Claim | Origen | ¿Lo puede elegir el guest? |
|---|---|---|
| `groups` | `owner_groups` de la sandbox, capturado del token del IdP en el create | **no**: el cuerpo del create no lo acepta (ni con IdP, ni en el lab abierto, ni con una API key) y el mint lo ignora (en el plano de control y en el proxy de identidad del nodo) |

Un cliente que ponga `owner_groups` en el cuerpo del create, o `groups` en el del mint, no cambia nada: el primero no existe como campo de entrada y el segundo se descarta, como `user_sub` y `act`.

### 4. Una sandbox sin dueño no lleva el claim, nunca una lista vacía

No llevan `groups`: las sandboxes creadas con una API key o en el lab abierto (sin `owner_sub`), las anteriores a la migración, y las de un dueño cuyo token no traía valores en el claim de roles. En todas ellas el claim está **ausente**; el plano de control no acuña `"groups": []` jamás.

La razón es lo que cada forma significa en el consumidor. Para infrabroker con `groups_claim`, una lista vacía es «este usuario no está en ningún grupo» y deniega todos los hosts; un claim ausente es «este token no dice sus grupos» y se rechaza antes de mirar ninguna política. Las dos deniegan, pero la segunda es la señal honesta: una sandbox sin dueño no tiene grupos que declarar, y un `[]` haría pasar por una respuesta del IdP lo que es la falta de ella. Y para un consumidor sin `groups_claim`, el claim ausente es exactamente lo que había hasta hoy: nada cambia para quien no lo pida.

### 5. Tamaño: un tope en el create, y por encima se rechaza, no se recorta

El token de workload viaja como bearer en cada llamada. La lista se acota a **64 valores de hasta 255 bytes cada uno**; un token del IdP que traiga más hace que el create falle con `400` y el motivo. No se recorta en silencio: una lista a la que le falta un grupo es una denegación en infrabroker que nadie sabría explicar. Quien tenga personas en más grupos que eso los filtra en el IdP (un *mapper* que solo emita los grupos que ASP y el broker necesitan es lo habitual en Keycloak y en Entra, que además deja de emitir el claim por encima de 200 grupos).

### 6. Rotación

El token se reacuña cada vez que el guest lo pide (cinco minutos de vida por defecto) y cada vez lleva la misma lista: la de la sandbox. La lista no se renueva sola; cambia con una sandbox nueva. La rotación de las claves de firma (`ASP_OIDC_KEY_PREV`) no tiene que ver con el claim.

## Alternativas consideradas

| Alternativa | Pros | Contras | Decisión |
|---|---|---|---|
| **Derivar el claim del rol de ASP** (`["asp-operator"]`) | Nada que persistir; vocabulario cerrado y pequeño | El broker tendría que nombrar hosts por roles de ASP, no por grupos del IdP; cuatro valores no distinguen al equipo de bases de datos del de red | **Rechazada** |
| **Consultar al IdP en cada mint** (cuenta de servicio, API de directorio) | Grupos frescos; un cambio en el IdP se nota en cinco minutos | Una credencial de plataforma más, una API distinta por IdP, el mint depende de que el IdP conteste; ASP no tiene hoy ningún cliente del IdP más allá del JWKS | **Rechazada.** Si algún día hace falta, es un corte propio |
| **Refrescar la lista en `resume`** con el token de quien reanuda | Barato: el token humano está ahí | Quien reanuda puede no ser el dueño (`operator`, `admin`), y entonces los grupos serían de otra persona; una sandbox reanudada por su dueño tendría grupos distintos de una que nunca paró, sin que nada lo diga | **No en este corte.** Se reabre si la foto del create resulta corta en la práctica |
| **Un filtro por prefijo en el plano de control** (`ASP_IDP_WORKLOAD_GROUPS_PREFIX`) para no enviar todos los grupos de una persona | Menos datos en el token; privacidad | Un ajuste más, con su clave en `server.yaml`; el IdP ya sabe filtrar, y es donde el operador espera decidir qué sale en un token | **No en este corte.** El tope de tamaño cubre el caso límite; el filtro queda anotado |
| **Recortar la lista al tope** en vez de rechazar | El create nunca falla por esto | Una denegación en el broker sin explicación posible | **Rechazada** |
| **Emitir `[]` sin dueño** | El claim siempre está | Hace pasar «no sé sus grupos» por «no tiene grupos»; un consumidor sin `groups_claim` vería un claim que antes no existía | **Rechazada** |

## Consecuencias

### Positivas

- Cierra la Fase 2 de [0019](0019-acceso-a-infraestructura.md) en lo que toca a ASP: con `groups_claim: "groups"` el signer de infrabroker aplica su RBAC por usuario a cada sandbox, y el token pasa de dar atribución a dar alcance.
- El operador gobierna el acceso en un solo sitio, su directorio: los grupos que dan el rol en ASP son los que llegan al broker.
- Nada cambia para quien no pida el claim: una sandbox sin dueño acuña lo mismo que ayer, y un consumidor sin `groups_claim` lo ignora.

### Negativas / coste

- Una columna más en `sandboxes` y una migración (la 025, la primera con gemela en SQLite).
- `owner_groups` es un dato personal más en la sandbox, visible a quien pueda verla. Un operador que no quiera eso filtra en el IdP qué grupos salen en el token.
- Los grupos que ASP usa para sí (`asp-operator`, `sandbox:destroy-any`, …) también viajan: no son secretos, y el broker simplemente no los encuentra en ningún host, pero están ahí.

## Límites honestos

- **Los grupos son una foto del create.** Una persona que sale de un grupo en el IdP sigue acuñando con él en las sandboxes que ya tenía, hasta que se destruyan (parar y reanudar conserva la lista). Que el token viva cinco minutos no ayuda: se reacuña con la misma lista. Revocar de verdad es destruir la sandbox, y un operador puede hacerlo (`destroy-any`).
- **Es lo que el IdP puso en el claim de roles**, ni más ni menos: si el *mapper* emite rutas (`/sre`) o solo un subconjunto de grupos, eso es lo que el broker ve. La guía del IdP ya pide grupos sin ruta.
- **El guest sigue eligiendo `aud`.** Cualquier sandbox del plano de control puede pedir `aud=infrabroker`; lo que la sandbox alcanza lo decide el broker con `groups`, no el hecho de pedir el token.
- Nada de esto está probado contra un infrabroker real ni en un host con KVM: lo prueban los tests del plano de control (el JWT de una sandbox con dueño lleva los grupos del token con que se creó; una sin dueño no lleva el claim; un `groups` en el cuerpo del mint o un `owner_groups` en el del create no cambian nada).
- Las guías de infrabroker (`OPERATIONS.md`, `SANDBOX.md`) dicen todavía «sin `groups_claim`» para el token de ASP: se actualizan desde ese repositorio.

## Referencias

- [Usar ASP con infrabroker](../how-to/infrabroker.md): cómo configurar `groups_claim` y qué cambia en el RBAC por usuario.
- [Conectar un IdP](../how-to/idp.md): el claim de roles del que salen los valores.
- [0007](0007-multi-user-identity.md) § Enmiendas, [0019](0019-acceso-a-infraestructura.md) § Enmiendas.
