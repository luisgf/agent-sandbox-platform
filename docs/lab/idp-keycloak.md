# El IdP del laboratorio (Keycloak, realm `asp`)

Cómo está cableado el plano de control del [host de pruebas](ncc1701d.md) a un Keycloak, y qué hay que saber para tocarlo sin meter secretos en git. Qué necesita ASP de un IdP y cómo se configura, para cualquier despliegue: [conectar un IdP](../how-to/idp.md). El diseño: [ADR-0007](../adr/0007-multi-user-identity.md).

> Los nombres de host reales están en el `EnvironmentFile` del plano de control y en la configuración SSH de quien mantiene el host; aquí se escriben `idp.example.org` y `ncc1701d`.

## Por qué existe

Las fases 3u.1–3u.5 del código ya sabían validar JWT, mapear roles y exigir token. Hacía falta un IdP **real** (no mocks en un test) para probar el camino `Authorization: Bearer <access_token>` de punta a punta, fijar el contrato de claims (`iss`, `aud`, `groups`) que luego se traduce a Entra u Okta, y separar los secretos del host del repositorio.

## Las piezas

```text
Internet / operadores
        │
        ▼
 https://idp.example.org  ──nginx──►  Keycloak :8081 (contenedor de otra aplicación)
        │                               realm asp, cliente asp-api
        │ discovery + JWKS (públicos)
        ▼
 ~/.secrets/asp-idp.env  ──EnvironmentFile──►  asp-control-plane.service
                                               escucha en 127.0.0.1:18112
        ▲
        │ (password grant / pruebas)
 usuario asp-lab + client secret  ←  ~/.secrets/asp-keycloak-lab.txt
```

| Recurso | Valor en el laboratorio |
|---|---|
| Issuer | `https://idp.example.org/realms/asp` |
| JWKS | `https://idp.example.org/realms/asp/protocol/openid-connect/certs` |
| Cliente | `asp-api` (la audiencia del token de acceso) |
| Grupos | claim `groups`, prefijo `asp-`: `asp-admin`, `asp-operator`, `asp-user`, `asp-viewer`; más `sandbox:destroy-any` y `sandbox:exec-any` |
| Usuario de prueba | `asp-lab`, solo en `asp-user` (su contraseña, solo en el fichero de secretos del host). Ninguna cuenta del realm es operador o administrador |
| Realm | registro desactivado, protección contra fuerza bruta activada, vida del token de 300 s, sin grupos por defecto. El realm `asp` comparte Keycloak con otro realm de otra aplicación: solo se toca el `asp` |

Ajustes del plano de control (`ASP_IDP_*`, no secretos), tal como están en el `EnvironmentFile`:

```bash
# ~/.secrets/asp-idp.env — modo 600; no se sube a git
ASP_IDP_ISSUER=https://idp.example.org/realms/asp
ASP_IDP_AUDIENCE=asp-api
ASP_IDP_JWKS_URL=https://idp.example.org/realms/asp/protocol/openid-connect/certs
ASP_IDP_REQUIRED=1
ASP_IDP_ROLE_CLAIM=groups
ASP_IDP_ROLE_PREFIX=asp-
ASP_IDP_DESTROY_ANY_GROUP=sandbox:destroy-any
ASP_IDP_DEFAULT_TENANT=default   # los tokens del realm no traen el claim tenant_id
```

Sin `ASP_IDP_DEFAULT_TENANT`, cada `POST /v1/sandboxes` con un token del realm responde 401 `idp token names no tenant`.

## Secretos del host

| Ruta | Contenido | Uso |
|---|---|---|
| `~/.secrets/asp-idp.env` | `ASP_IDP_*`, `ASP_BOOTSTRAP_API_KEY`, `ASP_DATABASE_URL` | `EnvironmentFile=` de la unit del plano de control |
| `~/.secrets/asp-keycloak-lab.txt` | `CLIENT_ID`, `CLIENT_SECRET`, `USER`, `PASSWORD`, issuer y JWKS | El password grant del CLI (`asp auth login`) y las notas de operación; el plano de control **no** lo carga |
| `~/.secrets/asp-node.key` | La API key de plataforma que manda el node-agent | `ASP_NODE_API_KEY_FILE` del agente: el plano de control se alcanza por HTTP plano, sin certificado de cliente |

Todos, modo `600`. No copiarlos al repositorio ni a issues o PRs. Quien lea el client secret puede hacerse pasar por el cliente; quien lea `~/.cache/asp/id_token.json` actúa como esa persona hasta que el token caduca.

La clave del nodo se crea una vez, antes de reiniciar plano de control y agente:

```bash
head -c 24 /dev/urandom | base64 | sudo install -m 0600 -o ubuntu -g ubuntu /dev/stdin ~/.secrets/asp-node.key
echo "ASP_BOOTSTRAP_API_KEY=$(cat ~/.secrets/asp-node.key)" >> ~/.secrets/asp-idp.env
# y el agente la manda: ASP_NODE_API_KEY_FILE=/home/ubuntu/.secrets/asp-node.key
```

Con `ASP_IDP_REQUIRED=1` las rutas de usuario siguen pidiendo el token del IdP: la API key solo vale para las de nodo y las de plataforma.

## La consola de administración de Keycloak

Se administra por túnel, nunca por el nombre público:

```bash
ssh -L 8081:127.0.0.1:8081 ncc1701d       # y abrir http://127.0.0.1:8081/admin/ (realm asp)
```

Se opera con el `kcadm.sh` del propio contenedor, autenticado con el entorno del contenedor (sin imprimirlo), y se borra el fichero de configuración temporal que deja.

## Probar el IdP sin pegar secretos

```bash
# Con CLIENT_ID / CLIENT_SECRET / USER / PASSWORD cargados desde el fichero del host (sin hacer echo)
TOKEN_URL='https://idp.example.org/realms/asp/protocol/openid-connect/token'
access=$(curl -fsS -X POST "$TOKEN_URL" -d grant_type=password -d client_id="$CLIENT_ID" \
  -d client_secret="$CLIENT_SECRET" -d username="$USER" -d password="$PASSWORD" | jq -r .access_token)
curl -fsS 'http://127.0.0.1:18112/v1/sandboxes?tenant_id=default' -H "Authorization: Bearer $access"
```

Esperado: 200 (una lista, quizá vacía) con el token; 401 sin la cabecera. El script [`scripts/smoke-asp-auth-lab.sh`](../../scripts/smoke-asp-auth-lab.sh) hace lo mismo con el CLI y sin imprimir el token.

## Al tocar el IdP

1. ¿Cambió el issuer, la audiencia o el JWKS? Actualizar `asp-idp.env` y `systemctl restart asp-control-plane`.
2. ¿Rotó el client secret? Solo `asp-keycloak-lab.txt` y los scripts del grant: el plano de control no usa el secreto, solo el JWKS.
3. ¿Grupos nuevos? Alinear los nombres con `ASP_IDP_ROLE_PREFIX` o definir `ASP_IDP_ROLE_MAP`.
4. ¿Pasar a Entra u Okta? Otro `EnvironmentFile`; no reutilizar el password grant ni el usuario `asp-lab`.

## Límites

- No es Entra ni Okta: el mapeo de grupos corporativos (`ASP_IDP_ROLE_MAP`) no está cableado a un directorio.
- No hay tabla de pertenencia por tenant: el rol y el tenant salen del token.
- El plano de control del laboratorio escucha solo en loopback, sin TLS: el acceso remoto es un túnel SSH.
- El password grant es para laboratorios y CI. No depender de él en producción.
