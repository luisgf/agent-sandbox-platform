# El fichero de configuración

Cada componente de ASP lee sus ajustes de un fichero YAML y de los ficheros de un directorio de *drop-ins* que lo acompaña, como el `config.yaml` y el `config.yaml.d/` de k3s. La unit de systemd se queda en `ExecStart=… --config /etc/asp/agent.yaml`, sin la fila de `Environment=` (diez o quince por componente) que había antes.

| Componente | Fichero | Drop-ins | Modo |
|---|---|---|---|
| node-agent | `/etc/asp/agent.yaml` | `/etc/asp/agent.yaml.d/*.yaml` | 0600, de root |
| plano de control | `/etc/asp/server.yaml` | `/etc/asp/server.yaml.d/*.yaml` | 0640, de root y del grupo `asp-control-plane` (el servicio lee el fichero con ese usuario) |
| CLI `asp` | `/etc/asp/asp.yaml`, y encima `~/.config/asp/asp.yaml` | `asp.yaml.d/*.yaml` junto a cada uno | 0600 si lleva una clave |

El fichero por defecto se lee si existe. `--config FILE` o `ASP_CONFIG` nombran otro, y entonces **tiene que existir** (un nombre mal escrito es un error, no un nodo que arranca con los valores por defecto). En el CLI la bandera va antes del comando: `asp --config ./asp.yaml sandbox list` (después del comando, los argumentos son del comando). `/dev/null` (`--config /dev/null`, `ASP_CONFIG=/dev/null`) no lee ningún fichero: así los smokes del repo no tocan el `/etc/asp` del host donde corren. Los paquetes dejan en `/etc/asp` el fichero con lo que necesita todo nodo o plano de control y unos ejemplos comentados; lo vuestro va en un drop-in (`10-site.yaml`), y así el fichero del paquete sigue siendo del paquete.

## Quién gana

**Bandera > variable de entorno > fichero > valor por defecto**, igual en los tres. Dentro de los ficheros, los drop-ins se leen **por orden de nombre** después del base y el último que da un valor, gana; un `null` quita una clave que puso uno anterior. En el CLI, el fichero del usuario gana al del sistema.

Que el entorno gane al fichero tiene una consecuencia: un `Environment=ASP_MTLS=1` en la unit no se puede cambiar desde el YAML. Por eso las unit del repo no llevan ninguno de los ajustes (un test lo comprueba) y solo conservan las variables que no son ajustes (`ASP_ATTEST_KEY` en el node-agent) y los secretos de un `EnvironmentFile`.

## Las claves

La clave es el nombre del ajuste en minúsculas con guiones bajos:

- **node-agent**: el nombre de la bandera (`--egress-proxy-listen` → `egress_proxy_listen`).
- **plano de control y CLI**: la variable sin `ASP_` (`ASP_LISTEN_ADDR` → `listen_addr`, `ASP_TENANT` → `tenant`).
- Un mapa anida: `egress: {proxy_listen: ":8888"}` es `egress_proxy_listen`. Una lista es el valor separado por comas que ya tomaba la variable. Los booleanos valen `true`/`false` y las demás grafías de siempre (`1`, `on`, `no`…).
- Una clave que no es un ajuste **detiene el arranque**, con el fichero que la trae y la más parecida: `"listen_adr" is not a setting (did you mean listen_addr?)`. Sin eso, una errata sería un ajuste que no hace nada y nadie lo vería.
- No van en el fichero las variables que el código lee a mano y no son ajustes declarados: en el node-agent, las de la tabla [«Variables sin opción»](../reference/configuration/node-agent.md#variables-sin-opción) (`ASP_ATTEST_KEY`, `ASP_NODE_API_KEY`, `ASP_EGRESS_ALLOWLIST_JSON`…). Se ponen en la unit (`Environment=`) o en un `EnvironmentFile`.

```yaml
# /etc/asp/agent.yaml.d/10-site.yaml
control_plane_url: https://cp.example.corp:8443
control_plane_ca: /etc/asp/cp-ca.pem
agent_tls_listen: 0.0.0.0:9443
endpoint: https://node1.example.corp:9443
max_sandboxes: 40
egress:
  proxy_listen: ":8888"
```

```yaml
# /etc/asp/server.yaml.d/10-site.yaml
listen_addr: 0.0.0.0:8443
tls_cert: /etc/asp/tls.crt
tls_key: /etc/asp/tls.key
database_url: postgres://asp:…@127.0.0.1:5432/asp?sslmode=disable
sched:
  policy: binpack
node_failover_after: 10m
```

```yaml
# ~/.config/asp/asp.yaml
control_plane_url: https://cp.example.corp:8443
tenant: acme
api_key: …            # chmod 600
```

## Ver lo que vale

Cada componente imprime su configuración efectiva, con el valor de cada ajuste y de dónde viene (bandera, entorno, fichero o valor por defecto), **sin las credenciales** (`<redacted>`):

```bash
sudo asp-node-agent --print-config        # en el nodo
asp-control-plane --print-config
asp config show --effective               # el CLI
asp config show --component server        # los ficheros del plano de control, tal como los ve el CLI
```

```text
# files: /etc/asp/agent.yaml, /etc/asp/agent.yaml.d/10-install.yaml
# order: flag, environment, file, default
control_plane_url: https://cp.example:8443  # file /etc/asp/agent.yaml.d/10-install.yaml
disk_dir: /var/lib/asp/disks  # file /etc/asp/agent.yaml
enroll_token: <redacted>  # file /etc/asp/agent.yaml.d/20-enroll.yaml
heartbeat_interval: 30s  # default
```

`--print-config` no arranca nada: sirve con el servicio parado, y el comando para ver por qué un fichero se rechaza. `asp config show` funciona aunque el fichero del CLI sea el roto.

## Las credenciales

El fichero lleva a veces un secreto (`bootstrap_api_key`, `database_url`, `enroll_token`, `api_key`). Si el fichero lo puede leer cualquier usuario, el componente avisa al arrancar: `…/agent.yaml.d/20-enroll.yaml holds enroll_token and can be read by every user: chmod 600 it`. El token de alta de un nodo tiene un fichero propio (`20-enroll.yaml`) para poder quitarlo cuando el nodo se ha registrado: el instalador lo borra solo.

## Pasar de `Environment=` y banderas a un fichero

1. En el host de hoy, con la unit tal como está, mira lo que se aplica: `sudo asp-node-agent --print-config` (con las mismas banderas del `ExecStart` detrás) o `asp-control-plane --print-config` (con el mismo entorno).
2. Copia los ajustes que no son el valor por defecto a un `10-site.yaml`.
3. Deja la unit en `ExecStart=… --config /etc/asp/agent.yaml` y quita lo que ya está en el fichero. Los dos caminos conviven mientras tanto: el fichero es una capa más, por debajo de la bandera y de la variable.

Los ejemplos del repo: [`packaging/etc/agent.yaml`](../../packaging/etc/agent.yaml), [`packaging/etc/server.yaml`](../../packaging/etc/server.yaml) (lo que instalan los paquetes) y [`scripts/systemd/lab/`](../../scripts/systemd/lab) (el laboratorio de los mantenedores, [descrito aquí](../lab/README.md)). La lista de ajustes de cada componente, generada del código: [plano de control](../reference/configuration/control-plane.md), [node-agent](../reference/configuration/node-agent.md), [`asp`](../reference/configuration/cli.md) y [`asp-server`](../reference/configuration/asp-server.md); las reglas que comparten, en [Configuración](../reference/configuration.md).
