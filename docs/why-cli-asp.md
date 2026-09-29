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
- Stderr para id/transiciones de estado; stdout para la salida del comando.
- Tests con `httptest` (sin servidores vivos) + smoke opcional
  [`scripts/smoke-asp-cli.sh`](../scripts/smoke-asp-cli.sh).
- Docs en español con one-liner contra el stack dry-run local.

## Límites honestos

Es un **cliente HTTP de demo/ops**, no un SDK multi-lenguaje ni un TUI. No
streaméa exec byte-a-byte (el CP hoy devuelve JSON acumulado). Auth = la misma
API key Bearer que el control-plane; no gestiona enrollment de nodos.
