# Conectar un IdP (OIDC)

Con un IdP configurado, el dueño de cada sandbox es el `sub` del token de quien la crea: no lo elige el cliente ni el guest ([ADR-0007](../adr/0007-multi-user-identity.md)), y el rol de esa persona (administrar, operar, solo usar sandboxes, solo mirar) sale del mismo token. Esta guía dice qué necesita el plano de control del IdP, cómo se configura, qué puede hacer cada rol y cómo se comprueba. Cada ajuste, con su valor por defecto: [la referencia del plano de control](../reference/configuration/control-plane.md#autenticación-y-autorización).

Un plano de control puede funcionar sin IdP, con API keys (`asp apikey`): es lo que hace [`asp-server`](single-host.md). El IdP añade que cada persona entra con su identidad y que el rol sale de sus grupos.

## Qué tiene que dar el IdP

- Un **issuer OIDC** con su JWKS (por *discovery*, o con `ASP_IDP_JWKS_URL`), y tokens de acceso firmados con RS256.
- Un cliente (aplicación) cuyo token lleve `aud` igual a la **audiencia** que se configura en el plano de control. Sin audiencia, el plano de control aceptaría el token que el emisor haya dado a cualquier otra aplicación.
- Un **claim con los grupos o roles** de la persona (por defecto `groups`).
- Un **claim con el tenant** (por defecto `tenant_id`; `ASP_IDP_TENANT_CLAIM` lo cambia), o un tenant fijo para todos (`ASP_IDP_DEFAULT_TENANT`). Cada petición queda confinada al tenant del token; un token sin tenant recibe 401.

Los nodos no usan este camino: se autentican con su certificado (mTLS) o con una API key de plataforma.

## Configurar el plano de control

```yaml
# /etc/asp/server.yaml.d/20-idp.yaml
idp_issuer: https://idp.example.org/realms/asp
idp_audience: asp-api
idp_required: true            # sin token del IdP, las rutas de usuario dan 401
idp_role_claim: groups
idp_role_prefix: asp-         # el grupo asp-operator da el rol operator
idp_default_tenant: default   # solo si el token no lleva el claim del tenant
```

- `idp_required: true` **exige** `idp_audience`: sin ella el plano de control no arranca (código 2). `ASP_IDP_ALLOW_ANY_AUDIENCE=1` lo permite para un lab y avisa en el log.
- El JWKS se vuelve a leer cada 5 minutos, y ante un `kid` desconocido como mucho una vez cada 30 s: una clave que el IdP retira deja de validar.
- Un token sin `exp` se rechaza (`ASP_IDP_REQUIRE_EXP=0` lo permite, no se recomienda).

## Cómo valida un token el plano de control

1. El cliente envía `Authorization: Bearer <JWT>`.
2. Si hay `ASP_IDP_ISSUER` y el bearer es un JWT, comprueba su firma RS256 contra el JWKS y sus `iss`, `aud` y `exp`.
3. El `sub` del token es el dueño (`owner_sub`) y el actor de lo que se haga; de los grupos sale el rol; del tenant, el tenant. Un `owner_sub` que mande el cliente en el cuerpo se rechaza.
4. `/healthz`, el discovery OIDC del propio ASP y su JWKS (con los que las sandboxes validan los tokens de workload que ASP emite) son públicos. Las rutas de los nodos no piden el token de una persona: usan mTLS o una API key de plataforma.

Sin `ASP_IDP_ISSUER` no hay roles de IdP. Con una API key, el actor es la key y las sandboxes no tienen dueño; solo en un laboratorio abierto (`ASP_INSECURE_OPEN_API=1`, sin IdP ni keys) el cliente puede poner `owner_sub` en el cuerpo.

## Los roles

El rol sale de los valores del claim de grupos. Con el prefijo `asp-` (el valor por defecto), `asp-admin`, `asp-operator`, `asp-user` y `asp-viewer` dan el rol del mismo nombre; **un grupo a secas (`admin`) no da ninguno**. Para otros nombres, `ASP_IDP_ROLE_MAP=grupo:rol,…` (por ejemplo `ops:operator,devs:user`), que gana sobre el prefijo; si no fijas también `ASP_IDP_ROLE_PREFIX`, un grupo que el mapa no nombra no da nada. Con varios grupos gana el rol más alto, y quien es `user` y `viewer` a la vez es `operator`, que es justo su unión.

| | `admin` | `operator` | `user` | `viewer` |
|---|---|---|---|---|
| Crear una sandbox | sí | sí | sí | no |
| Listar las del tenant | todas | todas | solo las suyas | todas |
| Ver una (`get`, eventos) | sí | sí | solo las suyas | sí |
| `exec` | en todas | en las suyas, y en las de otros con el grupo `sandbox:exec-any` | en las suyas | no |
| Parar, reanudar, borrar | todas | las suyas, y las de otros con el grupo `sandbox:destroy-any` | las suyas | no |
| Ver los nodos (`asp node list`) | sí | sí | no | no |
| Administrar nodos (`cordon`, `revoke`, `rotate-cert`) y la política de egress de un tenant | sí | no | no | no |

`sandbox:exec-any` y `sandbox:destroy-any` son grupos a secas, sin prefijo (sus nombres se cambian con `ASP_IDP_EXEC_ANY_GROUP` y `ASP_IDP_DESTROY_ANY_GROUP`). Hacer `exec` en la sandbox de otro da acceso a su workspace y a los tokens que esa sandbox obtiene, por eso un operador lo necesita expresamente.

## Un ejemplo con Keycloak

Lo que el plano de control espera, en términos de Keycloak (los pasos exactos de la consola cambian entre versiones):

1. Un **realm** (`asp`) y, en él, un **cliente confidencial** (`asp-api`). Su token de acceso tiene que llevar `aud: asp-api`: añade al cliente un *mapper* de audiencia.
2. Los **grupos** `asp-admin`, `asp-operator`, `asp-user`, `asp-viewer`, y los dos de permisos, `sandbox:exec-any` y `sandbox:destroy-any`. Las personas y los agentes que solo usan sandboxes van en `asp-user`; `asp-operator` es para quien opera sobre las de otros.
3. Un *mapper* de **pertenencia a grupos** que ponga los grupos en el claim `groups` **sin la ruta completa** (*Full group path* desactivado): con ella los valores serían `/asp-operator`, y el plano de control no reconoce un valor con barra.
4. Si no pones el tenant en un claim, `idp_default_tenant` para todos los usuarios del realm.

## Comprobarlo

```bash
curl -fsS https://cp.example:8443/healthz                                  # ok, sin token
curl -sS -o /dev/null -w '%{http_code}\n' https://cp.example:8443/v1/sandboxes   # 401 sin token
asp auth login                    # pide un token y lo guarda (ver más abajo)
asp sandbox list                  # 200, con el token
```

`asp auth login` obtiene el token con el flujo de contraseña o de `client_credentials` de un cliente del IdP; el CLI lo guarda en `~/.cache/asp/id_token.json` y lo adjunta a cada petición ([la referencia de `asp`](../reference/configuration/cli.md) lista sus variables `ASP_IDP_*`). Ese flujo de contraseña es para laboratorios y CI. Una persona real entra con el flujo que su IdP recomiende (código de autorización, *device*), y entonces pasa el token en `ASP_ID_TOKEN`.

## Lo que esto no hace

- **No hay tabla de pertenencia por tenant**: el rol sale del token, y el tenant también. Quien administra los grupos en el IdP administra los permisos.
- No habla SCIM ni sincroniza usuarios; ASP solo valida tokens.
- El mapeo de los grupos de un directorio corporativo (Entra, Okta) a estos roles lo hace el IdP (que emita los grupos con estos nombres) o `ASP_IDP_ROLE_MAP`.
