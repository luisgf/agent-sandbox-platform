# CLI `asp`

Cliente HTTP de demostración para el control-plane: ciclo de vida de sandboxes
y obtención transparente de Bearer JWT (IdP).

```bash
# Desde la raíz del repo
make asp
./build/asp sandbox run --cmd 'echo hello'   # --node-id fija un nodo si hace falta

# Lab IdP (ncc1701d) — el agente solo pasa el comando
export ASP_CONTROL_PLANE_URL=http://127.0.0.1:18112 ASP_REQUIRE_TOKEN=1
./build/asp sandbox run --tenant=default --cmd 'echo hello'   # --tenant/ASP_TENANT optional: by default, your token's or key's tenant
```

`stop` apaga la sandbox y **conserva su disco** (`asp session stop`, `asp sandbox stop`); `resume`/`start` la arranca otra vez en el mismo nodo y disco; `rm`/`delete` borra la sandbox y su disco ([ADR-0012](../docs/adr/0012-retained-disks.md)). `asp session stop` ya no borra.

Configuración: la URL del plano de control es `--control-plane-url` / `ASP_CONTROL_PLANE_URL` (como en el node-agent); la bandera manda sobre la variable. Un `~/.config/asp/asp.yaml` (o `/etc/asp/asp.yaml`, con sus `asp.yaml.d/`) puede dar valor a cualquier variable `ASP_*` del CLI (`control_plane_url`, `tenant`, `api_key`…; el entorno manda sobre el fichero); `asp --config FILE …` o `ASP_CONFIG` nombran otro, y `asp config show [--effective]` dice de dónde viene cada ajuste, sin las credenciales. `ASP_REQUIRE_TOKEN=1` hace que el CLI falle si no puede conseguir un token del IdP, en vez de llamar sin él. Los nombres antiguos, `--cp-url`, `ASP_CP_URL` y `ASP_IDP_REQUIRED` (que en el CLI, el plano de control y el nodo significaba tres cosas distintas), siguen valiendo con un aviso en stderr.

Imágenes del guest (en el nodo): `sudo asp image pull --version 0.1.0` instala el kernel y la imagen de una versión en `/var/lib/asp/images/<versión>` (con `current` apuntando a la última y enlaces en `/opt/sandbox`), comprobándolos contra el `SHA256SUMS` de la release a medida que llegan: si algo no coincide no instala nada. `asp image verify [dir]` vuelve a comprobar lo instalado. Detalle en [`images/guest/README.md`](../images/guest/README.md).

Diagnóstico: `asp node doctor <id> [--json]` pide al node-agent de un nodo (en marcha) que se compruebe y trae el informe; `sudo asp doctor [--json] [-- flags]` hace las mismas comprobaciones en el propio nodo, sin arrancar el agente, con los ajustes de `/etc/asp/agent.yaml`. Salen con 1 si una comprobación falla ([`docs/how-to/troubleshooting.md`](../docs/how-to/troubleshooting.md)).

Subcomandos: `sandbox create|get|list [--all]|exec|stop|start|delete|run`, `session start|exec|status|stop|resume|rm`, `auth login|logout|status`, `node list|cordon|uncordon|enroll-token`.

`--node-id` es opcional: sin él, el plano de control elige un nodo con hueco. Si no hay ninguno, `create`, `run` y `session start` fallan con «no capacity» y el motivo. `asp node list` muestra el uso de cada nodo y cuándo caduca su certificado (admin u operador); `asp node cordon|uncordon <id>` lo saca o lo devuelve al reparto (admin). Con API key, los comandos `node` necesitan una clave de plataforma: los nodos los comparten todos los tenants, así que una clave de tenant recibe 403. `asp node enroll-token [--node-id ID] [--ttl 1h] [--json]` (admin) imprime un token de enroll de un solo uso para un servidor nuevo; con `--node-id` queda fijado a ese nodo y también sirve para cambiarle la clave a un nodo ya enrolado. En el servidor: `node-agent --enroll --enroll-token=<token> --node-id=<id>`. Ver [`docs/ops-multi-node.md`](../docs/ops-multi-node.md).

Documentación: [`docs/why-cli-asp.md`](../docs/why-cli-asp.md),
[`docs/ops-asp-agent-runner.md`](../docs/ops-asp-agent-runner.md),
[`docs/ops-asp-session.md`](../docs/ops-asp-session.md) (sesión reutilizable; no sync de workspace),
quickstart dry-run [`docs/mvp-smoke.md`](../docs/mvp-smoke.md) §8,
lab Keycloak [`docs/ops-idp-keycloak-lab.md`](../docs/ops-idp-keycloak-lab.md).
