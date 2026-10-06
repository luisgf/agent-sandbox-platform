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

Subcomandos: `sandbox create|get|list|exec|delete|run`, `session start|exec|status|stop`, `auth login|logout|status`, `node list|cordon|uncordon`.

`--node-id` es opcional: sin él, el plano de control elige un nodo con hueco. Si no hay ninguno, `create`, `run` y `session start` fallan con «no capacity» y el motivo. `asp node list` muestra el uso de cada nodo (admin u operador); `asp node cordon|uncordon <id>` lo saca o lo devuelve al reparto (admin). Ver [`docs/ops-multi-node.md`](../docs/ops-multi-node.md).

Documentación: [`docs/why-cli-asp.md`](../docs/why-cli-asp.md),
[`docs/ops-asp-agent-runner.md`](../docs/ops-asp-agent-runner.md),
[`docs/ops-asp-session.md`](../docs/ops-asp-session.md) (sesión reutilizable; no sync de workspace),
quickstart dry-run [`docs/mvp-smoke.md`](../docs/mvp-smoke.md) §8,
lab Keycloak [`docs/ops-idp-keycloak-lab.md`](../docs/ops-idp-keycloak-lab.md).
