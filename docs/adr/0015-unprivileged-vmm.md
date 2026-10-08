# ADR-0015: El VMM corre como un usuario sin privilegios, uno por VM

- **Estado:** Aceptada
- **Fecha:** 2026-10 (#105)
- **Contexto:** [ADR-0014](0014-vms-outlive-the-agent.md) (cada VM en su unit), [ADR-0012](0012-retained-disks.md) (discos retenidos)

## Contexto

`cloud-hypervisor` es el proceso que emula los dispositivos de la VM (virtio-blk, virtio-net, vsock, virtio-fs) con lo que el guest le escribe en las colas. Es el código más expuesto del nodo a un guest hostil, y corría como root, el usuario del agente: un fallo de memoria en cualquiera de esos dispositivos daba root en el host. Firecracker lo resuelve con su *jailer* (chroot, namespaces, cgroups, privilegios fuera, seccomp); KubeVirt y Kata corren el VMM sin root en un pod propio.

#105a puso cada VM en su propia unit systemd (`asp-vm-<id>`) con cgroup y límites, y el VMM ya arrancaba con `--seccomp true`. Seguía siendo root.

## Decisión

1. **Cada VMM corre como un usuario y grupo propios: `--vm-uid-base` + el CID de su VM** (`0x70000000` + CID por defecto, por encima de los rangos de systemd y de los contenedores). Dos VMs vivas a la vez nunca comparten usuario porque no comparten CID (el agente reserva el CID de una VM que adopta). El grupo suplementario es el dueño de `/dev/kvm`, si el dispositivo no es de todos.
2. **El cambio de usuario lo hace `setpriv` dentro de la unit** (`systemd-run … -- setpriv --reuid=N --regid=N --groups=<kvm> --inh-caps=-all --ambient-caps=-all --bounding-set=-all --no-new-privs -- cloud-hypervisor …`): ninguna capability, ni heredada ni recuperable, y ningún binario setuid puede dar privilegios. `User=` de systemd no sirve (rechaza un uid numérico sin entrada en `passwd`) ni `DynamicUser=` (elige el uid al arrancar, y el TAP hay que dárselo antes).
3. **La unit del VMM se cierra** con lo que un VMM que solo necesita ficheros, `/dev/kvm` y un TAP no usa: sin sockets IP (`RestrictAddressFamilies=AF_UNIX`, `IPAddressDeny=any`), sin escribir fuera de su directorio y su disco (`ProtectSystem=strict`, `ReadWritePaths=`), sin cambiar de namespace, personalidad o clase de planificación, `UMask=0077`, sin core dumps y ningún fichero más grande que el disco que tiene (`LimitFSIZE`: un disco raw no crece). Su red es el TAP; sus otros interlocutores son sockets unix.
4. **Lo que el agente le entrega a ese usuario, justo antes de arrancar, y le quita al liberar la VM:**
   - un directorio propio `{--vm-run-dir}/{id}/` (0700, suyo; por defecto `{--ch-socket-dir}-vm`, `/run/asp-vm`) con sus sockets (API, vsock, consola), cortos para no pasar el límite de 107 bytes de un socket unix;
   - el TAP, creado con `ip tuntap add … user <uid>` (el kernel solo deja abrir un TAP persistente a su dueño);
   - los sockets que el agente y `virtiofsd` abren para que el VMM se conecte (`vsock.sock_26501`/`_26502`, `virtiofs.sock`): conectar a un socket unix pide permiso de escritura;
   - su disco (`chown` al arrancar, de vuelta a root al parar, fallar el arranque o morir el VMM). `--disk-dir` y `--vm-run-dir` se quedan en 0711: se pasa por ellos, no se lista ni se escribe, y cada disco es solo del usuario de su VM.
5. **`virtiofsd` sigue siendo root**, con `--sandbox chroot` en el workspace. Tiene que leer y escribir los ficheros del workspace como el dueño que sea cada uno, y un demonio sin privilegios solo puede actuar como él mismo.
6. **`--vm-unprivileged=auto|on|off`**, `auto` por defecto: se activa cuando el host puede y lo comprueba *abriendo como ese usuario* lo que el VMM abrirá (el kernel, `/dev/kvm`, `/dev/net/tun`, sus directorios, su binario); si no, deja los VMM como root diciendo por qué. `on` se niega a arrancar. Necesita `--vm-confine`: la caída de privilegios y las restricciones de la unit son un solo mecanismo.
7. **Registro, adopción y limpieza** ([ADR-0014](0014-vms-outlive-the-agent.md)): el registro de la VM lleva su usuario y su directorio; el agente que adopta la VM vuelve a abrir los aceptores y se los da a ese usuario; la limpieza de arranque borra los directorios de las VMs muertas y devuelve a root los discos que un VMM que murió con el agente parado dejó a su usuario.

## Alternativas

- **Un usuario `asp-vm` compartido.** Sencillo, pero un VMM comprometido abriría el disco y los sockets de todas las demás VMs: la frontera que importa entre tenants.
- **Namespaces de usuario (`PrivateUsers=`).** El VMM sería root solo dentro del namespace, pero `/dev/kvm`, el TAP y los ficheros siguen necesitando dueños fuera de él; no quita nada de lo de arriba y añade un modelo de ids más.
- **Landlock (`--landlock` de CH).** Cerraría el sistema de ficheros desde dentro del proceso, pero en CH v53 exige la configuración de la VM en la línea de comandos y no se combina con el flujo por la API REST que usa el agente. `ProtectSystem=strict` + `ReadWritePaths=` da el mismo efecto sobre ficheros. Se reconsidera al cambiar de versión de CH.
- **Pasar el TAP y el disco como descriptores abiertos** (SCM_RIGHTS, `--net fd=`). Evitaría los `chown`, pero CH v53 solo acepta descriptores para la red por la API, no para el disco, y habría que rehacer el flujo de creación.
- **Un jailer completo (chroot + namespaces de montaje por VM, como Firecracker).** Más aislamiento de la vista del sistema de ficheros, con bastante más código; lo que `ProtectSystem=strict`, `PrivateTmp` y `ProtectHome` dan cubre lo esencial para un usuario sin privilegios.

## Consecuencias

- Un guest que consiga código en el VMM llega a un proceso sin capabilities, sin red IP, con una vista de solo lectura del sistema, dueño de los ficheros de una sola VM. Sigue pudiendo usar KVM, hablar con su `virtiofsd` (root, en chroot: su superficie, el protocolo vhost-user, es la que queda) y consumir lo que su cgroup le deja (`MemoryMax`, `CPUQuota`, `TasksMax`).
- **`virtiofsd` como root es el límite conocido de esta decisión.** Quitárselo exige traducir ids (`--translate-uid`) o un namespace de usuario por VM; se trata aparte.
- Hace falta un rango de ids libre (`--vm-uid-base`), un `setpriv` de util-linux 2.31 o posterior y que el kernel y el binario del VMM los pueda leer cualquier usuario. `auto` lo comprueba y avisa.
- Compatibilidad: las VMs que arrancó un agente anterior siguen como root hasta que paran, se adoptan sin más (su registro no tiene usuario) y no se les quita nada. El formato del registro no cambia de versión: los campos nuevos son opcionales.
- Un VMM sin confinamiento (`--vm-confine=off`, sin systemd, `--ch-api-socket`) sigue como root: es hijo del agente.
- Verificado en el laboratorio ([host de pruebas](../lab/README.md)) con una pila de prueba aparte de la que corre, y en `scripts/smoke-vmm-user-kvm.sh`: dos sandboxes con otro usuario cada una, sin capabilities, `NoNewPrivs`, filtro seccomp, cgroup propio; el usuario de una no abre el disco, los sockets ni el directorio de la otra ni el directorio privado del agente; exec, workspace, red por el TAP y agente SSH funcionan; parar deja el disco a root, reanudar conserva los datos; reiniciar el agente adopta ambas con el mismo VMM; un VMM que muere (con el agente vivo o parado) no deja nada a su usuario.
