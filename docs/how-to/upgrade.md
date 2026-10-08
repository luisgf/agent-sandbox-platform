# Actualizar ASP

Cómo pasar un despliegue de una versión a otra sin perder sandboxes ni sesiones: el orden, qué hace cada paso con lo que está corriendo, cómo comprobar el resultado y cómo volver atrás. Vale para el plano de control y los nodos por separado y para `asp-server` (todo en un host). Cómo se publica una versión: [publicar una versión](release.md).

**Mientras la versión mayor sea 0, una versión menor puede cambiar ajustes y comportamiento**, y el [CHANGELOG](../../CHANGELOG.md) lo dice bajo *Changed* y *Removed*. Lee lo que hay entre tu versión y la nueva antes de empezar.

## Antes de empezar

1. **Haz una copia** de la base de datos y de las claves ([copias y restauración](backup-and-restore.md)). Es lo que te devuelve a donde estabas si una migración o un ajuste sale mal: volver a un binario anterior no deshace una migración.
2. **Mira lo que hay.** `asp version` en el cliente; `asp node list` muestra la versión del agente de cada nodo (`VERSION`) y el digest de su imagen del guest (`GUEST IMAGE`); `sudo asp doctor` en un nodo comprueba KVM, hipervisor, imágenes, disco y nftables. Lo que ya falle antes de actualizar no lo arregla actualizar.
3. **Ajustes con nombre antiguo.** Los nombres que cambian siguen valiendo con un aviso en el log (`ASP_X is deprecated: use ASP_Y`) hasta que se quitan. Tras actualizar, busca el aviso (`journalctl -u asp-control-plane | grep deprecated`) y cámbialo mientras sigue valiendo. [Configuración](../reference/configuration.md#nombres-que-cambiaron).

## El orden

1. **El plano de control.**
2. **Los nodos, de uno en uno.**
3. **`asp` en los clientes.**
4. **La imagen del guest**, solo si la versión nueva trae otra.

Primero el plano de control porque las migraciones de la base de datos solo **añaden** (el plano de control nuevo arranca sobre la base del anterior) y porque un nodo nuevo dice cosas que uno viejo no entiende: un nodo que reinicia con `adopted_sandboxes` solo conserva sus VMs si el plano de control lo entiende (si no, las pasa a `stopped` y las detiene, sin perder los discos), y firma campos de atestación que un plano de control anterior rechaza. **No dejes mezcladas dos versiones más tiempo del que dura la ventana de actualización**: entre versiones menores 0.x no hay garantía de que se entiendan.

## Los paquetes no reinician nada

`apt install ./asp-control-plane_X.Y.Z_linux_amd64.deb` (o `dnf install ./….rpm`, o `install.sh` con `INSTALL_ASP_VERSION=X.Y.Z`) deja los ficheros nuevos y **no reinicia el servicio**: dice `asp-control-plane is running the version it had before: systemctl restart asp-control-plane runs this one.` Tus ajustes (`/etc/asp/*.yaml` y los `*.yaml.d/`) no se pisan. El reinicio es un paso aparte, el que haces en el momento que elijas.

## 1. El plano de control

```bash
sudo apt install ./asp-control-plane_X.Y.Z_linux_amd64.deb      # o el rpm, o install.sh
sudo systemctl restart asp-control-plane
journalctl -u asp-control-plane -n 20 --no-pager     # «using Postgres store … migrations=ok» y «scheduler ready»
curl -fsS https://cp.example.corp:8443/healthz        # con --cacert si tu certificado es de una CA privada
```

Qué pasa con lo que corre:

- **Las sandboxes siguen.** Viven en los nodos; el plano de control solo las anota. Los nodos reintentan solos mientras no contesta y no paran nada por ello.
- **Los `exec` en curso se cortan**: el plano de control es el intermediario, y un comando en streaming se mata en el guest cuando su conexión se cierra. Haz el reinicio cuando nadie espere un comando largo, o avisa.
- **No se da por perdido ningún nodo por haberlo reiniciado.** El monitor cuenta el silencio de cada nodo desde que **arranca** el plano de control, así que un reinicio largo no dispara el fencing ni marca sandboxes como `failed` ([nodos caídos](../ops-multi-node.md#nodos-caídos)). Aun así, mantenlo corto: mientras no contesta no se pueden crear sandboxes, y un nodo que no habla en 90 s deja de recibir sandboxes.
- **Las migraciones se aplican solas** al arrancar, con Postgres y con SQLite (`asp-server`), cada una en una transacción: si una falla, el arranque se detiene con su nombre, esa migración se deshace entera y las anteriores se quedan aplicadas. Con el almacén en memoria no hay nada que migrar y se pierde todo al reiniciar: no es un despliegue.

Comprueba: `curl …/healthz`, `asp node list` (todos los nodos con `LAST SEEN` de hace segundos y `SCHEDULABLE`), y `asp sandbox list` (las mismas sandboxes que antes).

## 2. Cada nodo

Un nodo cada vez, empezando por el menos cargado.

```bash
sudo apt install ./asp-node-agent_X.Y.Z_linux_amd64.deb
sudo systemctl restart asp-node-agent
journalctl -u asp-node-agent -n 30 --no-pager      # «adopted the VMs a previous agent left running» count=N
asp node list                                      # VERSION nueva, SCHEDULABLE
asp node doctor <id>
```

**Con el confinamiento por defecto no hace falta vaciar el nodo.** Cada VM corre en su propio servicio de systemd (`asp-vm-<id>`), que el reinicio del agente no toca; el proceso nuevo las **adopta** al arrancar, antes de registrarse ([ADR-0014](../adr/0014-vms-outlive-the-agent.md)). Durante los segundos que dura el reinicio:

- los `exec` a ese nodo fallan con 502 (los que estaban en curso se cortan; la VM y sus procesos siguen);
- la política de egress se reaplica en el primer sondeo (2 s): hasta entonces el proxy deniega;
- en una sesión con `--local-net`, el plan del túnel se reaplica en ese sondeo;
- se pierde el historial de la consola serie anterior al reinicio.

**Cuándo sí vaciarlo** (`asp node cordon <id>`, esperar a que `asp node list` muestre `0/…` sandboxes o pedir que se paren las sesiones, `systemctl restart asp-node-agent`, `asp node uncordon <id>`):

- si el nodo corre sin confinamiento (`--vm-confine=off`, un host sin systemd, o `--ch-api-socket`) o con `--vm-survive-restart=false`: el reinicio **detiene** sus VMs y sus sandboxes quedan `stopped` con el disco intacto (`asp session resume` las arranca de nuevo);
- si vienes de un build anterior al ADR-0014 (solo existió en `main` antes de la 0.1.0): las VMs que arrancó aquel agente están atadas a su servicio y no tienen registro que adoptar, así que la primera actualización las para.

Para parar de verdad todas las VMs de un nodo: `sudo systemctl stop asp-vms.slice`.

**Reiniciar el servidor entero** (kernel, hardware) sí para todas las VMs: `cordon`, avisa a los usuarios o espera a que paren, reinicia. Al volver, el agente registra el nodo, las sandboxes que corrían pasan a `stopped` con `node_agent_restarted` y su disco, y `asp session resume` las arranca. Las paradas conservan su disco (caducan a los 7 días, `ASP_STOPPED_SANDBOX_TTL`); mientras el nodo está en cordon no se pueden reanudar.

## 3. El cliente

Actualiza `asp` donde se use (`install.sh` con el rol `cli`, o el paquete `asp`). El CLI y la API van juntos en la misma versión: entre menores 0.x no se garantiza que un cliente anterior entienda la API nueva. El CHANGELOG dice si cambió algo de la configuración del cliente (`~/.config/asp/asp.yaml`) o de los punteros de sesión (`~/.cache/asp/sessions/`).

## 4. La imagen del guest

Cuando una versión trae un kernel o una imagen nuevos:

```bash
sudo asp image pull --version X.Y.Z      # /var/lib/asp/images/X.Y.Z, current → X.Y.Z, enlaces en /opt/sandbox
sudo systemctl restart asp-node-agent    # el nodo calcula y declara los digests al registrarse
asp node list                            # GUEST IMAGE: el mismo en todos los nodos
```

- `asp image pull` comprueba lo que baja contra el `SHA256SUMS` de la release y **no instala nada** si algo no coincide; el nodo vuelve a comprobarlo antes de arrancar cada VM (`--guest-verify`).
- **Las sandboxes en marcha siguen con la imagen con que arrancaron**, y la de una sandbox parada conserva el disco que clonó: solo las nuevas usan la nueva imagen.
- Con una lista de imágenes permitidas (`ASP_ATTEST_ALLOWED_IMAGES`), **añade la entrada de la imagen nueva antes** de actualizar los nodos (`node-agent --print-measurement` la imprime) y quita la vieja cuando no quede ninguna sandbox de ella ([atestación remota](security-operations.md#atestación-remota)).
- Dos nodos con digests distintos arrancan imágenes distintas: `asp node list` lo enseña.

## Comprobar el resultado

```bash
asp node list                                        # versiones, LAST SEEN, SCHEDULABLE
asp session start --name upgrade-check && asp session exec --name upgrade-check -- uname -a
asp session stop  --name upgrade-check && asp session resume --name upgrade-check
asp session rm    --name upgrade-check
```

La prueba completa de un nodo (arrancar, exec, workspace, parar, reanudar, borrar): [comprobar un nodo o una actualización de extremo a extremo](e2e-kvm.md). Con Prometheus, `count by (version) (asp_node_agent_info)` sigue una actualización en un panel ([monitorizar](monitoring.md)).

## Volver atrás

- **Un nodo:** instala el paquete anterior (`apt install --allow-downgrades ./asp-node-agent_<anterior>.deb`) y reinicia el agente, como al actualizar. Mira en el CHANGELOG si la versión de la que vuelves cambió algo que el nodo guarda (el registro de las VMs que adopta, un ajuste).
- **El plano de control:** las migraciones solo añaden, así que el binario anterior suele arrancar sobre una base más nueva, pero **eso no se prueba**. Lo que vuelve a un estado conocido es la copia que hiciste antes: instala el paquete anterior, restaura la base ([restaurar](backup-and-restore.md#restaurar-la-base-de-datos)) y reinicia. Restaurar una base **más vieja que lo que hay en los nodos** tiene consecuencias (se paran VMs y se borran discos que la base no conoce): lee esa sección antes.
- **Un ajuste que falla:** el servicio no arranca y dice cuál (`journalctl -u …`); `asp-control-plane --print-config` y `asp-node-agent --print-config` enseñan lo que usa cada uno y de dónde viene cada valor.

## `asp-server` (todo en un host)

`asp-server` depende de los paquetes `asp`, `asp-control-plane` y `asp-node-agent`: actualiza los cuatro juntos (`install.sh` con `INSTALL_ASP_ROLE=standalone` o los paquetes a mano) y reinicia **solo** `asp-server`. Al pararse detiene el nodo y luego el plano de control (hasta 40 s cada uno); las VMs confinadas siguen, y el arranque siguiente las adopta. El orden plano de control → nodo no hace falta aquí: arrancan juntos y de la misma versión. [Un solo host](single-host.md).
