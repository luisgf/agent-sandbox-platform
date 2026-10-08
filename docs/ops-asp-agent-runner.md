# Ops — Primitiva one-shot y auth IdP (`asp sandbox run`)

**No es la superficie de integración del agente.** Un agente largo usa una [sesión](ops-asp-session.md) ([ADR-0009](adr/0009-agent-sessions.md)): un sandbox, muchos `exec`, stop o idle. Esta página es la primitiva create→exec→destroy (CI, un comando de ops) y cómo el CLI obtiene el Bearer sin copiarlo a mano.

Cómo esa primitiva (o cualquier otro subcomando `asp`) corre **sin** gestionar el JWT de Keycloak a mano.

Diseño IdP: [ADR-0007](adr/0007-multi-user-identity.md) · conectar un IdP: [how-to/idp.md](how-to/idp.md) · CLI base: [README del CLI](../cli/README.md).

El one-shot **no** desaparece. Deja de ser el contrato que un harness debe llamar por tool.

## Por qué

Con `ASP_IDP_REQUIRED=1` en el plano de control, cualquier `POST /v1/sandboxes` sin `Authorization: Bearer <JWT>` responde **401**. El ciclo de vida ya lo encapsula `asp sandbox run`, pero el token seguía siendo un paso manual (`curl` password-grant + `export`).

El contrato mínimo de **auth** (vale igual para `session` y para `sandbox run`):

1. Variables de entorno (URL del CP + dónde están los secretos del IdP).
2. El CLI adjunta el Bearer solo.

El comando de un agente largo no es `asp sandbox run` por tool; es `asp session start` una vez y `asp session exec` después. `sandbox run --cmd '…'` queda para un solo comando.

## Qué ganamos

| Pieza | Comportamiento |
|---|---|
| `asp auth login` | Password grant o `client_credentials` → cache `~/.cache/asp/id_token.json` (0600) |
| Auto-Bearer | `asp sandbox *` resuelve token (env → cache → fetch → API key) y envía `Authorization` |
| Secretos fuera de git | `~/.secrets/asp-keycloak-lab.txt` + env `ASP_IDP_*`; **nunca** en el repo |
| One-liner one-shot | Sigue en § uso; es la primitiva, no el bucle del agente |

## Qué no ganamos (límites)

| Límite | Realidad |
|---|---|
| Password grant = lab | No es el flujo corporativo (auth code / device). Prod → Entra/Okta + otro grant. |
| `client_credentials` | Puede no llevar `groups` de usuario; El RBAC suele necesitar el password grant de un usuario de pruebas que esté en un grupo. |
| CP en loopback | Si el plano de control solo escucha en el loopback de su servidor, desde fuera hace falta un túnel SSH. |
| Cache local | Quien lea `~/.cache/asp/id_token.json` actúa como ese principal hasta `exp`. |
| No mint de refresh robusto | Se re-pide access token; no hay máquina de estados OAuth completa. |

---

## Variables de entorno

| Variable | Rol |
|---|---|
| `ASP_CONTROL_PLANE_URL` | Base del plano de control (por ejemplo `https://cp.example:8443`) |
| `ASP_ID_TOKEN` | Access token ya obtenido (prioridad máxima; también `--id-token`) |
| `ASP_REQUIRE_TOKEN` | `1` → exigir token; si hay secretos, auto-fetch (antes `ASP_IDP_REQUIRED`, que sigue valiendo con un aviso) |
| `ASP_IDP_TOKEN_URL` | Endpoint token; si vacío, `{ASP_IDP_ISSUER}/protocol/openid-connect/token` |
| `ASP_IDP_ISSUER` | Issuer OIDC (por ejemplo `https://idp.example.org/realms/asp`) |
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

## Uso de la primitiva (one-liner)

Para el bucle del harness, para aquí y usa [`ops-asp-session.md`](ops-asp-session.md). Lo de abajo es un comando que crea y destruye la VM.

### Con un plano de control ya activo

```bash
# Precondiciones: el binario asp, los secretos del IdP con modo 600 y un plano de control activo
export ASP_CONTROL_PLANE_URL=https://cp.example:8443
export ASP_REQUIRE_TOKEN=1
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
ssh -L 8443:127.0.0.1:8443 usuario@servidor      # el plano de control escucha solo en el loopback del servidor
# otro terminal, con copia local de secretos o SSH remote-command:
export ASP_CONTROL_PLANE_URL=http://127.0.0.1:8443 ASP_REQUIRE_TOKEN=1
asp sandbox run --tenant=default --cmd 'uname -a'
```

### Solo comprobar auth (sin nodo / sin VMM)

```bash
export ASP_CONTROL_PLANE_URL=https://cp.example:8443 ASP_REQUIRE_TOKEN=1
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
5. Si `ASP_REQUIRE_TOKEN` y nada de lo anterior → error claro (no llamar al CP a ciegas)

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

# Smoke de auth contra un plano de control con IdP (no imprime el token)
./scripts/smoke-asp-auth-lab.sh
```

---

## Checklist ops

1. ¿Rotó `CLIENT_SECRET`? Solo el fichero del host; reiniciar no hace falta en el CP (solo JWKS).
2. ¿`ASP_REQUIRE_TOKEN=0`? El auto-fetch sigue si hay secretos, pero el CP puede aceptar llamadas sin JWT.
3. ¿Agente en CI? Preferir secret store → env `ASP_IDP_*`; no copiar `asp-keycloak-lab.txt` al repo ni a logs.
4. ¿Promoción Entra/Okta? Nuevo grant; no reutilizar el password grant ni el usuario de pruebas.

## Sesión (producto) vs one-shot (esta página)

| | `asp session` | `asp sandbox run` |
|---|---|---|
| Rol | **superficie del agente** ([ADR-0009](adr/0009-agent-sessions.md)) | primitiva interna: CI, un comando |
| Ciclo de vida | `start` deja el sandbox; `exec` lo reutiliza horas; `stop` o idle reap lo paran **sin borrar el disco** (`resume` lo arranca otra vez); `rm` lo borra | create → exec → destroy en un proceso |
| Estado en disco | `~/.cache/asp/sessions/<nombre>.json` (0600), sin secretos | ninguno |
| Workspace del host | `virtiofsd` + tag `workspace` si el path no está vacío; la imagen nueva monta `/workspace` al boot (la vieja, a mano) | el one-shot no pasa `--workspace` |

Detalle, wrapper de shell y consecuencias: [ops-asp-session.md](ops-asp-session.md) · [ADR-0009](adr/0009-agent-sessions.md).

## Referencias

- [adr/0009-agent-sessions.md](adr/0009-agent-sessions.md)
- [ADR-0009](adr/0009-agent-sessions.md)
- [ops-asp-session.md](ops-asp-session.md)
- [how-to/idp.md](how-to/idp.md)
- [README del CLI](../cli/README.md)
- [adr/0007-multi-user-identity.md](adr/0007-multi-user-identity.md)
- La unit del paquete: [`packaging/systemd/asp-control-plane.service`](../packaging/systemd/asp-control-plane.service)
