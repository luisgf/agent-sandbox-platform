# ADR-0013: Kubernetes, `agent-sandbox`, Kata y E2B frente al orquestador propio, medidos

- **Estado:** Propuesta
- **Fecha:** 2026-10-12
- **Revisa:** [ADR-0004](0004-k8s-scope.md)
- **Relacionados:** [ADR-0016](0016-single-host.md) (un solo host), [ADR-0017](0017-fast-start.md) (arranque rápido), [ADR-0011](0011-multi-node.md) (varios nodos), [ASP frente a Kubernetes](../concepts/asp-vs-kubernetes.md), [#143](https://github.com/luisgf/agent-sandbox-platform/issues/143)

## Contexto

[ADR-0004](0004-k8s-scope.md) descartó en 2026-09 que una sandbox fuera un Pod, con cuatro argumentos. La revisión de diseño de 2026-10-07 encontró que no se sostienen frente a Kata y `RuntimeClass`:

- **No hay dos planificadores.** Con Kata, el único es kube-scheduler, y el runtime arranca la VM.
- **El conflicto entre CNI y TAP lo crea ASP.** Kata pone el TAP en el netns del pod.
- **«El estado vive en Postgres y no en etcd» es circular.**
- **`runtimeClassName` hace explícita la frontera del VMM.**

ADR-0004 tampoco examinó el proyecto `agent-sandbox` de SIG Apps, ni Virtink, ni E2B. Y buena parte del código de ASP (el planificador, el ciclo de vida de los nodos, la PKI, el fencing, la detección de nodos caídos) reimplementa lo que Kubernetes ya da.

Este ADR pone números debajo de esa discusión. Se ejecutó el ciclo de vida de una sandbox (el mismo e2e de ASP) en tres pilas y se midió lo que pedía [#143](https://github.com/luisgf/agent-sandbox-platform/issues/143): pasos para instalar, arranque en frío, reanudación, latencia de `exec`, qué pasa al reiniciar y actualizar el demonio del nodo, cómo se mapean RBAC y tenants, y qué falta de identidad y de egress.

## Cómo se midió

**Dónde.** En el host de pruebas (ncc1701d, un Xeon E-2236 con 6 núcleos y 12 hilos, 32 GB), y sin tocar el host: cada pila vivió en una VM Ubuntu 24.04 propia, la cloud image del proveedor arrancada con Cloud Hypervisor y **KVM anidado**, dentro de un network namespace propio. Cada VM tenía 4 vCPU, 8 GiB (12 GiB la de E2B, que lo pide) y salida solo por un proxy con lista blanca. Ni el firewall del host, ni Docker, ni el nodo de producción se tocaron, y todo se borró al terminar.

**Qué versiones.**
- k3s v1.36.5+k3s1, actualizado después a v1.37.1+k3s1.
- Kata Containers 4.2.0 con kata-deploy (Helm), runtime `kata-clh-runtime-rs` (Cloud Hypervisor y el runtime en Rust, el predeterminado de Kata 4) y, para comparar, `kata-clh` (el runtime en Go).
- `agent-sandbox` v1.0.6 (CRDs `agents.x-k8s.io/v1beta1`).
- E2B Embed con Docker Compose desde `main` del 2026-10-11 (orquestador v0.16.202610051633, Firecracker v1.14).
- ASP en el commit `bf9a8bf` (0.0.0-SNAPSHOT), instalado con el instalador de un solo host.

**Qué sandbox.** 1 vCPU y 512 MiB en ASP y en Kata (la VM de Kata se dimensionó con sus anotaciones). La plantilla `base` de E2B tiene 2 vCPU y 512 MiB. Cada medida va desde la petición hasta que **un comando corre dentro**. Los tiempos son medianas de 5 (arranque) o de 20 (`exec`) repeticiones.

**Límites de la medida.** Con virtualización anidada los arranques son más lentos que en metal (ASP: 3,1 s en metal en el mismo host frente a 5,1 s anidado). Las tres pilas se midieron en las mismas condiciones, pero los números absolutos no son los de producción. Son medidas de un día en una máquina, no un benchmark. Dos medidas de la primera pasada se repitieron porque estaban mal: un selector de etiquetas vacío dejaba contar el pod de otra sandbox. Las cifras de abajo son las repetidas.

## Resultados

### Instalar

| | ASP | k3s + Kata + `agent-sandbox` | E2B Embed |
|---|---|---|---|
| Órdenes | **1** (`curl … \| sh`, rol `standalone`) | 4 o más: k3s, helm, kata-deploy (con 5 ajustes) y el manifiesto de `agent-sandbox`; después, cada plantilla lleva su `runtimeClassName` | 3: Docker, dos ficheros y `docker compose up -d --wait` |
| Tiempo hasta la primera sandbox | **44 s** | k3s en 54 s, `agent-sandbox` en 18 s; kata-deploy, el tiempo de bajar su imagen (1,8 GB) | unos 4 min (la primera vez baja 3,2 GB de imágenes y construye la plantilla `base` en 90 s) |
| Qué ocupa | paquetes de 18 MB, imagen del guest de 40 MB y kernel de 46 MB | binario de k3s, imagen de kata-deploy de 1,8 GB, imágenes del sistema | 13 imágenes (3,2 GB) y 2,1 GB de datos |
| Tropiezos | ninguno; el doctor solo avisó del reloj sin NTP de la VM | con los valores por defecto, kata-deploy entra en bucle: su shim por defecto (`qemu-runtime-rs`) no está entre los instalados | la plantilla se construye dentro de una microVM que necesita salir a Internet (`apt`); el laboratorio necesitó un DNS y un reenviador; recomienda 12 GiB y reserva 4 GiB en *hugepages* |

### El ciclo de vida de una sandbox

| | ASP | `agent-sandbox` (Kata, runtime-rs) | E2B Embed |
|---|---|---|---|
| **Arranque en frío** | 5,1 s (3,1 s en metal) | 2,6 s (un pod con Kata, 2,6 s; con runc, 1,35 s) | **0,09 s** (restaura un snapshot) |
| **Desde un pool precalentado** | no hay | 0,65 s (`SandboxClaim` contra un `SandboxWarmPool`) | — (ya restaura) |
| **`exec` de un comando** | 22 ms (31 ms en metal) | 122 ms (`kubectl exec`) | **11 ms** |
| **Parar o suspender** | 3,1 s, con apagado limpio del sistema de ficheros | 1,5 s (`operatingMode: Suspended` borra el pod) | 0,2 s (`pause`) |
| **Reanudar** | 5 a 7 s: arranca de nuevo sobre su disco | 2,4 s: un pod nuevo con el mismo volumen | **0,14 s**: restaura memoria y disco |
| **Qué sobrevive a la pausa** | **todo el disco** (8 MiB escritos sin `sync` incluidos) | solo el volumen; lo escrito en el rootfs se pierde | **disco y memoria**: un proceso que corría sigue corriendo |
| **Workspace de un directorio del host, con su dueño** | sí (virtiofs, uid 1000) | sí (`hostPath`, con `runAsUser: 1000` declarado) | no: sus volúmenes los gestiona E2B |
| **Borrar** | inmediato; el disco se borra | inmediato; el PVC **queda** y hay que borrarlo aparte | inmediato |

### Reiniciar y actualizar el nodo

| | ASP | k3s + Kata | E2B Embed |
|---|---|---|---|
| **Reiniciar el demonio del nodo** con una sandbox corriendo | `exec` vuelve a los 0,44 s; la VM sigue (el uptime no se reinicia) y el proceso nuevo la adopta | el API vuelve a los 6,7 s y el `exec` a los 8,2 s; la VM sigue | **la sandbox se pierde** (`docker compose restart orchestrator`) |
| **Actualizar** | no se repitió aquí; el ensayo en máquinas limpias ([#209](https://github.com/luisgf/agent-sandbox-platform/pull/209)) mostró que el paquete no reinicia nada y que el reinicio adopta la VM | de v1.36.5 a v1.37.1 con el script de instalación: 14 s, y **la VM sigue** | no se midió |

### Identidad, egress y tenants

| | ASP | k3s + Kata + `agent-sandbox` | E2B Embed |
|---|---|---|---|
| **Identidad del workload** | token OIDC acuñado por el servidor (`sub` de la sandbox, `tenant_id`, `sandbox_id`; `user_sub` y `act` cuando la crea una persona del IdP) y **agente SSH del host por vsock**; los dos respondieron dentro del guest | token de ServiceAccount proyectado (`system:serviceaccount:<ns>:<sa>`, audiencia el API server): no dice qué persona es la dueña; no hay vsock ni agente del host | «workload identity»: tokens con audiencia por sandbox, derivados de equipo, sandbox y plantilla (documentado, no medido) |
| **Egress** | allowlist **por nombre** y por tenant, denegada por defecto; host no permitido → 403 y una línea de auditoría con el `sandbox_id`; otros puertos, descartados | NetworkPolicy, **solo IP y puerto**: con Kata funciona (puerto permitido sí, otro puerto no, fuera no), sin nombres ni auditoría | listas por sandbox de CIDR, IPs o **nombres** (por SNI y Host), abiertas por defecto; no se pudo verificar aquí (el DNS del laboratorio resolvía todo a una dirección) |
| **Tenants** | una API key de tenant recibe 403 en otro tenant | un namespace por tenant y un Role: `alice` puede reclamar sandboxes en el suyo y no en otro | equipos con sus API keys (no medido) |

### Densidad y coste

Ocho sandboxes de 512 MiB pedidas a la vez:

| | ASP | k3s + Kata | E2B Embed |
|---|---|---|---|
| Todas corriendo | 17,8 s | 9,4 s | **0,6 s** |
| Memoria por sandbox | **126 MiB** (118 MiB son el proceso de Cloud Hypervisor) | 158 MiB (Cloud Hypervisor 140, shim 25, virtiofsd 8) | unos 170 MiB (168 MiB de *hugepages* tocadas, 3 MiB del proceso) |
| Plano de control en reposo | **unos 47 MiB** (plano de control 23, node-agent 15, supervisor 9) | **unos 1 GB** (k3s 536 MiB, containerd 176 MiB, más CoreDNS, metrics-server, el provisionador de volúmenes y el controlador de `agent-sandbox`) | unos 0,8 GB (ClickHouse sola son 574 MiB), más **4 GiB reservados en *hugepages*** (6 GiB para correr ocho a la vez) |
| Cuántas admite | 12 por la memoria declarada (512 + 64 MiB cada una, sin sobresuscribir); la decimotercera recibe un 503 que dice por qué | 8; después nada, ni un pod runc de 100m (ver abajo) | 7 con la reserva por defecto, ampliable |

**Una observación sobre k3s y Kata.** Con 8 sandboxes corriendo, la caché de kube-scheduler apuntaba 4225m de CPU pedidos sobre 4000m asignables, cuando los pods declaraban 2225m. La diferencia, 2000m, es justo el overhead de la `RuntimeClass` de Kata (250m y 130 MiB) contado **dos veces** por cada pod en marcha (volcado de la caché con `SIGUSR2`). Pasó con k3s v1.36.5 y con v1.37.1, y Kata 4.2.0. Parece un fallo y no una propiedad de Kubernetes. No se investigó más allá ni se ha reportado, pero en la práctica limita la densidad hasta que se corrija.

### Virtink

Se leyó, no se ejecutó. La última versión es la v0.17.0, de diciembre de 2024. Su README declara Kubernetes ≤ 1.25 y API inestable. No es un candidato.

## Lo que dicen los números

1. **En un solo host, lo pequeño gana, y ASP es lo más pequeño.** ASP es una orden, 44 s y unos 50 MiB de plano de control. k3s con Kata son 4 o más órdenes, una imagen de 1,8 GB y 1 GB de memoria antes de la primera sandbox. E2B son 3,2 GB de imágenes, 4 GiB reservados y salida a Internet para construir plantillas. Para «mi servidor, mi equipo», Kubernetes es demasiado, como ya decía [ASP frente a Kubernetes](../concepts/asp-vs-kubernetes.md), y ahora con cifras.
2. **Donde ASP pierde es en el arranque y en la pausa, y por mucho.** Arrancar le cuesta de 3 a 5 s, frente a 2,6 s de Kata y 0,09 s de E2B. Su reanudación es un arranque nuevo, mientras que E2B reanuda en 0,14 s **con la memoria**. Es el hueco que más cambia la experiencia de un agente que abre y cierra sesiones, y es justo lo que [ADR-0017](0017-fast-start.md) dejó para después (los overlays de [#204](https://github.com/luisgf/agent-sandbox-platform/issues/204), el sondeo de [#205](https://github.com/luisgf/agent-sandbox-platform/issues/205), el pool de [#81](https://github.com/luisgf/agent-sandbox-platform/issues/81) y las instantáneas de [#144](https://github.com/luisgf/agent-sandbox-platform/issues/144)). Cloud Hypervisor sabe hacer snapshot y restore, así que E2B demuestra que se puede.
3. **Lo que ASP tiene y los demás no** son las credenciales fuera del guest (el agente SSH por vsock y el OIDC con la persona dueña), el egress por nombre denegado por defecto y auditado por sandbox, y el workspace de un directorio del host con su dueño. Kubernetes no trae nada de eso de serie: su identidad es un ServiceAccount y su egress es L3/L4. E2B se acerca en egress (nombres, aunque abierto por defecto) y en identidad de workload, pero no comparte directorios del host.
4. **Lo que Kubernetes hace mejor ya funciona con Kata.** Actualizar el demonio del nodo sin matar las VMs (verificado), RBAC por namespace (verificado), un pool precalentado (0,65 s) y una API estándar. Para varios nodos, varios tenants, un IdP y actualizaciones sin cortes, ASP reimplementa con años de retraso lo que ahí ya está, y el coste de mantenerlo es permanente.
5. **E2B Embed es la alternativa real en el terreno de ASP**, no Kubernetes: un host, Apache-2.0, microVMs y arranques de decenas de milisegundos. Lo que pesa en contra para el público de ASP es su huella (memoria reservada, imágenes, ClickHouse), que sus plantillas necesitan salir a Internet al construirse, que no comparte directorios del host y que un reinicio del orquestador se lleva las sandboxes.

## Decisión (propuesta)

**(A) ASP sigue con su plano de control, para un público explícito, y congela la orquestación.**

- **El público de ASP es «mi servidor o el de mi equipo, de uno a tres hosts»**: autoalojado, sin cluster, con las credenciales y el egress resueltos. El caso corporativo de muchos nodos, muchos tenants y actualizaciones sin cortes queda **fuera del objetivo**, y la documentación lo dirige a Kubernetes con `agent-sandbox` y Kata.
- **La orquestación se congela.** No crecen el planificador, el fencing, la PKI ni la detección de nodos caídos. Lo que ya hay (ADR-0011) se mantiene y se arregla, pero no se amplía. Una función nueva de varios nodos necesita un ADR que justifique por qué no es Kubernetes.
- **La prioridad pasa al arranque y a la reanudación**, que es donde los números dejan peor a ASP: los overlays qcow2 (#204), el sondeo (#205) y, después, las instantáneas de Cloud Hypervisor para reanudar con memoria (#144) o un pool (#81), medidas contra los 0,09 s y 0,14 s de E2B y los 0,65 s del pool de `agent-sandbox`.
- **Lo que distingue a ASP se mantiene portable**: la identidad por vsock y el egress por nombre se podrían ofrecer como un DaemonSet y un controlador sobre `agent-sandbox` el día que haga falta (el camino B). No se hace ahora.

**ADR-0004 queda enmendado.** Su conclusión, que en ASP una sandbox no es un Pod, se mantiene para este público. Pero sus razones originales se sustituyen por las de este ADR: la huella, la identidad y el egress, no los dos planificadores ni el conflicto de CNI.

## Alternativas consideradas

| Alternativa | A favor | En contra | Decisión |
|---|---|---|---|
| **(A)** Plano de control propio, un solo host primero, orquestación congelada | la huella más pequeña medida (1 orden, 44 s, unos 50 MiB); identidad y egress propios; directorios del host | arranque y reanudación lentos (hay que arreglarlos); lo de varios nodos queda como está | **Propuesta** |
| **(B)** Pasar a Kubernetes + `agent-sandbox` + Kata/CH y entregar lo de ASP como complementos (un DaemonSet de identidad y egress, un controlador) | actualizaciones sin cortes, RBAC, pools y API estándar ya hechos y verificados; ASP dejaría de mantener su orquestación | 1 GB y 4 o más piezas antes de la primera sandbox: demasiado para el público de un host; reescribir identidad y egress como complementos; el fallo de densidad observado | rechazada **para este público**; es la recomendación para el corporativo |
| **(C)** Una API fina de ASP sobre los CRDs de `agent-sandbox` | conserva la CLI y la sesión de ASP | hereda la huella de B y además mantiene una capa propia; no resuelve identidad ni egress por sí sola | rechazada |
| **(D)** Construir sobre E2B Embed (o recomendarlo) | arranque y reanudación de decenas de milisegundos, con memoria; un host; Apache-2.0 | 4 a 6 GiB reservados, 3,2 GB de imágenes, salida a Internet para las plantillas, sin directorios del host, el reinicio del orquestador pierde las sandboxes; otra base de código grande que seguir | rechazada como base; es la referencia de arranque a batir y la recomendación para quien necesite miles de sandboxes cortas |

## Consecuencias

### Positivas

- El proyecto deja de intentar servir a dos públicos y dice a cuál sirve.
- Se libera el esfuerzo que iba a la orquestación de varios nodos, y va al arranque y la reanudación.
- La comparación con Kubernetes y con E2B deja de ser una opinión: hay cifras y un procedimiento para repetirlas.

### Negativas

- Quien necesite muchos nodos con ASP tendrá lo que hay hoy y nada más, hasta que un ADR nuevo diga otra cosa.
- El hueco con E2B en arranque y reanudación es de un orden de magnitud. Cerrarlo exige instantáneas de VM, que no son poca cosa (ADR-0017).
- Si un día el público cambia a corporativo, el camino es B, y el código de orquestación de ASP se habrá mantenido para nada.

## Límites honestos

- Todo se midió con virtualización anidada en un solo host y en un día. Los números relativos valen, pero los absolutos no son los de producción.
- No se midió el caso de tres nodos (#143 lo dejaba abierto), ni la actualización de E2B, ni su egress por nombre (el DNS del laboratorio lo impedía), ni su identidad de workload.
- El doble cómputo del overhead de Kata en kube-scheduler no se investigó ni se reportó: la densidad de k3s con Kata podría ser mejor con un arreglo.
- La memoria por sandbox depende de lo que haga el guest. Aquí los guests estaban ociosos.
- `agent-sandbox` se probó con Kata. Con gVisor el arranque y la memoria serían otros, y el aislamiento también.

## Cómo repetirlo

El laboratorio fueron tres VMs con la red y el proxy de [`scripts/e2e-install-kvm.sh`](../../scripts/e2e-install-kvm.sh) y un script por pila, que no están en el repositorio.
- **ASP:** el instalador con `INSTALL_ASP_ROLE=standalone`, y `asp session start|exec|stop|resume|rm`.
- **k3s + Kata + `agent-sandbox`:**
  - k3s con `--disable traefik --disable servicelb`.
  - `helm install kata-deploy <chart 4.2.0> --set k8sDistribution=k3s --set shims.disableAll=true --set shims.clh-runtime-rs.enabled=true --set defaultShim.amd64=clh-runtime-rs --set-json 'snapshotter.setup=[]'`.
  - `kubectl apply -f sandbox-with-extensions.yaml` (v1.0.6), y `Sandbox`, `SandboxWarmPool` y `SandboxClaim` con `runtimeClassName: kata-clh-runtime-rs` y las anotaciones `io.katacontainers.config.hypervisor.default_memory` y `default_vcpus`.
- **E2B Embed:** el `compose.yaml` y el `.env` de su guía, y su SDK de Python 2.46.0.

Cada medida cronometra desde la petición hasta que `true` corre dentro.

## Enmiendas
