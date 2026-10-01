# Ops — Runner transparente para agentes (`asp` + IdP)

Cómo un agente (o un one-liner de ops) ejecuta un comando en sandbox **sin** gestionar a mano create → wait → exec → destroy **ni** el Bearer JWT de Keycloak.

Diseño IdP: [ADR-0007](adr/0007-multi-user-identity.md) · lab Keycloak: [ops-idp-keycloak-lab.md](ops-idp-keycloak-lab.md) · CLI base: [why-cli-asp.md](why-cli-asp.md).

## Por qué

Con `ASP_IDP_REQUIRED=1` en el CP lab (`127.0.0.1:18112`), cualquier `POST /v1/sandboxes` sin `Authorization: Bearer <JWT>` responde **401**. El ciclo de vida ya lo encapsula `asp sandbox run`, pero el token seguía siendo un paso manual (`curl` password-grant + `export`).

Los agentes necesitan un contrato mínimo:

1. Variables de entorno (URL del CP + dónde están los secretos del IdP).
2. Un comando: `asp sandbox run --cmd '…'`.

## Qué ganamos

| Pieza | Comportamiento |
|---|---|
| `asp auth login` | Password grant o `client_credentials` → cache `~/.cache/asp/id_token.json` (0600) |
| Auto-Bearer | `asp sandbox *` resuelve token (env → cache → fetch → API key) y envía `Authorization` |
| Secretos fuera de git | `~/.secrets/asp-keycloak-lab.txt` + env `ASP_IDP_*`; **nunca** en el repo |
| One-liner agente | Ver § uso abajo |

## Qué no ganamos (límites)

| Límite | Realidad |
|---|---|
| Password grant = lab | No es el flujo corporativo (auth code / device). Prod → Entra/Okta + otro grant. |
| `client_credentials` | Puede no llevar `groups` de usuario; RBAC lab suele necesitar password + `asp-lab`. |
| CP en loopback | Desde fuera de ncc1701d hace falta tunnel SSH al `18112`. |
| Cache local | Quien lea `~/.cache/asp/id_token.json` actúa como ese principal hasta `exp`. |
| No mint de refresh robusto | Se re-pide access token; no hay máquina de estados OAuth completa. |

---

## Variables de entorno

| Variable | Rol |
|---|---|
| `ASP_CP_URL` | Base del CP (lab: `http://127.0.0.1:18112`) |
| `ASP_ID_TOKEN` | Access token ya obtenido (prioridad máxima; también `--id-token`) |
| `ASP_IDP_REQUIRED` | `1` → exigir token; si hay secretos, auto-fetch |
| `ASP_IDP_TOKEN_URL` | Endpoint token; si vacío, `{ASP_IDP_ISSUER}/protocol/openid-connect/token` |
| `ASP_IDP_ISSUER` | Issuer OIDC (lab: `https://auth.luisgf.es/realms/asp`) |
| `ASP_IDP_SECRETS_FILE` | Fichero `KEY=VALUE` (default `~/.secrets/asp-keycloak-lab.txt`) |
| `ASP_IDP_CLIENT_ID` / `ASP_IDP_CLIENT_SECRET` | Overrides del fichero |
| `ASP_IDP_USERNAME` / `ASP_IDP_PASSWORD` | Password grant |
| `ASP_IDP_GRANT_TYPE` | `password` \| `client_credentials` (auto: password si hay user/pass) |
| `ASP_IDP_TOKEN_CACHE` | Ruta cache (default `~/.cache/asp/id_token.json`) |
| `ASP_API_KEY` | Fallback lab sin IdP (no sustituye JWT cuando el CP exige IdP) |

Claves del fichero de secretos del host (nombres; **valores solo en el host**):

```text
CLIENT_ID / CLIENT_SECRET / USER / PASSWORD / ISSUER / (opcional TOKEN_URL, JWKS)
```

---

## Uso para agentes (one-liner)

### En ncc1701d (CP lab ya activo)

```bash
# Precondiciones: build/asp, secretos 600 en ~/.secrets/, unit asp-control-plane activo
export ASP_CP_URL=http://127.0.0.1:18112
export ASP_IDP_REQUIRED=1
# ASP_IDP_SECRETS_FILE por defecto: ~/.secrets/asp-keycloak-lab.txt

./build/asp sandbox run --tenant=default --timeout=120s --cmd 'echo hello-from-agent'
```

El CLI:

1. Lee secretos / env → password grant a Keycloak.
2. Cachea el access token.
3. `create` → wait `running` → `exec` → `destroy` con `Authorization: Bearer …`.

### Login explícito + eval

```bash
eval "$(./build/asp auth login --print-env)"
./build/asp sandbox list --tenant=default
./build/asp auth status
./build/asp auth logout
```

### Desde el portátil (tunnel)

```bash
ssh -L 18112:127.0.0.1:18112 ubuntu@ncc1701d
# otro terminal, con copia local de secretos o SSH remote-command:
export ASP_CP_URL=http://127.0.0.1:18112 ASP_IDP_REQUIRED=1
asp sandbox run --tenant=default --cmd 'uname -a'
```

### Solo comprobar auth (sin nodo / sin VMM)

```bash
export ASP_CP_URL=http://127.0.0.1:18112 ASP_IDP_REQUIRED=1
./build/asp auth login
./build/asp sandbox list --tenant=default
# Esperado: 200 (lista posiblemente vacía). Sin token → 401.
```

---

## Resolución de Bearer (orden)

1. `--id-token` / `ASP_ID_TOKEN` / `ASP_IDP_ACCESS_TOKEN`
2. Cache válida (`exp` − 30s)
3. Fetch IdP si hay `CLIENT_ID` + token URL/issuer (fichero o env)
4. `ASP_API_KEY`
5. Si `ASP_IDP_REQUIRED` y nada de lo anterior → error claro (no llamar al CP a ciegas)

---

## Comandos `asp auth`

| Comando | Efecto |
|---|---|
| `asp auth login` | Force-fetch + cache; stderr con `source` / `expires` |
| `asp auth login --print-env` | `export ASP_ID_TOKEN='…'` en stdout (para `eval`) |
| `asp auth login --print-token` | Solo el JWT en stdout |
| `asp auth logout` | Borra cache |
| `asp auth status [--json]` | env / cache / can_fetch / idp_required |

---

## Tests y smoke

```bash
# Unit (httptest; sin Keycloak real)
cd cli && go test ./...

# Smoke CLI dry-run (sin IdP)
make smoke-asp

# Smoke auth contra lab (en ncc1701d; no imprime el token)
./scripts/smoke-asp-auth-lab.sh
```

---

## Checklist ops

1. ¿Rotó `CLIENT_SECRET`? Solo el fichero del host; reiniciar no hace falta en el CP (solo JWKS).
2. ¿`ASP_IDP_REQUIRED=0`? El auto-fetch sigue si hay secretos, pero el CP puede aceptar llamadas sin JWT.
3. ¿Agente en CI? Preferir secret store → env `ASP_IDP_*`; no copiar `asp-keycloak-lab.txt` al repo ni a logs.
4. ¿Promoción Entra/Okta? Nuevo grant; no reutilizar password grant ni el usuario `asp-lab`.

## Referencias

- [ops-idp-keycloak-lab.md](ops-idp-keycloak-lab.md)
- [why-cli-asp.md](why-cli-asp.md)
- [adr/0007-multi-user-identity.md](adr/0007-multi-user-identity.md)
- Plantilla unit: [`scripts/systemd/asp-control-plane.service`](../scripts/systemd/asp-control-plane.service)
