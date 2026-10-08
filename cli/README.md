# CLI `asp`

Cliente HTTP de demostración para el control-plane: ciclo de vida de sandboxes
y obtención transparente de Bearer JWT (IdP).

```bash
# Desde la raíz del repo
make asp
./build/asp sandbox run --cmd 'echo hello'   # --node-id fija un nodo si hace falta

# Con un IdP — el agente solo pasa el comando
export ASP_CONTROL_PLANE_URL=https://cp.example:8443 ASP_REQUIRE_TOKEN=1
./build/asp sandbox run --tenant=default --cmd 'echo hello'   # --tenant/ASP_TENANT optional: by default, your token's or key's tenant
```

**Por qué existe.** El ciclo create → esperar `running` → exec → destroy se hacía con `curl` y `python` en los smokes: cada script repetía las rutas, la cabecera Bearer y el bucle de espera, y el código de salida del guest no llegaba al shell que lanzó el comando. `asp` habla el mismo HTTP que ya existe, sin protocolo nuevo: `asp sandbox run --cmd '…'` hace el ciclo entero y sale con el código del guest; stderr lleva el id y las transiciones, y stdout, la salida del comando. **Qué no es:** un cliente HTTP de demostración y de operaciones, no un SDK en varios lenguajes, ni un TUI, ni un plugin de OpenCode. `asp session exec` pide un PTY y reenvía stdin, pero el cuerpo viaja como NDJSON (no son bytes opacos y no hay SIGWINCH). La autenticación es una API key o un JWT del IdP; no es el flujo OAuth corporativo completo.

`stop` apaga la sandbox y **conserva su disco** (`asp session stop`, `asp sandbox stop`); `resume`/`start` la arranca otra vez en el mismo nodo y disco; `rm`/`delete` borra la sandbox y su disco ([ADR-0012](../docs/adr/0012-retained-disks.md)). `asp session stop` ya no borra.

Configuración: la URL del plano de control es `--control-plane-url` / `ASP_CONTROL_PLANE_URL` (como en el node-agent); la bandera manda sobre la variable. Un `~/.config/asp/asp.yaml` (o `/etc/asp/asp.yaml`, con sus `asp.yaml.d/`) puede dar valor a cualquier variable `ASP_*` del CLI (`control_plane_url`, `tenant`, `api_key`…; el entorno manda sobre el fichero); `asp --config FILE …` o `ASP_CONFIG` nombran otro, y `asp config show [--effective]` dice de dónde viene cada ajuste, sin las credenciales. `ASP_CA_FILE` (`ca_file`) nombra el PEM del certificado que firmó el del plano de control (una CA propia, o el autofirmado de `asp-server`), que el CLI acepta además de los del sistema; `ASP_API_KEY_FILE` (`api_key_file`) nombra un fichero con la clave, que se usa si `ASP_API_KEY` no está puesta (así la clave de administración de un host no va en un fichero que lea todo el mundo). `asp node enroll-token` imprime, además del token, el comando del instalador para añadir el nodo, con la huella (`INSTALL_ASP_CA_SHA256`) del certificado en que confía el CLI. `ASP_REQUIRE_TOKEN=1` hace que el CLI falle si no puede conseguir un token del IdP, en vez de llamar sin él. Los nombres antiguos, `--cp-url`, `ASP_CP_URL` y `ASP_IDP_REQUIRED` (que en el CLI, el plano de control y el nodo significaba tres cosas distintas), siguen valiendo con un aviso en stderr.

Imágenes del guest (en el nodo): `sudo asp image pull --version 0.1.0` instala el kernel y la imagen de una versión en `/var/lib/asp/images/<versión>` (con `current` apuntando a la última y enlaces en `/opt/sandbox`), comprobándolos contra el `SHA256SUMS` de la release a medida que llegan: si algo no coincide no instala nada. `asp image verify [dir]` vuelve a comprobar lo instalado. Detalle en [`images/guest/README.md`](../images/guest/README.md).

Diagnóstico: `asp node doctor <id> [--json]` pide al node-agent de un nodo (en marcha) que se compruebe y trae el informe; `sudo asp doctor [--json] [-- flags]` hace las mismas comprobaciones en el propio nodo, sin arrancar el agente, con los ajustes de `/etc/asp/agent.yaml`. Salen con 1 si una comprobación falla ([`docs/how-to/troubleshooting.md`](../docs/how-to/troubleshooting.md)).

Subcomandos: `sandbox create|get|list [--all]|exec|stop|start|delete|run`, `session start|exec|status|stop|resume|rm`, `auth login|logout|status`, `node list|cordon|uncordon|enroll-token`.

`--node-id` es opcional: sin él, el plano de control elige un nodo con hueco. Si no hay ninguno, `create`, `run` y `session start` fallan con «no capacity» y el motivo. `asp node list` muestra el uso de cada nodo y cuándo caduca su certificado (admin u operador); `asp node cordon|uncordon <id>` lo saca o lo devuelve al reparto (admin). Con API key, los comandos `node` necesitan una clave de plataforma: los nodos los comparten todos los tenants, así que una clave de tenant recibe 403. `asp node enroll-token [--node-id ID] [--ttl 1h] [--json]` (admin) imprime un token de enroll de un solo uso para un servidor nuevo; con `--node-id` queda fijado a ese nodo y también sirve para cambiarle la clave a un nodo ya enrolado. En el servidor: `node-agent --enroll --enroll-token=<token> --node-id=<id>`. Ver [`docs/ops-multi-node.md`](../docs/ops-multi-node.md).

Documentación: [README del CLI](README.md),
[`docs/ops-asp-agent-runner.md`](../docs/ops-asp-agent-runner.md),
[`docs/ops-asp-session.md`](../docs/ops-asp-session.md) (sesión reutilizable; no sync de workspace),
quickstart dry-run [`docs/mvp-smoke.md`](../docs/mvp-smoke.md) §8,
conectar un IdP [`docs/how-to/idp.md`](../docs/how-to/idp.md).
