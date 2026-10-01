# Por qué / Qué ganamos — CLI `asp` (demo lifecycle)

## Por qué

Hasta ahora el ciclo create → wait `running` → exec → destroy se demostraba con
`curl` + `python` en smokes y en [`mvp-smoke.md`](mvp-smoke.md). Eso funciona
para CI, pero:

1. Agentes/ops tienen que recordar paths, headers Bearer y el bucle de poll.
2. El código de espera/backoff se duplica en cada script.
3. El exit code del guest no llega al shell que invocó el one-liner.

Queremos un binario único que hable el mismo HTTP API que ya existe, sin
añadir un protocolo nuevo.

## Qué ganamos

- Binario **`asp`** (`cli/cmd/asp`): `sandbox create|get|list|exec|delete|run`.
- **`asp sandbox run --cmd '…'`**: create → poll Get hasta `running` → Exec →
  destroy (salvo `--keep`). Exit code = `exit_code` del guest cuando es posible.
- Flags/env alineados con el CP: `--cp-url` / `ASP_CP_URL`, `--api-key` /
  `ASP_API_KEY`, `--tenant`, `--timeout`, `--node-id`.
- **IdP transparente:** `asp auth login` + auto `Authorization: Bearer` vía
  `ASP_ID_TOKEN` / cache / password|client_credentials (`ASP_IDP_*`,
  `~/.secrets/asp-keycloak-lab.txt`). Ver
  [`ops-asp-agent-runner.md`](ops-asp-agent-runner.md).
- Stderr para id/transiciones de estado; stdout para la salida del comando.
- Tests con `httptest` (sin servidores vivos) + smoke opcional
  [`scripts/smoke-asp-cli.sh`](../scripts/smoke-asp-cli.sh) /
  [`scripts/smoke-asp-auth-lab.sh`](../scripts/smoke-asp-auth-lab.sh).
- Docs en español con one-liner contra el stack dry-run local y el lab IdP.

## Límites honestos

Es un **cliente HTTP de demo/ops**, no un SDK multi-lenguaje ni un TUI. No
streaméa exec byte-a-byte (el CP hoy devuelve JSON acumulado). Auth = API key
**o** JWT IdP (lab Keycloak password grant / client_credentials); no es el
flujo OAuth corporativo completo. No gestiona enrollment de nodos.
