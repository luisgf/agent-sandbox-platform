# Comprobar un nodo o una actualización de extremo a extremo

Los tests y los smokes de CI mueven nodos `--dry-run` (FakeVMM): ejercitan el plano de control, no el datapath. Los fallos del datapath (un `/` con modo 0700, un stop que perdía lo que el guest no había sincronizado, un disco retenido que borraba un self-fence, un nodo que se quedaba con el disco o el TAP de una sandbox ya borrada) solo aparecen arrancando VMs de verdad. `scripts/e2e-kvm.sh` recorre la vida de una sandbox en un host con KVM y falla si algo de eso vuelve.

## Qué hace

1. Arranca una sandbox con un workspace cuyo dueño es el uid 1000.
2. Ejecuta `id -u` (es **1000**, el dueño del workspace) y como root con `--root` (**0**), y lee un fichero del host.
3. Escribe datos que nadie sincroniza (un fichero y 8 MiB aleatorios con su suma) y un fichero en el workspace.
4. `stop`. Comprueba que el disco **se queda limpio** (`dumpe2fs`: `Filesystem state: clean`, y `e2fsck -fn` no encuentra nada), vuelve a ser de root, lo que el guest escribió en el workspace llegó al host, y no quedan TAP ni unit.
5. `resume`. Comprueba que arrancó por segunda vez, que los datos sin sincronizar están (y la suma de los 8 MiB coincide) y que el workspace sigue compartido.
6. `rm`. Comprueba que no queda disco, directorio de la VM, TAP ni unit de la sandbox en el nodo.

## Cómo se ejecuta

En un host con KVM, como root, con un kernel y una imagen del guest ([`images/guest/README.md`](../../images/guest/README.md)) y los binarios (`make build`):

```bash
sudo ASP_E2E_ROOTFS=/var/lib/asp/images/rootfs.img ASP_E2E_KERNEL=/opt/sandbox/vmlinux \
     ASP_E2E_BIN=build scripts/e2e-kvm.sh        # o: sudo make e2e-kvm (con esas variables)
```

Levanta un plano de control y un node-agent **desechables** en ese host (directorio, puertos, id de nodo y red de guests propios; no toca el nodo del propio host y solo para sus unidades) y lo borra todo al terminar. `ASP_E2E_KEEP=1` deja el montaje en pie si falla. `ASP_E2E_AGENT_ARGS` añade flags al agente: con `--stop-grace=1ms` cada stop es una muerte y el script **tiene que fallar** («el sistema de ficheros quedó limpio»), que es como se comprueba que la prueba muerde.

Contra un despliegue ya hecho (después de actualizarlo, desde cualquier máquina con `asp`), solo por la API:

```bash
ASP_E2E_CONTROL_PLANE_URL=https://cp.example ASP_API_KEY=... \
ASP_E2E_WORKSPACE=/srv/asp/workspaces/default/e2e \   # un directorio del nodo, del uid 1000; opcional
     scripts/e2e-kvm.sh
```

Hace lo mismo menos lo que solo ve el host del nodo (el estado del disco, el TAP, las units). Crea la sandbox `e2e-<pid>` y la borra al terminar, también si falla. Los scripts ignoran los ficheros de configuración (`ASP_CONFIG=/dev/null`, [el fichero de configuración](config-file.md)): ni el `/etc/asp` del host donde corren ni el `asp.yaml` de quien los lanza entran en la prueba, y las credenciales para un despliegue van en el entorno.

## En CI

[`.github/workflow-drafts/nightly-kvm.yml`](../../.github/workflow-drafts/nightly-kvm.yml) lo ejecuta cada noche en un runner propio con KVM, con `smoke-vmm-user-kvm.sh` y `smoke-egress-kvm.sh`, y para un PR que un mantenedor etiquete `needs-kvm`. Hace falta un runner (`self-hosted, linux, kvm`) en una máquina para él solo, con `sudo` sin contraseña, y el permiso `workflow` para activar el workflow (`gh auth refresh -s workflow`, luego `git mv` a `.github/workflows/`). No lo ejecuta para PRs de forks.
