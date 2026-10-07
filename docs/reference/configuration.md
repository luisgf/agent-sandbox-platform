# Configuración

Cada programa de ASP se configura con opciones de la línea de órdenes, variables de entorno `ASP_*` y un fichero YAML. Las tablas con **todos** los ajustes de cada uno se generan del código y un test las compara con él (`make docs` las reescribe), así que no se quedan atrás cuando se añade o se cambia un ajuste:

| Programa | Qué es | Todos sus ajustes | Fichero |
|---|---|---|---|
| `asp-control-plane` | El plano de control: la API, el planificador, las claves y la CA | [plano de control](configuration/control-plane.md) | `/etc/asp/server.yaml` |
| `asp-node-agent` | El agente de cada nodo: las microVMs, la red, el egress | [node-agent](configuration/node-agent.md) | `/etc/asp/agent.yaml` |
| `asp` | La línea de órdenes | [ajustes](configuration/cli.md) y [órdenes](cli.md) | `/etc/asp/asp.yaml` y `~/.config/asp/asp.yaml` |
| `asp-server` | Plano de control y nodo en un solo host ([guía](../how-to/single-host.md)) | [asp-server](configuration/asp-server.md) | `/etc/asp/standalone.yaml` |

## Las reglas, iguales en todos

- **Prioridad:** opción > variable de entorno > fichero > valor por defecto. En `asp-server` no hay capa de entorno: los programas que arranca leen el suyo.
- **Nombres:** la variable es `ASP_` más el nombre de la opción en mayúsculas y con guiones bajos (`--control-plane-url` → `ASP_CONTROL_PLANE_URL`); las pocas excepciones ya estaban en uso y las lista un test. La clave del fichero es la variable sin `ASP_`, en minúsculas (el node-agent y `asp-server` usan el nombre de la opción con guiones bajos). Un mapa anida: `sched: {policy: binpack}` es `sched_policy`.
- **Booleanos:** `1`, `true`, `yes`, `on` y `0`, `false`, `no`, `off`, igual en la opción, la variable y el fichero. Cualquier otra cosa es un error que nombra el ajuste, no un ajuste que no hace nada.
- **Ficheros:** el del paquete en `/etc/asp` y los `<fichero>.d/*.yaml` que lo acompañan (el último que da un valor gana; `null` lo quita). `--config FILE` o `ASP_CONFIG` nombran otro, que tiene que existir; `/dev/null` no lee ninguno. Una clave que no es un ajuste detiene el arranque y dice cuál se parece. [El fichero de configuración](../how-to/config-file.md) lo explica con ejemplos.
- **Ver lo que vale:** `asp-control-plane --print-config`, `asp-node-agent --print-config` y `asp config show --effective` imprimen cada ajuste con su valor y de dónde viene (opción, entorno, fichero o valor por defecto). Las credenciales (🔒 en las tablas) salen como `<redacted>`, y un fichero con una credencial que cualquier usuario puede leer hace que el componente avise al arrancar.

## Nombres que cambiaron

Los nombres antiguos siguen valiendo, con un aviso en el log, hasta que se quiten; cada tabla los lista junto al ajuste. Uno merece cuidado: **`ASP_IDP_REQUIRED` significa otra cosa en cada programa.** En el plano de control exige un token del IdP en las rutas de usuario; en un nodo era lo que ahora es `ASP_MULTI_USER`, y en `asp` lo que ahora es `ASP_REQUIRE_TOKEN`. Los tres nombres nuevos son distintos entre sí.

## Cómo se mantienen las tablas

Las tablas salen de:

| Página | Fuente | Test que la guarda |
|---|---|---|
| plano de control | `settingsTable` en `control-plane/cmd/api/settingstable.go`: una fila por variable, con su grupo, valor por defecto y descripción | `TestTheTableIsEveryVariableTheSourceReads` (la fuente no lee ninguna variable que no esté), `TestReferenceIsUpToDate` |
| node-agent | las opciones que declara `declareSettings` en `node-agent/cmd/node-agent/main.go` (su texto de ayuda es la descripción), el grupo de cada una en `referenceGroups` y las variables sin opción en `envOnlySettings` | `TestEveryFlagIsInOneGroup`, `TestEveryVariableTheSourceReadsIsDocumented`, `TestReferenceIsUpToDate` |
| `asp` (ajustes) | `settingsTable` en `cli/cmd/asp/settingstable.go` | `TestSettingsReferenceIsUpToDate` |
| `asp` (órdenes) | `asp <orden> -h` de cada orden del uso de `asp` | `TestCommandsReferenceIsUpToDate` |
| `asp-server` | sus opciones (`newFlags`) | `TestReferenceIsUpToDate` |

Al añadir un ajuste: declararlo (con una descripción: es lo que lee el operador), ponerlo en su grupo si el componente los tiene y ejecutar `make docs`. Si se olvida, `make test` y CI fallan con el nombre de la página que hay que regenerar.
