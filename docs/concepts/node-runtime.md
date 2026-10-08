# Cómo ejecuta un nodo las sandboxes

Lo que hace el node-agent con cada sandbox, de la microVM al `exec`: un proceso de Cloud Hypervisor por sandbox, su imagen y su canal vsock, el workspace, qué pasa cuando el agente se reinicia, qué limpia al arrancar y cómo se confina el VMM. Es explicación y referencia; para instalar un nodo, [instalar un nodo](../how-to/install-node.md), y para su red, [red y egress](networking-and-egress.md). La arquitectura de todo el sistema: [architecture.md](../architecture.md).

## Una VM, un proceso de Cloud Hypervisor

Un proceso de Cloud Hypervisor (CH) es **una** VM (el modelo de su API REST). El cliente de CH está en `node-agent/internal/vmm/cloudhypervisor.go`:

- **Modo por defecto (un proceso por sandbox):** `Start(sandbox)` lanza `cloud-hypervisor --api-socket /run/asp/ch-{sandboxID}.sock` (`--ch-socket-dir`, `/run/asp`; `--ch-binary` es el binario), espera a que el socket acepte `vmm.ping` y hace `vm.create` y `vm.boot`. `Stop(id)` hace `vm.delete`, mata el proceso y borra el socket. Las rutas que usa: `GET /api/v1/vmm.ping`, `PUT /api/v1/vm.create`, `vm.boot`, `vm.delete`, `vm.pause`.
- **Modo compartido (depuración):** con `--ch-api-socket` el agente no lanza nada: habla con un CH ya arrancado en ese socket, que lleva **una VM a la vez** (una segunda sandbox queda `failed`). Un CH así no es del agente y no se toca nunca; si no contesta, avisa `CH ping failed`.
- `--dry-run` usa un VMM de mentira (`FakeVMM`) y ningún CH.

| Comportamiento | FakeVMM (`--dry-run`) | CH real |
|---|---|---|
| Sandboxes concurrentes | Sí | Sí: un proceso CH y un socket por sandbox (`--ch-socket-dir`) |
| Socket | — | `/run/asp/ch-{sandboxID}.sock` (o el compartido, con `--ch-api-socket`) |
| `Stop(id)` | Borra por id | `vm.delete` + matar el proceso + borrar el socket de ese id |
| CID de vsock | 3 | **Único** por sandbox (se reparten desde el 3; el socket híbrido es `/run/asp/vsock-{id}.sock`) |

El directorio de sockets (`/run/asp`) es de root y de modo `0700`: los sockets de dentro dan autoridad sobre cada sandbox (ejecutar como root en el guest, los tokens de identidad, el vhost-user de `virtiofsd`). El agente lo crea así y aprieta uno que ya exista y no sea de sistema.

## La imagen del guest, pod-daemon y vsock

La imagen sale de [`images/guest`](../../images/guest/README.md): Debian bookworm-slim, el `pod-daemon` y sus unidades de systemd (el `pod-daemon` escucha en vsock **26500**). El `pod-daemon` corre como **root** dentro del guest, a propósito: tiene que lanzar cada comando como el dueño del workspace, como `sandboxd` o como root si la petición lo pide. Lo que aísla es la VM, no el usuario de ese proceso.

| Modo del `pod-daemon` | Estado |
|---|---|
| `--listen vsock --vsock-port 26500` | El de producción: AF_VSOCK `CID_ANY:26500` en el guest |
| `--listen unix --unix-socket …` | En el host, para dry-run y smokes |
| `--listen tcp --tcp-addr 0.0.0.0:26500` | Alternativa de laboratorio por el TAP (sin vsock) |

El agente reparte un CID único (≥ 3) a cada VM. Kernel del guest: `CONFIG_VIRTIO_VSOCKETS`.

### Cómo llega un `exec` a la VM

Cloud Hypervisor expone un **multiplexor en un socket unix** del host (`vsock.cid` y `vsock.socket` en `vm.create`). El protocolo host→guest es el de Firecracker:

```text
host: connect(/run/asp/vsock-{id}.sock)
host: write "CONNECT 26500\n"
CH  : reenvía a AF_VSOCK, puerto 26500 del guest
guest pod-daemon: accept → HTTP
CH  : ACK "OK <host_port>\n" al host
host: POST /v1/exec sobre el mismo stream
```

```text
Cliente
  → POST /v1/sandboxes/{id}/exec          (plano de control)
  → POST {agent_endpoint}/v1/internal/exec (node-agent: loopback con el token del agente, u otro host por mTLS)
  → Registry[sandbox_id] → HybridVsockDialer (CONNECT 26500)
  → pod-daemon del guest: POST /v1/exec
```

| Entorno | Dialer | Escucha del guest |
|---|---|---|
| CH real | `HybridVsockDialer` (socket unix + `CONNECT`) | `--listen vsock --vsock-port 26500` |
| Dry-run / smoke | `UnixDialer` (`--pod-daemon-sock`) | `--listen unix` en el host |
| Host con `/dev/vsock` | `AFVsockDialer` | el mismo guest vsock |

`--pod-daemon-port` (26500) es el puerto del guest; `--pod-daemon-sock` solo vale para dry-run o como alternativa.

### Del guest al host: identidad y agente SSH (`--host-vsock`)

Cloud Hypervisor (y Firecracker) usan *vsock híbrido*: un multiplexor en un socket unix del host (`--vsock cid=…,socket=/run/asp/vsock-{id}.sock`). Las dos direcciones no son simétricas:

| Dirección | Mecánica | En ASP |
|---|---|---|
| Host → guest | `connect(multiplexor)`, `CONNECT <puerto>\n` → `OK …\n` | `HybridVsockDialer` (el `exec` y el `pod-daemon`) |
| Guest → host | el guest marca AF_VSOCK, CID **2**, puerto; **el VMM** conecta al socket unix del host `{multiplexor}_{puerto}` | un listener unix por sandbox y puerto, abierto al arrancar la VM |

Por eso un `vsock.Listen(26501)` en el host no recibe nada: el VMM nunca entrega esas conexiones a AF_VSOCK, y espera un listener en `/run/asp/vsock-{id}.sock_26501`; sin él, CH responde RST al guest (`connection reset by peer` en el proxy del agente SSH). Al arrancar una sandbox, antes de `Engine.Start`, el reconciler abre:

```text
guest AF_VSOCK connect(cid=2, port=26501) → {vsock}_26501 → bomba del agente SSH del nodo
guest AF_VSOCK connect(cid=2, port=26502) → {vsock}_26502 → identidad: POST /v1/tokens/oidc
```

Son los mismos *handlers* que el camino global, pero ligados a **esa** sandbox: el token de identidad es el suyo y las firmas solo consumen aprobaciones suyas; un `X-ASP-Sandbox-ID` de otra da 403 ([ADR-0003](../adr/0003-identity.md) § 2) y el guest sigue siendo no confiable. Al parar la sandbox se cierran. El guest no cambia: sigue marcando CID 2.

`--ssh-agent-bridge` (un socket unix; el reconciler crea `/run/asp/ssh-agent-{id}.sock` hacia él) y `--identity-listen` (sin vínculo con una sandbox: solo laboratorio, con `--insecure-identity-sandbox-header`) siguen existiendo, igual que AF_VSOCK de verdad y `--host-vsock-dir` (sockets unix de laboratorio). Un dry-run con `FakeVMM` y `PodDaemonUnix` **no** abre listeners híbridos.

La imagen lleva `ssh-agent-vsock.service` (vsock CID 2:26501 → `/run/agent-sandbox/ssh-agent.sock`): al nodo le basta `--host-vsock` (la antigua `--guest-ssh-agent-auto` ya no hace nada). `--host-vsock` necesita `/dev/vsock`, o `--host-vsock-dir`.

**Límites.** Un listener por sandbox y puerto. Hay que abrirlos *antes* de que el guest marque. Si la ruta del multiplexor cambia (restaurar un snapshot), habría que reabrirlos: no está implementado. Comprobarlo en un host exige una imagen con `vsock-ssh-agent-proxy` y un nodo con esto.

**Probarlo.** `cd node-agent && go test ./internal/hostvsock/ -count=1` (`TestHybridAttachSSHAgentFake`: escucha `{path}_26501`, pide las identidades y recibe un `type 12`); en un host con KVM, [`guest-vsock-notes.md`](../../scripts/guest-vsock-notes.md) y su script de demostración. El protocolo, en la [documentación de CH](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/vsock.md) (`socat - UNIX-LISTEN:/tmp/ch.vsock_1234` en el host, `socat - VSOCK-CONNECT:2:1234` en el guest; Firecracker usa la misma convención `uds_path_PORT`).

Detalle del agente SSH: [`guest-vsock-notes.md`](../../scripts/guest-vsock-notes.md), [ADR-0006](../adr/0006-fase-2e-nft-ssh-guest.md). Confirmar cada firma: [operaciones de seguridad](../how-to/security-operations.md#confirmación-del-agente-ssh).

## El workspace del host (virtiofs) y el exec con PTY

`asp session start --workspace /ruta` persiste `workspace_host_path`. Si no está vacío, el node-agent arranca `virtiofsd` (binario Rust, `--virtiofsd-bin` / `ASP_VIRTIOFSD_BIN`) con un socket por sandbox y `vm.create` incluye `fs` tag `workspace`. Sin el binario el sandbox pasa a `failed`. Sin workspace no hay `fs`.

La imagen guest de este corte monta sola: `workspace-virtiofs.service` hace `mkdir -p /workspace` y `mount -t virtiofs workspace /workspace`, y sale 0 si el tag no está (el boot no se para). Hay que **reconstruir** el rootfs (`./scripts/build-guest-rootfs.sh`) para que una imagen ya desplegada lo lleve. Hasta entonces, dentro de la VM:

```sh
mkdir -p /workspace
mount -t virtiofs workspace /workspace
```

Sin ese mount (imagen vieja) el exec ve el disco del guest. FakeVMM no bootea; los tests afirman socket, tag, y que el helper sale 0 si `mount` falla. Detalle: [`ops-asp-session.md`](../ops-asp-session.md), [sesiones](../ops-asp-session.md#virtiofs-y-pty--qué-aterrizó).

El exec con PTY también viaja por el vsock **26500** (`POST /v1/exec?stream=1` y `POST /v1/exec/stdin`). La imagen tiene que llevar el pod-daemon de este corte; si no, no hay `ready` y el node-agent degrada a un JSON final reescrito como un solo burst. El stream no tiene timeout total en el guest: `--exec-timeout-secs` (default 30) solo limita el exec acumulado, y `--stream-idle-timeout-secs` añade, si se pone, un límite por inactividad.

## Reiniciar el agente no para las VMs

**Reiniciar el agente no para sus VMs** ([ADR-0014](../adr/0014-vms-outlive-the-agent.md)). Con confinamiento (`--vm-confine`, por defecto en un host con systemd y root) cada VMM y cada `virtiofsd` corren en un servicio transitorio propio (`asp-vm-<id>`, `asp-vm-<id>-fs`) que **no** está atado al del agente: una actualización, un `systemctl restart` o un fallo del agente no tocan las VMs, y el proceso nuevo las **adopta**. Cada VM que está en marcha deja un registro en `{--ch-socket-dir}/state/<id>.json` (su CID, su TAP y su /30, sus sockets, su disco y su `virtiofsd`; `0600`, en el `tmpfs` de `/run`, así que un reinicio del host lo borra con las VMs). Al arrancar, antes de registrarse:

1. Por cada registro, el VMM comprueba que la VM sigue viva: su servicio está activo y su API (`vm.info`) contesta `Running` o `Paused`.
2. La limpieza de arranque (más abajo) **salva** todo lo de esas VMs (procesos, sockets, TAP, túnel local-net, disco) y borra el resto.
3. El reconciler toma las vivas: las vigila otra vez (un fallo posterior se informa como siempre, [ADR-0012](../adr/0012-retained-disks.md) § 9), recupera su endpoint de exec, los aceptores guest→host (agente SSH, identidad), el prefijo del proxy de egress, su `virtiofsd`, y aparta su CID y su /30 del reparto. El registro de una VM que ya no vive se borra.
4. El agente se registra con `adopted_sandboxes: [...]`: el plano de control **no** pasa esas sandboxes a `stopped` como huérfanas del reinicio. Las que no adoptó (su VM murió mientras el agente estaba parado) sí: `stopped` con `stop_reason=node_agent_restarted` y su disco intacto (`asp session resume`; [ADR-0011](../adr/0011-multi-node.md), [ADR-0012](../adr/0012-retained-disks.md)).

Lo que se pierde en un reinicio: el historial de la consola serie (el agente se reengancha y lee lo que escriba el guest desde ese momento), la política de egress hasta el primer sondeo (2 s: sin ella el proxy deniega) y, en una sesión local-net, el plan se reaplica en ese sondeo. Una sandbox con un `exec` en curso lo pierde (el agente era el intermediario) pero la VM y sus procesos siguen. Con un plano de control anterior a esto (no entiende `adopted_sandboxes`) el reinicio acaba igual que antes: las pasa a `stopped` y el agente, al verlas sin asignar, las para (seguro, y por eso se actualiza primero el plano de control).

**La primera actualización a una versión con esto sí detiene las VMs**: las que arrancó el agente anterior nacieron atadas a su servicio (`BindsTo=`) y no tienen registro que adoptar. Haz `cordon` y drena esa vez; las siguientes ya no hace falta.

`--vm-survive-restart=false` (`ASP_VM_SURVIVE_RESTART=0`) devuelve el comportamiento anterior: el servicio de cada VM nace con `BindsTo=asp-node-agent.service` y systemd lo para con el agente (ninguna VM queda corriendo para nadie); nada se registra ni se adopta. Un agente lanzado a mano (no es un servicio), sin confinamiento o con `--ch-api-socket` tampoco deja VMs que adoptar: sus VMs son hijos suyos y acaban con él.

Dos defensas contra lo que el agente deje de verdad huérfano:

Dos defensas contra lo que el agente deje de verdad huérfano:

**1. La unit** [`scripts/systemd/asp-node-agent.service`](../../scripts/systemd/asp-node-agent.service), con `KillMode=control-group`: al parar o reiniciar el servicio mata a lo que cuelga del agente (también los `systemd-run` que vigilan sus VMs, que no son las VMs). Sin confinamiento, ahí están los `cloud-hypervisor` y `virtiofsd`, y mueren con él. Después, `ExecStopPost=-node-agent --reap-only` borra lo que esté muerto y **salva lo que sigue vivo** y adoptable. Para parar de verdad todas las VMs de un nodo: `sudo systemctl stop asp-vms.slice`.

```bash
systemctl list-units 'asp-vm-*'               # una VM = asp-vm-<id>.service (y -fs si tiene workspace)
systemd-cgls /asp.slice                         # su cgroup, bajo asp-vms.slice
systemctl show asp-vm-<id> -p MemoryMax -p CPUQuotaPerSecUSec -p TasksMax
journalctl -u asp-vm-<id>                       # la salida de cloud-hypervisor, que antes se perdía
```

`MemoryMax` es la memoria de la VM más `--vm-memory-overhead-mib`; `CPUQuota`, sus vCPU más `--vm-cpu-overhead-percent`. Una VM que sobrepase su `MemoryMax` la mata el kernel: la sandbox falla, el resto del host no lo nota. El VMM arranca además con `--seccomp true` explícito (es su valor por defecto; así un cambio de ese defecto no apaga el filtro).

**2. Limpieza al arrancar** (`--reap-leftovers=on`, por defecto). Antes de abrir ningún socket y antes de registrarse, el agente busca lo que dejó un proceso anterior y lo borra. Cubre lo que la unit no evita: un agente lanzado a mano o una unit con `KillMode=process`. **Los discos no entran aquí**: una sandbox parada conserva el suyo ([ADR-0012](../adr/0012-retained-disks.md)) y el reaper no sabría distinguirlo de un resto. Tras el primer sondeo de `/work`, el reconciler borra las copias de `--disk-dir` que no son de ninguna sandbox (ni asignada, ni retenida, ni en borrado).

| Resto | Cómo lo reconoce |
|---|---|
| Procesos `cloud-hypervisor` | argv `--api-socket {--ch-socket-dir}/ch-{id}.sock` o `{--vm-run-dir}/{id}/api.sock`. SIGTERM, 5 s, SIGKILL. **No** los de una VM viva con registro (se adopta, ver arriba) |
| Procesos `virtiofsd` | argv `--socket-path {--ch-socket-dir}/virtiofs-{id}.sock` o `{--vm-run-dir}/{id}/virtiofs.sock` |
| Directorios de VMM sin privilegios | `{--vm-run-dir}/{id}/` (ver «El VMM sin privilegios»), con todo lo que el VMM dejó dentro. Se quedan si su proceso sigue vivo tras SIGKILL |
| Discos de un usuario de VMM | `{--disk-dir}/rootfs-{id}.img` cuyo dueño no es root (el agente murió con la VM en marcha): se devuelven a root. No se borra ninguno |
| Túneles local-net | claves `{id}.key` en `ASP_LOCAL_NET_KEY_DIR` y devices WireGuard `wg-asp-*`. Borra el device, las `ip rule`, la tabla, las reglas FORWARD y las excepciones nft, como al parar la sandbox |
| TAPs | devices TUN/TAP `asp-{8 hex}` |
| Sockets y enlaces | `ch-`, `vsock-` (y sus `_26501`/`_26502`), `virtiofs-` y `ssh-agent-{id}.sock` en `--ch-socket-dir` |
| Locks y pid files | `ch-{id}.sock.lock` (de Cloud Hypervisor) y `virtiofs-{id}.sock.pid` (de virtiofsd) en `--ch-socket-dir`: ninguno de los dos los borra al salir. Se quedan si su socket es `--ch-api-socket` o si su proceso sigue vivo tras SIGKILL. Al parar una sandbox el agente también los borra, una vez ha salido el proceso |

- Solo toca nombres con un id de sandbox del plano de control (UUID en minúsculas) y con el tipo de fichero que crea el agente. Nunca toca el rootfs base, `--ch-api-socket`, el bridge SSH ni el socket de identidad.
- Un fallo no para el resto: sale como `host cleanup incomplete` en el log y el agente arranca igual. Cada resto borrado deja una línea `removing leftover of a previous node-agent`.
- `--reap-leftovers=report` solo lo lista; `off` lo desactiva. Con `--dry-run` solo informa, y no mira TAPs ni túneles del host.
- `--reap-only` hace solo la limpieza y sale (código 0 si todo se borró). Para ver qué queda en un nodo con el servicio parado: `sudo node-agent --reap-only --reap-leftovers=report`, con los mismos `--ch-socket-dir` y `--disk-dir` que el servicio si no son los de por defecto.
- **Un agente por host.** Mientras vive, el agente mantiene un `flock` sobre `{--ch-socket-dir}/node-agent.lock`. Un segundo agente en ese directorio no arranca (`refusing to start … is held by pid N`), y `--reap-only` tampoco corre. Los TAPs y túneles son de todo el host: dos agentes reales en una misma máquina (solo lab) necesitan directorios de sockets distintos y `--reap-leftovers=off`.
- Modo shared (`--ch-api-socket`): ese CH no es del agente y no se toca; su VM sigue ahí tras el reinicio (`vm.delete` a mano).
- **Actualizar el agente ya no detiene sus VMs** (confinadas): el proceso nuevo las adopta. Sin confinamiento, o con `--vm-survive-restart=false`, sí: haz `asp node cordon` y drena antes de `systemctl restart` ([`ops-multi-node.md`](../ops-multi-node.md)).

## El VMM sin privilegios (un usuario por VM)

([ADR-0015](../adr/0015-unprivileged-vmm.md).) El VMM es el proceso más expuesto del nodo a un guest hostil: emula sus discos, su red, su vsock y su virtio-fs con lo que el guest escribe en las colas. Con confinamiento (ver arriba) **corre como un usuario sin privilegios propio**, no como root:

| | |
|---|---|
| Usuario y grupo | `--vm-uid-base` + el CID de la VM (`1879048192` + CID por defecto), grupo del mismo número. Más el grupo que es dueño de `/dev/kvm` (`kvm`), si el dispositivo no es de todos |
| Privilegios | ninguna capability (`CapEff`, `CapBnd`, `CapAmb` a cero) y `no_new_privs`: nada que ejecute puede recuperarlas. Lo hace `setpriv`, dentro de la unit `asp-vm-<id>` |
| Seccomp | el del propio Cloud Hypervisor (`--seccomp true`) |
| La unit no puede | abrir sockets IP (`RestrictAddressFamilies=AF_UNIX`, `IPAddressDeny=any`: su red es el TAP, y habla con el agente y con `virtiofsd` por sockets unix), escribir fuera de su directorio y su disco (`ProtectSystem=strict`, `ReadWritePaths=`), cambiar de namespace, de personalidad o de clase de planificación, escribir un fichero mayor que su disco (`LimitFSIZE`: un disco raw no crece) ni volcar un core. Ve un `/tmp` y un `/var/tmp` propios y vacíos (`PrivateTmp`), salvo que su directorio, su disco o el kernel estén en uno de ellos (ahí `ReadWritePaths` no devuelve el fichero): entonces la unit va sin `PrivateTmp`, y la de `virtiofsd` nunca lo lleva, para que un workspace en `/tmp` exista para él |
| Ficheros suyos | `{--vm-run-dir}/{id}/` (0700): sus sockets (API, vsock y los `_26501`/`_26502` que abre el agente, consola, `virtiofsd`); su disco (`chown` al arrancar, de vuelta a root al parar, fallar el arranque o morir el VMM); el TAP (`ip tuntap add … user <uid>`) |
| Todo lo demás | ni el directorio de sockets del agente (0700, root), ni el directorio ni el disco de otra VM, ni las claves, ni los tokens |

`virtiofsd` **sigue siendo root**, en `--sandbox chroot` sobre el workspace: tiene que leer y escribir los ficheros como el dueño que sea cada uno, y un demonio sin privilegios solo puede actuar como él mismo. Es el límite conocido de este modelo (ADR-0015).

**Comprobarlo en un host KVM:** `make smoke-vmm-user-kvm` (root, `ASP_SMOKE_ROOTFS=` con la imagen del guest) arranca dos sandboxes y comprueba usuario, capabilities, unit, ficheros entregados, que el usuario de una no toca nada de la otra, parar/reanudar, reinicio del agente (adopción), un VMM que muere y borrar. Usa sus propios directorios, puertos y subred, y solo para sus unidades `asp-vm-*`.

```bash
ps -eo user:14,pid,args | grep '[c]loud-hypervisor --api'      # un usuario numérico por VM, no root
sudo grep -E '^(Uid|Cap(Eff|Bnd)|NoNewPrivs|Seccomp):' /proc/$(systemctl show -p MainPID --value asp-vm-<id>)/status
systemctl show asp-vm-<id> -p IPAddressDeny -p RestrictAddressFamilies -p ProtectSystem -p ReadWritePaths
ip -d link show asp-<id8> | grep -o 'user [0-9]*'              # el TAP es de ese usuario
sudo ls -ln /run/asp-vm /run/asp-vm/<id>                       # /run/asp-vm: 0711; la de la VM: de su usuario
```

**Requisitos del host.** `setpriv` (util-linux 2.31 o posterior), un rango de ids libre, y que ese usuario llegue a lo que el VMM abre: el kernel y el binario `cloud-hypervisor` legibles y ejecutables por todos (`o+r`, `o+x` y `o+x` en cada directorio por encima), `/dev/kvm` (su grupo) y `/dev/net/tun`. Al arrancar, el agente crea `--vm-run-dir` y deja `--disk-dir` en 0711 (se pasa por ellos, no se listan ni se escriben) y **lo prueba abriendo cada cosa como ese usuario**. Si algo falla, `auto` deja los VMM como root con un `WARN` que dice qué y cómo arreglarlo; `on` no arranca; `off` lo desactiva (el comportamiento anterior).

**Lo que no hay.** Landlock: `--landlock` de Cloud Hypervisor v53 exige la configuración de la VM en la línea de comandos y no se combina con el flujo por la API REST del agente. Los VMM sin confinamiento (`--vm-confine=off`, sin systemd, `--ch-api-socket`) siguen como root. Las VMs que arrancó un agente anterior siguen como root hasta que paran; el agente nuevo las adopta igual.

| Síntoma al arrancar el agente | Causa | Arreglo |
|---|---|---|
| `microVMs run as root: … a VM's user cannot read the kernel` | el kernel o un directorio por encima no es legible por todos | `chmod o+r` el kernel y `o+x` en sus directorios |
| `… cannot open /dev/kvm` | el dispositivo no es de un grupo que se pueda sumar, o no es 0666 | `ls -l /dev/kvm`: el grupo del dispositivo se suma solo si es el dueño; si es `root:root 0600`, `chmod 0660` y un grupo `kvm` |
| `… cannot run /usr/local/bin/cloud-hypervisor` | el binario o un directorio por encima no es ejecutable por todos | `chmod o+rx` |
| `… setpriv … is not installed` | falta util-linux, o es anterior a 2.31 | instalar/actualizar `util-linux` |
| `cannot run microVMs as unprivileged users` (con `--vm-unprivileged=on`) | cualquiera de las de arriba | la misma causa, ahora fatal |

## Autodefensa: lo que el plano de control ya no asigna

- Cada `GET /v1/nodes/{id}/work` trae `assigned`: las sandboxes colocadas en ese nodo que lo siguen ocupando. El reconciler para cualquier VM local que no esté ahí (fallada por nodo perdido, destruida o de otro nodo), sin reportar estado. Un 409 al reportar `running` también la para.
- Sustituye a los leases por sandbox (`node_lease_until`, migración `004`, y `POST …/renew-lease`), retirados en 2026-10: `renew-lease` responde 410 y la columna ya no se escribe. En estado estable el nodo solo hace el sondeo de `/work` y el heartbeat.
- Ningún nodo reclama sandboxes de otro: un nodo caído lo detecta el monitor del plano de control ([ADR-0011](../adr/0011-multi-node.md), [`ops-multi-node.md`](../ops-multi-node.md)): `offline` a los `ASP_NODE_STALE_AFTER` (90 s); fencing y sandboxes → `failed` (`node_lost`) a los `ASP_NODE_FAILOVER_AFTER` (5 min).
- El destino de fencing de un nodo (`fence_endpoint`, `fence_token`) lo fija un admin en el plano de control (`asp node fence set`, [fencing](../how-to/security-operations.md#fencing-stonith)), nunca el nodo: un agente comprometido podría apuntarlo al BMC de otro servidor.

**Límite split-brain (honesto):** la autodefensa del nodo **no** es STONITH. Un nodo particionado sigue corriendo sus VMs hasta que vuelve y ve que ya no están en su conjunto `assigned`, o hasta el fencing. Mitigación: `ASP_FENCE_PROVIDER` ([fencing](../how-to/security-operations.md#fencing-stonith)).

## Lo que no hay

TPM o SEV de hardware (la atestación es de software); el fencing de producción por BMC validado de punta a punta; y una demostración de que el nftables resiste un bypass con TAP y KVM reales en CI (CI usa `soft` o `--dry-run`: `make smoke-egress-kvm` lo comprueba en un host KVM). El resto de límites: [README](../../README.md#status-and-known-limits).
