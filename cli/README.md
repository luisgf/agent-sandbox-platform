# CLI `asp`

Cliente HTTP de demostración para el control-plane: ciclo de vida de sandboxes
y obtención transparente de Bearer JWT (IdP).

```bash
# Desde la raíz del repo
make asp
./build/asp sandbox run --cmd 'echo hello'   # --node-id fija un nodo si hace falta

# Lab IdP (ncc1701d) — el agente solo pasa el comando
export ASP_CP_URL=http://127.0.0.1:18112 ASP_IDP_REQUIRED=1
./build/asp sandbox run --tenant=default --cmd 'echo hello'   # --tenant/ASP_TENANT optional: by default, your token's or key's tenant
```

`stop` apaga la sandbox y **conserva su disco** (`asp session stop`, `asp sandbox stop`); `resume`/`start` la arranca otra vez en el mismo nodo y disco; `rm`/`delete` borra la sandbox y su disco ([ADR-0012](../docs/adr/0012-retained-disks.md)). `asp session stop` ya no borra.

Subcomandos: `sandbox create|get|list [--all]|exec|stop|start|delete|run`, `session start|exec|status|stop|resume|rm`, `auth login|logout|status`, `node list|cordon|uncordon|enroll-token`.

`--node-id` es opcional: sin él, el plano de control elige un nodo con hueco. Si no hay ninguno, `create`, `run` y `session start` fallan con «no capacity» y el motivo. `asp node list` muestra el uso de cada nodo y cuándo caduca su certificado (admin u operador); `asp node cordon|uncordon <id>` lo saca o lo devuelve al reparto (admin). Con API key, los comandos `node` necesitan una clave de plataforma: los nodos los comparten todos los tenants, así que una clave de tenant recibe 403. `asp node enroll-token [--node-id ID] [--ttl 1h] [--json]` (admin) imprime un token de enroll de un solo uso para un servidor nuevo; con `--node-id` queda fijado a ese nodo y también sirve para cambiarle la clave a un nodo ya enrolado. En el servidor: `node-agent --enroll --enroll-token=<token> --node-id=<id>`. Ver [`docs/ops-multi-node.md`](../docs/ops-multi-node.md).

Documentación: [`docs/why-cli-asp.md`](../docs/why-cli-asp.md),
[`docs/ops-asp-agent-runner.md`](../docs/ops-asp-agent-runner.md),
[`docs/ops-asp-session.md`](../docs/ops-asp-session.md) (sesión reutilizable; no sync de workspace),
quickstart dry-run [`docs/mvp-smoke.md`](../docs/mvp-smoke.md) §8,
lab Keycloak [`docs/ops-idp-keycloak-lab.md`](../docs/ops-idp-keycloak-lab.md).
