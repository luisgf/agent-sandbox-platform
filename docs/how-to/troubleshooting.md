# Diagnosticar un nodo: `asp doctor`

Cuando un nodo no arranca, no registra o arranca sandboxes que fallan, el síntoma suele estar tres capas por encima de la causa: un error de `vm.create`, un `502` al hacer `exec`, o un guest que corre sin las reglas de egress porque el nft nunca se aplicó. `asp doctor` mira la causa directamente: cada comprobación lee una cosa del servidor y de la configuración del agente y responde `ok`, `warn`, `fail` o `skip` con lo que encontró y cómo arreglarlo.

## Cómo se ejecuta

| Comando | Dónde | Qué hace |
|---|---|---|
| `asp node doctor <id>` | desde un puesto con rol admin u operador (o una clave de plataforma) | el control plane le pide al node-agent **en marcha** que se compruebe; lo que ve es lo que el agente ve con sus ajustes reales |
| `sudo asp doctor [--json] [-- flags del agente]` | en el nodo, como root | ejecuta `node-agent --doctor` con los ajustes del nodo, **sin arrancar el agente**: es la herramienta para el nodo que no arranca |
| `sudo node-agent --doctor [--doctor-json] [flags]` | en el nodo, como root | lo mismo, sin pasar por `asp` |

`asp doctor` carga las variables de `/etc/asp/node-agent.env` (`--env-file` para otro fichero), como hace la unit de systemd, y pasa al agente lo que pongas detrás de `--` (las banderas que la unit lleva en `ExecStart`: `--disk-dir`, `--ch-socket-dir`, `--egress-proxy-listen`…). Con `--json` imprime el informe para automatizar. **El código de salida es 1 si alguna comprobación falla** (los avisos no cuentan); 2 si no encuentra el `node-agent`.

```text
$ asp node doctor node1
node doctor (node1)
  ok    privileges        running as root
  ok    kvm               /dev/kvm opens read-write (-rw-rw----)
  ok    cloud-hypervisor  /usr/local/bin/cloud-hypervisor v53.0
  warn  virtiofsd         "virtiofsd" not found: a sandbox with a workspace cannot start
                          fix: install the Rust virtiofsd (not the QEMU one) or point --virtiofsd-bin at it
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
| `cloud-hypervisor` | no está, o `--version` falla. **`warn`** si la versión mayor no es la probada (v53): su API REST y sus opciones cambian entre mayores | instala la v53 ([bare-metal §2.1](../bare-metal-ch.md)), o apunta `--ch-binary` |
| `virtiofsd` | (solo `warn`) no está: una sandbox con workspace no arranca | instala el `virtiofsd` en Rust (no el de QEMU), o `--virtiofsd-bin` |
| `guest-kernel`, `guest-rootfs` | el fichero no existe, está vacío, o su digest no es el que dice un `SHA256SUMS` en su directorio. El kernel se hashea siempre; la imagen base (gigas) solo para compararla con un `SHA256SUMS` | `sudo asp image pull --version X` (baja, comprueba e instala una versión) o `scripts/build-guest-image.sh`, y apunta `--guest-kernel` / `--guest-rootfs`. Con `SHA256SUMS` al lado (o enlazado desde `/opt/sandbox`) y `--guest-verify=auto`, un fichero que no coincide **no arranca**: la sandbox falla con el motivo |
| `disk-dir` | no se puede crear un fichero en `--disk-dir`, o queda menos libre que `--disk-min-free-mib` (el nodo se niega entonces a clonar o reanudar). **`warn`** por debajo del doble | libera espacio, borra sandboxes paradas (`asp session rm`), sube el disco o baja el mínimo |
| `systemd` | con `--vm-confine=on`: no hay systemd, `systemd-run` o cgroup v2 con el controlador de memoria. **`warn`** con `auto`: las VMs correrían como hijas del agente, sin cgroup y sin sobrevivir a su reinicio | un host con systemd y cgroup v2, o `--vm-confine=off` a conciencia |
| `tap` | con `--tap-auto`: no hay `ip`, o no se puede crear un TAP de prueba (`aspdoctor0`, que se borra al instante) | root (CAP_NET_ADMIN) y el módulo `tun` |
| `vsock` | con `--host-vsock` sin `--host-vsock-dir`: no existe `/dev/vsock` (los servicios guest→host escuchan en AF_VSOCK 26501/26502) | `modprobe vsock vhost_vsock`, o `--host-vsock-dir` para sockets unix (labs) |
| `nft` | el nodo debería forzar el egress por su proxy (`--egress-proxy-listen`) y no hay `nft`, o la tabla `asp_egress` no está aplicada, o le falta una cadena o el puerto del proxy. **`warn`** si la tabla está pero este nodo no pide egress: **un resto de otro agente, cuyas reglas siguen vivas y aplican a todos los TAP `asp-*` del host** (soltó las respuestas de un guest de otra subred) | reinicia el agente; para un resto: `nft delete table ip asp_egress` (y `ip6`) |
| `ip-forward` | nunca falla: informa del valor. Los túneles local-net lo necesitan a 1; el redirect de egress no | `sysctl net.ipv4.ip_forward=1` solo si usas local-net |
| `time` | (solo `warn`) el reloj del host no está sincronizado: los guests lo siguen por el dispositivo PTP | un cliente NTP (systemd-timesyncd o chrony) |
| `vmm-user` | con `--vm-unprivileged=on`: un `cloud-hypervisor` sin privilegios no podría correr aquí (kernel ilegible, `/dev/kvm` cerrado, `setpriv` ausente…). **`warn`** con `auto`: los VMM correrían como root | [bare-metal §5.7](../bare-metal-ch.md#57-el-vmm-sin-privilegios-un-usuario-por-vm) |
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
