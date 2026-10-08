# ADR-0017: Arrancar y reanudar rápido: primero el desperdicio medido, después el pool y las instantáneas

- **Estado:** Propuesta
- **Fecha:** 2026-10-08 (#144, #81)
- **Implementación:** en parte. Hecho: el helper del guest deja de esperar a un tag virtiofs que no existe (primer cambio de la decisión). Pendiente: los discos como overlay qcow2 ([#204](https://github.com/luisgf/agent-sandbox-platform/issues/204)) y un sondeo de trabajo más corto ([#205](https://github.com/luisgf/agent-sandbox-platform/issues/205)).
- **Extiende:** [ADR-0012](0012-retained-disks.md) (qué es parar y reanudar)
- **Relacionados:** [ADR-0014](0014-vms-outlive-the-agent.md) (las VMs sobreviven al agente), [ADR-0015](0015-unprivileged-vmm.md) (el VMM sin capacidades), [#81](https://github.com/luisgf/agent-sandbox-platform/issues/81) (pool precalentado), [#144](https://github.com/luisgf/agent-sandbox-platform/issues/144) (esta decisión)

## Contexto

Empezar una sesión tarda unos 6 s hasta que la sandbox está `running`, y reanudar una parada, algo menos. Los agentes abren sesiones a menudo, así que esa latencia la paga cada bucle de herramientas. Hay dos ideas grandes sobre la mesa:

- **Un pool de VMs precalentadas** (#81, como `SandboxWarmPool` de `agent-sandbox`): el nodo mantiene unas pocas VMs ya arrancadas y una sesión nueva adopta una. Ayuda a **crear**.
- **Pausar e instantánea de Cloud Hypervisor** (#144, como E2B): parar es pausar y guardar la memoria y el estado de los dispositivos, y reanudar es restaurar. Ayuda a **reanudar** y, con una instantánea base, a **crear**.

Antes de elegir se midió qué hay realmente en esos 6 s y qué hacen las dos ideas, en el host de pruebas ([laboratorio](../lab/README.md)): Cloud Hypervisor v53.0, kernel de host 7.0, un guest con 1 vCPU, el kernel y la imagen que arranca un nodo. El script que produce las cifras de esta página, para repetirlas: [`scripts/bench-vm-start.py`](../../scripts/bench-vm-start.py).

### De qué se compone un arranque

Los eventos de 22 arranques reales del 6 y 7 de octubre (`sandbox.created`, `sandbox.claimed` y el paso a `running`):

| Tramo | Medido |
|---|---|
| Del `create` a que el nodo la reclama (espera al sondeo `/work`, cada 2 s) | 0,15 a 1,95 s; mediana 0,8 s |
| De reclamarla a `running`, con una sola arrancando | 3,4 a 5,3 s |
| Lo mismo con 4 a 8 arrancando a la vez | 12,5 a 33 s |
| Total, con una sola arrancando | 3,6 a 7,2 s |

Y el tramo grande, de que el nodo lanza Cloud Hypervisor a que el pod-daemon contesta por vsock, desglosado con un VM desechable:

| Montaje del disco | Preparar el disco | Arrancar | Junto |
|---|---|---|---|
| a. Una copia dispersa nueva de la imagen, arrancada de inmediato (lo que hace un nodo hoy) | 0,08 s | 4,77 s | **4,85 s** |
| b. La copia, vaciada a disco antes de arrancar | 2,01 s | 3,01 s | 5,02 s |
| c. Un overlay qcow2 sobre la imagen, sin copia | 0,07 s | 3,19 s | 3,26 s |
| d. Un overlay sobre una imagen cuyo helper no espera a un tag ausente | 0,08 s | 1,38 s | **1,46 s** |
| e. Como c, con un workspace virtiofs | 0,10 s | 1,38 s | 1,48 s |
| f. Como d, con un workspace | 0,09 s | 1,47 s | 1,56 s |

(Medianas de 4 arranques.) Lo que dicen:

- **Unos 1,7 s son la escritura en disco de la copia recién hecha.** El primer arranque de una copia sin vaciar espera a que el host la escriba (la fila b lo confirma: vaciarla antes solo mueve ese tiempo a otro sitio y suma 5,0 s). Con un overlay no hay nada que copiar ni que vaciar (c).
- **Unos 1,8 s son el helper del guest esperando a un tag virtiofs que no existe.** `workspace-virtiofs.service` va antes del pod-daemon y reintenta el `mount` 8 veces, con 0,25 s de pausa, cuando no hay workspace. Las sandboxes **con** workspace arrancaban antes que las que no tenían (3,0 s frente a 4,8 s en el banco); en producción es lo que separa los 3,4 a 3,5 s de las que tienen workspace (6 arranques) de los 5,0 a 5,3 s de las que no (8). Sin el reintento, el guest contesta a los 1,4 s (d).
- Queda un arranque de **1,4 a 1,5 s**, con o sin workspace. Un overlay ocupa 2,4 MiB tras arrancar; la copia que sustituye, unos 310 MB.

### Qué hace una instantánea

| Guest | Instantánea | Fichero de memoria | Restaurar por copia, en la caché de páginas | por copia, fuera de ella | a demanda (`userfaultfd`), fuera de ella |
|---|---|---|---|---|---|
| 512 MiB | 0,16 s | 512 MiB | 0,14 s | 2,5 s | no probado en frío |
| 2 GiB | 0,6 s | 2048 MiB | 0,3 s | 10,3 s | 2,6 s |
| 4 GiB | 3,9 s | 4096 MiB | 2,7 s | 20,3 s | 3,3 s |

- El fichero de memoria ocupa **toda la RAM del guest en disco**, aunque esté casi vacía (no es disperso), por cada sandbox parada, durante los 7 días de retención.
- Restaurar copia esa memoria entera a la RAM del proceso (518 MiB de residente tras restaurar un guest de 512 MiB, contra 174 MiB de uno corriendo). Con la memoria fuera de la caché de páginas, restaurar tarda lo que tarda leerla: 2,5 a 20 s. **Solo gana al arranque en frío optimizado (1,5 s) en un guest pequeño con el fichero en caché.**
- El modo a demanda (`memory_restore_mode=OnDemand`) evita copiar, pero pide **`CAP_SYS_PTRACE`** para crear el `userfaultfd` (con `vm.unprivileged_userfaultfd=0`, el valor por defecto): con un VMM sin capacidades ([ADR-0015](0015-unprivileged-vmm.md)) falla con `Operation not permitted`. Y aun así tarda 2,6 a 3,3 s en responder, porque el guest toca muchas páginas al despertar.
- Lo que sí conserva: los procesos que corrían (un `sleep` seguía allí), los ficheros de `/tmp`, el `boot_id`, la sesión de red. El reloj del guest **no sabe que estuvo fuera**: va 21 s atrasado tras 20 s de pausa y hay que ponerlo en hora.
- **virtiofs funciona** en lo probado: una instantánea con un workspace y descriptores abiertos sobre él, restaurada con un `virtiofsd` nuevo, siguió escribiendo y leyendo. Es un caso sencillo, no una prueba para un árbol con mucha actividad.
- Una instantánea queda atada al Cloud Hypervisor que la hizo y a la CPU del host (`cpus.profile: Host` en su `config.json`): no vale tras actualizar el VMM ni en otro nodo con otra CPU.
- Restaurar una instantánea base en varias VMs deja **estados idénticos**: mismo `boot_id`, misma disposición de memoria, y Cloud Hypervisor v53 no tiene un identificador de generación de VM con que el guest se entere de que es un clon.

### Cuánto cuesta el pool

Una VM ociosa ocupa 174 MiB de memoria del host con un guest de 512 MiB, 196 MiB con uno de 2 GiB y 314 MiB con uno de 4 GiB, más un overlay de unos 2 MiB. Un pool de 4 son 0,7 a 1,2 GiB de memoria reservada que no sirve a nadie hasta que llega una sesión. Y la sesión que lo adopta paga igual la espera al sondeo (0,8 s de mediana): un pool no baja de ahí mientras el nodo se entere de las sandboxes sondeando.

## Decisión

**No se construye ahora el pool (#81) ni se adoptan las instantáneas para crear o reanudar (#144).** Se baja primero el desperdicio medido, que es el 70 % del arranque, con cambios pequeños y comprobables, y se vuelve a medir:

1. **El helper del guest no espera a un tag que no existe** (hecho). Mira si hay un dispositivo virtio-fs (id 26 en `/sys/bus/virtio/devices`, que existe desde que el kernel enumera el bus) y, si no lo hay, sale sin reintentar. −1,8 s en toda sandbox sin workspace; con workspace no cambia (1,47 s antes, 1,50 s después, y el workspace está montado cuando el pod-daemon contesta).
2. **El disco de una sandbox es un overlay qcow2 sobre la imagen base** de su versión, en vez de una copia ([#204](https://github.com/luisgf/agent-sandbox-platform/issues/204)). −1,5 s, 310 MB menos por sandbox y sin la escritura que hacía esperar al primer arranque (y que multiplicaba el tiempo con varios arranques a la vez). Hay que decidir y probar: la imagen base tiene que ser inmutable (la ruta con versión de `asp image pull`, no el enlace `current`) y no se puede borrar mientras haya overlays que la usen, el borrado de un disco retenido sigue siendo un `unlink` (ADR-0012), y Cloud Hypervisor necesita `backing_files=on` solo para estos discos.
3. **El nodo se entera antes de que hay trabajo** ([#205](https://github.com/luisgf/agent-sandbox-platform/issues/205)). Un sondeo más corto (0,5 s en vez de 2 s: de 0,8 s de espera media a unos 0,25 s) o una espera larga (`GET /v1/nodes/{id}/work?wait=…`), que además quita la carga de un sondeo constante.

Con las tres, una sesión nueva tarda unos 2,1 s de punta a punta (2,7 s si el sondeo sigue en 2 s; hoy, 3,6 a 7,2 s), una reanudación algo menos, y **un pool solo ahorraría el arranque que queda (1,4 s) a cambio de memoria ociosa y de las invariantes de seguridad de abajo**. Se reevalúa con esas cifras.

**Las instantáneas no son una optimización de latencia para ASP**: restaurar un guest de 2 GiB o más no gana al arranque en frío optimizado, el fichero ocupa toda la RAM por sandbox parada, el modo a demanda choca con el VMM sin capacidades y la instantánea muere con el Cloud Hypervisor. Si algún día se quiere **conservar los procesos y la memoria de una sesión al parar**, es una función con su propio ADR (opt-in por sesión, con el coste en disco a la vista), no una forma de arrancar rápido.

### Cuándo volver sobre esto

- **El pool**, si tras las tres medidas el arranque sigue siendo el cuello de botella de un uso real (por ejemplo, agentes que abren cientos de sesiones por hora) y se acepta reservar memoria: entonces con las condiciones del #81 (una VM, una sandbox, nunca reciclada; nadie ejecuta código en una VM sin reclamar; las sandboxes con workspace o `--local-net` siguen por el camino frío hasta que el cliente de Cloud Hypervisor sepa hacer `vm.add-fs`, que la API ya tiene).
- **La instantánea al parar**, si hace falta que un `exec` largo o un servidor de desarrollo sobrevivan a una parada. Condiciones: opt-in, el VMM con `CAP_SYS_PTRACE` solo si se usa `OnDemand`, poner el reloj en hora al restaurar, invalidar las instantáneas al actualizar Cloud Hypervisor, y un presupuesto de disco por tenant.
- **Una instantánea base para crear** (como E2B), solo con una respuesta a los clones idénticos (reseed del generador de números aleatorios al reclamar, hasta que Cloud Hypervisor tenga un identificador de generación de VM) y a la red: el IP del guest va en la línea de comandos del kernel y habría que reconfigurarlo.

## Alternativas consideradas

- **El pool primero (#81).** Habría ahorrado 4 s de los 6, pero 3,5 s eran desperdicio que se arregla sin tener VMs ociosas, y el pool no puede bajar de la espera al sondeo. Además toca todo lo que hoy se deriva del id de la sandbox (sockets, TAP, disco, atestación, la política de egress).
- **Instantánea al parar y restaurar al reanudar.** 0,14 s en el mejor caso (512 MiB, en caché) y 2,5 a 20 s con 2 a 4 GiB fuera de caché; un fichero de la RAM del guest por sandbox parada; atado al VMM y a la CPU.
- **Una instantánea base por clase de sandbox.** 0,14 s de restauración y un overlay, pero clones con el mismo estado y una reconfiguración de red por clon.
- **No hacer nada.** Seis segundos por sesión son aceptables para sesiones de horas, pero 3,5 s de ellos eran un bug, no un coste de diseño.

## Consecuencias

- **Positivas.** El grueso de la mejora llega con cambios locales, medibles y reversibles. No hay capacidad ociosa que dimensionar ni invariantes de aislamiento nuevas. El script de medida queda para repetir las cifras con cada Cloud Hypervisor y cada imagen.
- **Negativas.** El suelo del arranque en frío sigue siendo 1,4 s de guest más lo que tarde el nodo en enterarse. Los overlays acoplan cada sandbox a una versión inmutable de la imagen base.

## Límites honestos

- Las cifras son de **un host** (12 cores, discos md en RAID1, ext4, kernel 7.0) y de un guest de 1 vCPU con la imagen de producción; otro disco o más vCPUs cambian los tiempos, no el reparto. Las medianas son de 4 arranques por montaje.
- El tiempo de arranque **con varias sandboxes a la vez** (12 a 33 s en producción con 4 a 8) no se ha medido aquí. Los overlays deberían mejorarlo, porque quitan la escritura de la copia, pero hay que comprobarlo.
- La prueba de virtiofs es un caso sencillo: un descriptor abierto para escribir y un `tail -f`.
- No se midió un pool: su coste es el de una VM ociosa más lo que hace el nodo al reclamar, y lo segundo se estima (unos 0,4 s que hoy se pagan tras arrancar: aplicar la política de egress, firmar la atestación, informar de `running`).
