# Diagnosticar un nodo: `asp doctor`

Cuando un nodo no arranca, no registra o arranca sandboxes que fallan, el síntoma suele estar tres capas por encima de la causa: un error de `vm.create`, un `502` al hacer `exec`, o un guest que corre sin las reglas de egress porque el nft nunca se aplicó. `asp doctor` mira la causa directamente: cada comprobación lee una cosa del servidor y de la configuración del agente y responde `ok`, `warn`, `fail` o `skip` con lo que encontró y cómo arreglarlo.

## Cómo se ejecuta

| Comando | Dónde | Qué hace |
|---|---|---|
| `asp node doctor <id>` | desde un puesto con rol admin u operador (o una clave de plataforma) | el control plane le pide al node-agent **en marcha** que se compruebe; lo que ve es lo que el agente ve con sus ajustes reales |
| `sudo asp doctor [--json] [-- flags del agente]` | en el nodo, como root | ejecuta `node-agent --doctor` con los ajustes del nodo, **sin arrancar el agente**: es la herramienta para el nodo que no arranca |
| `sudo node-agent --doctor [--doctor-json] [flags]` | en el nodo, como root | lo mismo, sin pasar por `asp` |

`asp doctor` ejecuta las comprobaciones del propio node-agent, que lee `/etc/asp/agent.yaml` (y `agent.yaml.d/`) como cuando lo arranca systemd, y le pasa lo que pongas detrás de `--` (`--config OTRO.yaml`, o cualquier bandera: `--disk-dir`, `--ch-socket-dir`…). Si tu unit carga un `EnvironmentFile`, `--env-file` lo carga también. Con `--json` imprime el informe para automatizar. **El código de salida es 1 si alguna comprobación falla** (los avisos no cuentan); 2 si no encuentra el `node-agent`.

```text
$ asp node doctor node1
node doctor (node1)
  ok    privileges        running as root
  ok    kvm               /dev/kvm opens read-write (-rw-rw----)
  ok    cloud-hypervisor  /usr/local/bin/cloud-hypervisor v53.0
  warn  virtiofsd         "virtiofsd" not found: a sandbox with a workspace cannot start
                          fix: install the Rust virtiofsd (not the QEMU one: sudo apt install virtiofsd on Ubuntu 24.04 and Debian 13) or point --virtiofsd-bin at it
  fail  nft               the table asp_egress is not applied: guests are not forced through the proxy
                          fix: restart the agent; if it started, read its log for the nft error
  ...
12 ok, 1 warn, 1 fail, 0 skipped
```

## Qué comprueba

Una comprobación que no aplica a la configuración del nodo sale como `skip` con el motivo (`--dry-run`, sin `--tap-auto`, sin proxy de egress…).

| Comprobación | `fail` si… | Qué hacer |
|---|---|---|
| `privileges` | no es root (fuera de `--dry-run`) o el sistema no es Linux | ejecuta el agente como root, p. ej. con su unit de systemd |
| `kvm` | `/dev/kvm` no existe o el agente no puede abrirlo en lectura y escritura | activa VT-x/AMD-V en el firmware y carga `kvm_intel`/`kvm_amd`; dentro de una VM, activa la virtualización anidada en el host que la corre; usuario en el grupo `kvm` |
| `cloud-hypervisor` | no está, o `--version` falla. **`warn`** si la versión mayor no es la probada (v53): su API REST y sus opciones cambian entre mayores | instala la v53 ([instalar un nodo](install-node.md#2-cloud-hypervisor)), o apunta `--ch-binary` |
| `virtiofsd` | (solo `warn`) no está en el `PATH` ni en `/usr/libexec/virtiofsd` (donde lo deja el paquete de Ubuntu y Debian): una sandbox con workspace no arranca | `sudo apt install virtiofsd` (Ubuntu 24.04, Debian 13) o el `virtiofsd` en Rust de otra fuente (no el de QEMU), o `--virtiofsd-bin` |
| `guest-kernel`, `guest-rootfs` | el fichero no existe, está vacío, o su digest no es el que dice un `SHA256SUMS` en su directorio. El kernel se hashea siempre; la imagen base (gigas) solo para compararla con un `SHA256SUMS` | `sudo asp image pull --version X` (baja, comprueba e instala una versión) o `scripts/build-guest-image.sh`, y apunta `--guest-kernel` / `--guest-rootfs`. Con `SHA256SUMS` al lado (o enlazado desde `/opt/sandbox`) y `--guest-verify=auto`, un fichero que no coincide **no arranca**: la sandbox falla con el motivo |
| `disk-dir` | no se puede crear un fichero en `--disk-dir`, o queda menos libre que `--disk-min-free-mib` (el nodo se niega entonces a clonar o reanudar). **`warn`** por debajo del doble | libera espacio, borra sandboxes paradas (`asp session rm`), sube el disco o baja el mínimo |
| `systemd` | con `--vm-confine=on`: no hay systemd, `systemd-run` o cgroup v2 con el controlador de memoria. **`warn`** con `auto`: las VMs correrían como hijas del agente, sin cgroup y sin sobrevivir a su reinicio | un host con systemd y cgroup v2, o `--vm-confine=off` a conciencia |
| `tap` | con `--tap-auto`: no hay `ip`, o no se puede crear un TAP de prueba (`aspdoctor0`, que se borra al instante) | root (CAP_NET_ADMIN) y el módulo `tun` |
| `vsock` | nunca falla: sin `/dev/vsock` dice `skip`, porque con Cloud Hypervisor los servicios guest→host (26501, 26502) escuchan en un socket unix por sandbox y no hace falta el módulo | nada |
| `nft` | el nodo debería forzar el egress por su proxy (`--egress-proxy-listen`) y no hay `nft`, o la tabla `asp_egress` no está aplicada, o le falta una cadena o el puerto del proxy. **`warn`** si la tabla está pero este nodo no pide egress: **un resto de otro agente, cuyas reglas siguen vivas y aplican a todos los TAP `asp-*` del host** (soltó las respuestas de un guest de otra subred) | reinicia el agente; para un resto: `nft delete table ip asp_egress` (y `ip6`) |
| `ip-forward` | nunca falla: informa del valor. Los túneles local-net lo necesitan a 1; el redirect de egress no | `sysctl net.ipv4.ip_forward=1` solo si usas local-net |
| `time` | (solo `warn`) el reloj del host no está sincronizado: los guests lo siguen por el dispositivo PTP | un cliente NTP (systemd-timesyncd o chrony) |
| `vmm-user` | con `--vm-unprivileged=on`: un `cloud-hypervisor` sin privilegios no podría correr aquí (kernel ilegible, `/dev/kvm` cerrado, `setpriv` ausente…). **`warn`** con `auto`: los VMM correrían como root | [el VMM sin privilegios](../concepts/node-runtime.md#el-vmm-sin-privilegios-un-usuario-por-vm) |
| `control-plane` | el plano de control no contesta o no acepta la credencial del nodo (certificado, CA, clave de API, nodo desconocido) | `--control-plane-url`, `--control-plane-ca`, `asp node list`, la caducidad del certificado |

## Síntomas frecuentes y qué comprobación los explica

| Síntoma | Mira |
|---|---|
| `vm.create` o `vm.boot` fallan al arrancar una sandbox | `kvm`, `cloud-hypervisor`, `guest-kernel`, `guest-rootfs` |
| La sandbox falla a los 120 s con `guest_not_ready` | `guest-rootfs` (imagen vieja, sin pod-daemon), `tap` |
| `no node can fit … disk` / el nodo no clona ni reanuda | `disk-dir` |
| Un guest llega a Internet sin pasar por el proxy, o no llega a nada | `nft`, `ip-forward` |
| El agente sale al arrancar con `cannot confine microVMs` | `systemd` |
| `workspace set but virtiofsd is not installed` | `virtiofsd` |
| `asp node list` muestra el nodo `offline` o `egress off` | `control-plane`, `nft` |
| Certificados que «de repente» no valen | `time` |

El informe no cambia nada del nodo salvo el TAP de prueba (creado y borrado al instante), y no hace falta que el nodo tenga sandboxes en marcha. Cada comprobación tiene un tiempo máximo de 20 s y una que se cuelga o falla no impide las demás.

## Otros síntomas

Lo que `asp doctor` no mira: el comportamiento de una sandbox o de un nodo en marcha. Primero, mira qué dice el log del agente (`journalctl -u asp-node-agent`) y `asp session status`.

### Arrancar y ejecutar en un nodo

| Síntoma | Causa probable | Qué mirar |
|---|---|---|
| `kvm-ok` FAIL / no `/dev/kvm` | VT-x/AMD-V off o nested no habilitado | BIOS; `lsmod kvm`; permisos grupo `kvm` |
| `CH ping failed` | Solo modo shared: CH no corre o path distinto | `ps aux \| grep cloud-hypervisor`; `ASP_CH_API_SOCKET` vs `--api-socket` |
| `wait for CH API` / spawn fail | Binario ausente, `/run/asp` no writable, KVM | `--ch-binary`; `ls -ld /run/asp`; `/dev/kvm` |
| `vm.create` falla al abrir el TAP | No hay `--tap-auto` (nadie crea el TAP), o el agente no tiene `CAP_NET_ADMIN` / el nombre ya existe (la sandbox queda `failed` con `tap: …`) | `--tap-auto`, root; [la red de una sandbox](../concepts/networking-and-egress.md) |
| `vm.create` falla con el kernel o el rootfs | Las rutas `--guest-kernel` / `--guest-rootfs` (por defecto `/opt/sandbox/*`) están rotas | Los enlaces y la línea `root=/dev/vda`: [instalar un nodo](install-node.md#3-el-kernel-y-la-imagen-del-guest) |
| Segundo sandbox `failed` (shared) | `--ch-api-socket` = un VM | Quita el flag; usa `--ch-socket-dir` (default) |
| Create → `running` sin VMM | `ASP_AUTO_PROVISION=1` | Pon `0` (es el valor por defecto) y usa `--reconcile` |
| Enroll 401 | Token / TLS | `ASP_NODE_BOOTSTRAP_TOKEN`; enroll **no** lleva client cert |
| Register/heartbeat 401 mTLS | Sin client cert | `--enroll` previo; `--mtls` + `cert-dir`; `ASP_CLIENT_CA` en CP |
| `exec` da 502 o `connection refused` | El `agent_endpoint` del nodo, o el `pod-daemon` del guest | El nodo registrado con `http://127.0.0.1:9100` (o `https://` con `--agent-tls-listen`); `--pod-daemon-sock` en dry-run; [cómo llega un `exec` a la VM](../concepts/node-runtime.md#cómo-llega-un-exec-a-la-vm) |
| El `exec` llega al host pero no al guest | El guest no escucha en vsock o el puerto no es el 26500 | El `pod-daemon` del guest con `--listen vsock`; el `CONNECT` y su `OK`: [cómo llega un `exec` a la VM](../concepts/node-runtime.md#cómo-llega-un-exec-a-la-vm) |
| El tráfico del guest sale sin pasar por el proxy | El nodo no fuerza el egress: el redirect de nftables está apagado (`--egress-nft-redirect=false`) o en `soft` sin poder aplicarse | `asp node list` (columna `EGRESS`), `sudo asp doctor` (`nft`); [red y egress](../concepts/networking-and-egress.md) |
| Permission denied cert-dir | `/var/lib/asp/node-certs` no writable | `mkdir` + owner; o `ASP_CERT_DIR` writable |
| `refusing to start … node-agent.lock is held by pid N` | Ya corre un node-agent (o un `--reap-only`) con ese `--ch-socket-dir` | `systemctl status asp-node-agent`; `ps -p N`; no arranques otro agente a mano junto al servicio |
| `reconciler worker panicked` en el log, con `sandbox_id` y la traza | Un bug en el agente al arrancar, parar o borrar esa sandbox. El agente sigue vivo y las demás VMs también: la sandbox queda `failed` (primer arranque), `stopped` con su disco (reanudar o parar) o `deleted`, con `status_detail=panic: …`. Lo que el arranque aún no había registrado (la TAP y el CID que reservaba) lo libera el siguiente arranque del agente | Guarda la traza y abre una issue; `asp session status` muestra el detalle |
| `host cleanup incomplete` al arrancar | La limpieza de arranque del agente no pudo parar o borrar algo (permisos, proceso en estado D) | El error nombra el recurso; `node-agent --reap-only --reap-leftovers=report` lista lo que queda |
| La sandbox queda `failed` con `guest_not_ready` | El guest no respondió al `/healthz` del pod-daemon en `--guest-ready-timeout` (120 s): imagen rota, kernel que no encuentra el disco, o un pod-daemon que no arranca. La VM se paró y su disco nuevo se borró | El log del agente trae lo último de la consola serie (`last output of the guest's console`) y `status_detail` su última línea; en una reanudación la sandbox vuelve a `stopped` con su disco |
| La sandbox queda `stopped` con `stop_reason=vmm_exited` | El proceso de Cloud Hypervisor acabó solo mientras corría: lo mató el OOM killer o `kill`, falló, o el guest se apagó (`poweroff`). El nodo lo detecta en el siguiente sondeo (2 s) y lo informa con la causa en `status_detail` (`vmm_exited: … Finished with result: signal/oom-kill …` o `the guest powered off`) | `asp session status` lo cuenta; el disco está intacto: `asp session resume`. `journalctl -u asp-node-agent` trae `cloud-hypervisor exited on its own` y `the VM ended on its own` con la última línea de la consola; si se repite, busca el OOM en `journalctl -k` y sube `--vm-memory-overhead-mib` |
| `cloud-hypervisor` o TAP `asp-*` huérfanos tras reiniciar el agente | Agente fuera de systemd, unit con `KillMode=process`, o `--reap-leftovers=off` | Usa la unit del paquete ([instalar un nodo](install-node.md#5-el-servicio)); el siguiente arranque los borra |
| Tras reiniciar el agente las VMs siguen corriendo, y el log dice `adopted the VMs a previous agent left running` | Es lo esperado ([ADR-0014](../adr/0014-vms-outlive-the-agent.md)) | Nada. Para pararlas todas: `systemctl stop asp-vms.slice` |
| `a VM of a previous agent could not be adopted` | La VM murió mientras el agente estaba parado, o su API no contesta | El motivo va en el log; la sandbox queda `stopped` (`node_agent_restarted`) con su disco: `asp session resume` |

### Varios servidores

| Síntoma | Causa probable |
|---|---|
| `503 no schedulable nodes registered` | Ningún nodo registrado con `--reconcile`, o el agente aún no se ha registrado. |
| `503 no node can fit … (2 nodes: 1 cordoned, 1 max_sandboxes)` | Los motivos cuentan por qué se descartó cada nodo. Añade nodos, haz `uncordon`, o espera a que terminen sesiones. |
| `409 node X is not registered` / `cannot take sandboxes: stale` | `--node-id` fijado a un nodo desconocido o caído. Quita el pin o revisa ese nodo. |
| `502 … x509: certificate is valid for nodeA, not nodeB` | El `agent_endpoint` de un nodo apunta al agente de otro. Revisa `--endpoint`. |
| `502 … is plain HTTP on a non-loopback host` | Endpoint `http://` hacia otra máquina. Usa `--agent-tls-listen`. |
| `403 client certificate is for node …` | El agente usa un `--node-id` distinto del CN de su certificado. Quita `--node-id` o re-enrola. |
| El agente no arranca: `node certificate cannot serve TLS` | Certificado anterior a ADR-0011. Re-enrola con `--enroll` o `rotate-cert`. |
| `sandbox was lost with its node` | El nodo llevaba más de `ASP_NODE_FAILOVER_AFTER` sin señales (o fue revocado). Abre una sesión nueva. |
| `stop_reason=node_agent_restarted` | El node-agent se reinició y su VM no estaba viva para adoptarla (murió mientras el agente estaba parado, o el nodo corre sin confinamiento). La sandbox queda `stopped` con su disco: `asp session resume`. |
| El agente no arranca: `refusing to start … node-agent.lock is held by pid N` | Ya corre otro node-agent con ese `--ch-socket-dir` (p. ej. el servicio, si lo lanzaste a mano). |
| `self-fencing` en el log del agente | El plano de control ya no le asigna esa sandbox (failover o destroy); el agente paró la VM. Esperado tras una partición. |

### Sesiones (`asp session`)

| Síntoma | Causa probable |
|---|---|
| `no active session` | No hubo `start` de ese `--name`, otro `HOME`, u otro `--session-dir`. |
| `active session …` en start | Ya hay JSON para ese nombre. `resume --name` si está parada, `rm --name` para borrarla, o `start --force --name`. |
| `invalid session name` | El nombre tiene `/`, espacios o `..`. |
| exec HTTP 404 | Alguien borró el sandbox y el JSON sigue. `rm` (limpia en 404) o `start --force`. |
| `idle timeout` / `idle_reaped=true` | El reaper paró el sandbox y **conservó su disco**. El JSON local sigue. `asp session resume --name …`. |
| `sandbox is stopped; its disk is kept` | Alguien paró la sesión. `asp session resume --name …`. |
| `no capacity on the node that holds the disk` al reanudar (503) | El nodo del disco está lleno. La sandbox sigue parada: reintenta, o libera hueco en ese nodo. No se puede mover el disco a otro. |
| `disk_lost` al reanudar | Falta el disco en el nodo (alguien lo borró, o el plano de control olvidó la sandbox y el GC del nodo lo recogió). No hay vuelta atrás: `asp session rm` y `start`. |
| `no capacity: …` en start (503) | Ningún nodo tiene hueco; el mensaje cuenta por qué se descartó cada uno (`max_sandboxes`, `insufficient_memory`, `cordoned`, `stale`…). Reintenta, o que un admin añada nodos o haga `uncordon` (`asp node list`). |
| `node pin rejected: …` en start (409) | `--node-id` apunta a un nodo desconocido, caído, revocado o en cordon. Quita el pin o revisa ese nodo. |
| `lost_with_node=true` / `sandbox was lost with its node` | Estaba `failed` porque su nodo dejó de dar señales (`node_lost`): el disco vivía en ese servidor, `asp session start --force --name …`. |
| `was stopped when the node agent restarted` / `…when its node stopped responding` | Es una sandbox `stopped` (`node_agent_restarted`, o `node_lost` si se estaba parando): su VM murió (con el agente, o mientras estaba parado: un reinicio normal del agente **no** la para, la adopta) pero **el disco se conserva** en el nodo. `asp session resume` (cuando el nodo vuelva, si es `node_lost`). `start --force` la borraría: pide `--yes` a propósito. |
| exec 401 | Token caducado o `ASP_REQUIRE_TOKEN` sin secretos. `asp auth status`. |
| stdout vacío y exit ≠ 0 | El guest falló sin stdout; el código es el `exit_code`. El error del CLI (red, 500, stream sin evento `exit`) es exit **1**, no el código del guest. |
| `exec stream: missing exit event` | El proxy cortó el NDJSON. No hubo `exit_code`. El fin del stream no refresca la actividad (su tiempo abierto sí la refrescó). |
| `state file kept` | `DELETE` falló (no 404). El JSON sigue para reintentar `rm`. |
| El guest no ve `/workspace` | Imagen nueva: `systemctl status workspace-virtiofs` en el guest. Si el tag no estaba, la unidad sale 0 y no hay mount (sandbox sin workspace, o `virtiofsd` no arrancó). Imagen vieja, sin esa unidad: `mkdir -p /workspace && mount -t virtiofs workspace /workspace`. Si el start falló con `virtiofsd`, el binario no está en el nodo (`--virtiofsd-bin` / `ASP_VIRTIOFSD_BIN`). |
| Salida de golpe al final | `--buffered`, `--json`, o un pod-daemon que no habla `?stream=1` (el node-agent emite un burst). |
