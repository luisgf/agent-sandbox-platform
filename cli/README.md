# CLI `asp`

Cliente HTTP de demostración para el control-plane: ciclo de vida de sandboxes
y obtención transparente de Bearer JWT (IdP).

```bash
# Desde la raíz del repo
make asp
./build/asp sandbox run --node-id=dev-node --cmd 'echo hello'

# Lab IdP (ncc1701d) — el agente solo pasa el comando
export ASP_CP_URL=http://127.0.0.1:18112 ASP_IDP_REQUIRED=1
./build/asp sandbox run --tenant=default --cmd 'echo hello'
```

Subcomandos: `sandbox create|get|list|exec|delete|run`, `auth login|logout|status`.

Documentación: [`docs/why-cli-asp.md`](../docs/why-cli-asp.md),
[`docs/ops-asp-agent-runner.md`](../docs/ops-asp-agent-runner.md),
quickstart dry-run [`docs/mvp-smoke.md`](../docs/mvp-smoke.md) §8,
lab Keycloak [`docs/ops-idp-keycloak-lab.md`](../docs/ops-idp-keycloak-lab.md).
