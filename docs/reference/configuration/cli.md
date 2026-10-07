<!-- Generado por `make docs` a partir de settingsTable (cli/cmd/asp/settingstable.go). No lo edites a mano: un test falla si no coincide con el código. -->
# Configuración de `asp`

Cada ajuste es una variable de entorno `ASP_*`; muchos tienen también una opción en la orden que la usa (`--tenant`, `--api-key`…, ver [las órdenes](../cli.md)). Prioridad: **opción > variable de entorno > fichero de configuración > valor por defecto**. Los ficheros son `/etc/asp/asp.yaml` y, encima, `~/.config/asp/asp.yaml`, cada uno con los `asp.yaml.d/*.yaml` que lo acompañan; `asp --config FILE <orden>` o `ASP_CONFIG` leen ese fichero en su lugar (`--config` solo se admite antes de la orden). La clave de un ajuste es su variable sin `ASP_` y en minúsculas ([el fichero de configuración](../../how-to/config-file.md)). `asp config show [--effective]` lista cada ajuste con su valor y de dónde sale; las credenciales (🔒) salen como `<redacted>`.

Los ajustes de los otros programas: [plano de control](control-plane.md), [node-agent](node-agent.md) y [`asp-server`](asp-server.md).

| Variable | Clave del fichero | Por defecto | Qué hace |
|---|---|---|---|
| `ASP_CONTROL_PLANE_URL` | `control_plane_url` | `http://127.0.0.1:8080` | Base URL of the control plane. |
| `ASP_CP_URL` | — (un fichero no puede fijarla) | — | The old name of `ASP_CONTROL_PLANE_URL`. |
| `ASP_TENANT` | `tenant` | — | Tenant for create, list and run; empty means the caller's own. |
| `ASP_API_KEY` 🔒 | `api_key` | — | API key sent as a bearer token. |
| `ASP_API_KEY_FILE` | `api_key_file` | — | A file holding the API key, used when `ASP_API_KEY` is not set. |
| `ASP_CA_FILE` | `ca_file` | — | A PEM file with the certificate that signed the control plane's TLS certificate, trusted besides the system's. |
| `ASP_ID_TOKEN` 🔒 | `id_token` | — | IdP access token sent as a bearer token (preferred over the key). |
| `ASP_IDP_ACCESS_TOKEN` 🔒 | `idp_access_token` | — | Another name for `ASP_ID_TOKEN`. |
| `ASP_REQUIRE_TOKEN` | `require_token` | — | 1 fails when no IdP token can be had (it fetches one when it can). |
| `ASP_IDP_REQUIRED` | — (un fichero no puede fijarla) | — | The old name of `ASP_REQUIRE_TOKEN`. |
| `ASP_SESSION_DIR` | `session_dir` | `~/.cache/asp/sessions` | Where named sessions are kept (mode 0700). |
| `ASP_SESSION_FILE` | `session_file` | — | A single session file; ignores `--name`. |
| `ASP_IDP_ISSUER` | `idp_issuer` | — | OIDC issuer; the token endpoint is derived from it. |
| `ASP_IDP_JWKS_URL` | `idp_jwks_url` | — | The IdP's key set. |
| `ASP_IDP_TOKEN_URL` | `idp_token_url` | — | OIDC token endpoint. |
| `ASP_IDP_TOKEN_CACHE` | `idp_token_cache` | `~/.cache/asp/id_token.json` | Where `asp auth login` keeps the token. |
| `ASP_IDP_SECRETS_FILE` | `idp_secrets_file` | `~/.secrets/asp-keycloak-lab.txt` | KEY=VALUE file with the IdP client and user. |
| `ASP_IDP_CLIENT_ID` | `idp_client_id` | — | OIDC client id. |
| `ASP_IDP_CLIENT_SECRET` 🔒 | `idp_client_secret` | — | OIDC client secret. |
| `ASP_IDP_GRANT_TYPE` | `idp_grant_type` | — | OIDC grant: password or client_credentials. |
| `ASP_IDP_USERNAME` | `idp_username` | — | IdP user for the password grant. |
| `ASP_IDP_USER` | `idp_user` | — | The short name of `ASP_IDP_USERNAME`. |
| `ASP_IDP_PASSWORD` 🔒 | `idp_password` | — | IdP password for the password grant. |
| `ASP_LOCAL_NET_APPLY` | `local_net_apply` | — | 1 forces applying the local-net device, 0 never does. |
| `ASP_LOCAL_NET_OS` | `local_net_os` | — | Darwin writes the macOS utun script even on another system. |
| `ASP_LOCAL_NET_UTUN` | `local_net_utun` | — | An existing utun to reuse on macOS. |
