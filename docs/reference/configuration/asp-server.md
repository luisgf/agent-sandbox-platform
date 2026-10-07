<!-- Generado por `make docs` a partir de las opciones de asp-server (cli/cmd/asp-server/main.go). No lo edites a mano: un test falla si no coincide con el código. -->
# Configuración de `asp-server`

`asp-server` ([un solo host](../../how-to/single-host.md)) se configura con opciones y con un fichero: **opción > fichero > valor por defecto**. El fichero es `/etc/asp/standalone.yaml` (más los `standalone.yaml.d/*.yaml` que lo acompañan), o el que nombre `--config FILE`; la clave de un ajuste es su opción con guiones bajos (`data_dir`) y una clave que no es un ajuste es un error ([el fichero de configuración](../../how-to/config-file.md)). El entorno no es una capa: los programas que arranca, `asp-control-plane` y `asp-node-agent`, leen el suyo, y una variable pensada para uno no debe cambiar a este.

Los ajustes de esos programas: [plano de control](control-plane.md) y [node-agent](node-agent.md); el cliente, [`asp`](cli.md).

| Opción | Clave del fichero | Por defecto | Qué hace |
|---|---|---|---|
| `--cli-config` | `cli_config` | `/etc/asp/asp.yaml` | Where to leave the configuration of the asp command of this host if it has none ("-" leaves none). |
| `--config` | — | — | A settings file (default `/etc/asp/standalone.yaml`, read when it exists); the keys are these flags with underscores. |
| `--control-plane` | `control_plane` | — | The asp-control-plane program (default: beside this one, on the PATH, or where the packages put it). |
| `--data-dir` | `data_dir` | `/var/lib/asp` | Where everything lives: the database, the keys, the certificate, the disks. |
| `--group` | `group` | `asp` | The group that may read the administration key (if there is one). |
| `--listen` | `listen` | `127.0.0.1:8443` | Where the control plane listens (host:port). The loopback by default: `0.0.0.0:8443` lets other hosts join. |
| `--no-agent` | `no_agent` | — | Run the control plane alone, with no node on this host. |
| `--node-agent` | `node_agent` | — | The asp-node-agent program (same places). |
| `--node-id` | `node_id` | — | The id of the node on this host (default: the host name and -node; a name in the control plane's certificate is refused). |
| `--profile` | `profile` | `default` | The profile: default, or lab, a node without VMs (`--dry-run`) for a host with no KVM and for the smokes. |
| `--tls-san` | `tls_san` | — | More names or addresses its TLS certificate must be valid for (comma separated, or repeated). |
| `--user` | `user` | `asp-control-plane` | The account the control plane runs as when this runs as root. |
| `--version` | — | — | Print the version and exit. |
