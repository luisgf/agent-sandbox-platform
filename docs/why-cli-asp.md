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
- **Sesión de agente (superficie primaria, [ADR-0009](adr/0009-agent-sessions.md)):**
  `asp session start|exec|status|stop --name` guarda id + URL en
  `~/.cache/asp/sessions/<nombre>.json` (0600, sin token) para que un harness
  apunte su bash tool a `asp session exec` durante la vida del agente. El exec
  de sesión imprime NDJSON según llega; `--buffered` deja el JSON de una pieza.
  Ver [`ops-asp-session.md`](ops-asp-session.md) y
  [`why-agent-sessions.md`](why-agent-sessions.md). `sandbox run` queda como
  primitiva de un solo comando (CI/ops), no como integración del bucle.
- Stderr para id/transiciones de estado; stdout para la salida del comando.
- Tests con `httptest` (sin servidores vivos) + smoke opcional
  [`scripts/smoke-asp-cli.sh`](../scripts/smoke-asp-cli.sh) /
  [`scripts/smoke-asp-auth-lab.sh`](../scripts/smoke-asp-auth-lab.sh).
- Docs en español con one-liner contra el stack dry-run local y el lab IdP.

## Límites honestos

Es un **cliente HTTP de demo/ops**, no un SDK multi-lenguaje ni un TUI ni un
plugin de OpenCode. `asp session exec` pide un PTY y reenvía stdin, pero el
cuerpo sigue siendo NDJSON (no bytes opacos, no SIGWINCH). `--workspace` hace
que el nodo exporte virtiofs; la imagen nueva monta el tag al boot y una imagen vieja lo monta a mano. El apagado por idle, si existe, es el reaper del CP
(`ASP_SANDBOX_IDLE_TIMEOUT`, default off), no un GC del fichero. Auth = API key **o** JWT IdP
(lab Keycloak password grant / client_credentials); no es el flujo OAuth
corporativo completo. No gestiona enrollment de nodos.
